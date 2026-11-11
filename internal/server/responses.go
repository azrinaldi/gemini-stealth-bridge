package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/toolbridge"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

// NormalizeResponsesInput converts an OpenAI Responses API body (`instructions`,
// `input`, `tools`, `tool_choice`) into canonical Chat Completions messages and tools.
func NormalizeResponsesInput(body map[string]any) (messages []map[string]any, tools any, toolChoice any, err error) {
	if body == nil {
		return nil, nil, nil, transport.NewUpstreamError("request body must be a JSON object", 400, "invalid_request_error", nil, nil)
	}

	if instr, ok := body["instructions"].(string); ok && strings.TrimSpace(instr) != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": instr,
		})
	}

	// Support callers that send `messages` to /v1/responses as well as `input`.
	inputRaw, hasInput := body["input"]
	if !hasInput || inputRaw == nil {
		inputRaw = body["messages"]
	}

	if singleMap, ok := inputRaw.(map[string]any); ok && singleMap != nil {
		inputRaw = []any{singleMap}
	}

	switch inp := inputRaw.(type) {
	case string:
		if strings.TrimSpace(inp) != "" {
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": inp,
			})
		}
	case []any:
		var (
			pendingAssistant map[string]any
			pendingToolCalls []any
			pendingReasoning []string
			callIDToName     = make(map[string]string)
		)
		recordToolCallNames := func(tcs []any) {
			for _, tcAny := range tcs {
				tcMap, ok := tcAny.(map[string]any)
				if !ok {
					continue
				}
				cid, _ := tcMap["id"].(string)
				fnMap, _ := tcMap["function"].(map[string]any)
				fnName, _ := fnMap["name"].(string)
				if cid != "" && fnName != "" {
					callIDToName[cid] = fnName
				}
			}
		}
		flushPendingAssistant := func() {
			if pendingAssistant == nil && len(pendingToolCalls) == 0 {
				return
			}
			var msg map[string]any
			if pendingAssistant != nil {
				msg = pendingAssistant
			} else {
				msg = map[string]any{"role": "assistant", "content": nil}
			}
			if len(pendingToolCalls) > 0 {
				tcCopy := make([]any, len(pendingToolCalls))
				copy(tcCopy, pendingToolCalls)
				msg["tool_calls"] = tcCopy
				recordToolCallNames(tcCopy)
			}
			if len(pendingReasoning) > 0 {
				existing, _ := msg["reasoning_content"].(string)
				if existing != "" {
					msg["reasoning_content"] = existing + "\n" + strings.Join(pendingReasoning, "\n")
				} else {
					msg["reasoning_content"] = strings.Join(pendingReasoning, "\n")
				}
			}
			messages = append(messages, msg)
			pendingAssistant = nil
			pendingToolCalls = nil
			pendingReasoning = nil
		}

		for i, itemAny := range inp {
			switch item := itemAny.(type) {
			case string:
				flushPendingAssistant()
				if strings.TrimSpace(item) != "" {
					messages = append(messages, map[string]any{
						"role":    "user",
						"content": item,
					})
				}
			case map[string]any:
				if item == nil {
					continue
				}
				itemType, _ := item["type"].(string)
				role, _ := item["role"].(string)

				switch itemType {
				case "function_call", "custom_tool_call":
					callID, _ := item["call_id"].(string)
					if callID == "" {
						callID, _ = item["id"].(string)
					}
					if callID == "" {
						callID = fmt.Sprintf("call_%d", i+1)
					}
					name, _ := item["name"].(string)
					if callID != "" && name != "" {
						callIDToName[callID] = name
					}
					argsStr := "{}"
					if itemType == "custom_tool_call" {
						inStr, _ := item["input"].(string)
						b, _ := json.Marshal(map[string]any{"input": inStr})
						argsStr = string(b)
					} else if a, ok := item["arguments"].(string); ok && a != "" {
						argsStr = a
					} else if aObj, ok := item["arguments"].(map[string]any); ok {
						b, _ := json.Marshal(aObj)
						argsStr = string(b)
					}
					pendingToolCalls = append(pendingToolCalls, map[string]any{
						"id":   callID,
						"type": "function",
						"function": map[string]any{
							"name":      name,
							"arguments": argsStr,
						},
					})
					continue

				case "function_call_output", "custom_tool_call_output":
					flushPendingAssistant()
					callID, _ := item["call_id"].(string)
					if callID == "" {
						callID, _ = item["id"].(string)
					}
					name, _ := item["name"].(string)
					if name == "" && callID != "" {
						name = callIDToName[callID]
					}
					var outStr string
					switch o := item["output"].(type) {
					case string:
						outStr = o
					case nil:
						outStr = ""
					default:
						b, _ := json.Marshal(o)
						outStr = string(b)
					}
					messages = append(messages, map[string]any{
						"role":         "tool",
						"tool_call_id": callID,
						"name":         name,
						"content":      outStr,
					})
					continue
				case "reasoning":
					if pendingAssistant != nil {
						flushPendingAssistant()
					}
					if sumArr, ok := item["summary"].([]any); ok {
						for _, sAny := range sumArr {
							if sMap, ok := sAny.(map[string]any); ok {
								if t, _ := sMap["text"].(string); strings.TrimSpace(t) != "" {
									pendingReasoning = append(pendingReasoning, strings.TrimSpace(t))
								}
							}
						}
					} else if t, _ := item["text"].(string); strings.TrimSpace(t) != "" {
						pendingReasoning = append(pendingReasoning, strings.TrimSpace(t))
					}
					continue

				case "input_text", "input_image", "image":
					flushPendingAssistant()
					messages = append(messages, map[string]any{
						"role":    "user",
						"content": []any{item},
					})
					continue
				}

				if role == "" {
					role = "user"
				}
				if role == "assistant" || role == "model" {
					flushPendingAssistant()
					msg := normalizeResponsesAssistantMessage(item, i)
					if len(pendingReasoning) > 0 {
						existing, _ := msg["reasoning_content"].(string)
						if existing != "" {
							msg["reasoning_content"] = strings.Join(pendingReasoning, "\n") + "\n" + existing
						} else {
							msg["reasoning_content"] = strings.Join(pendingReasoning, "\n")
						}
						pendingReasoning = nil
					}
					if tcs, ok := msg["tool_calls"].([]any); ok {
						pendingToolCalls = append(pendingToolCalls[:0], tcs...)
						delete(msg, "tool_calls")
						recordToolCallNames(tcs)
					}
					pendingAssistant = msg
				} else {
					flushPendingAssistant()
					messages = append(messages, map[string]any{
						"role":    role,
						"content": normalizeResponsesContentParts(item["content"]),
					})
				}
			}
		}
		flushPendingAssistant()
	}

	if len(messages) == 0 {
		return nil, nil, nil, transport.NewUpstreamError("input must contain at least one message or non-empty string", 400, "invalid_request_error", nil, nil)
	}

	tools = normalizeResponsesTools(body["tools"])
	toolChoice = normalizeResponsesToolChoice(body["tool_choice"])
	return messages, tools, toolChoice, nil
}

func normalizeResponsesAssistantMessage(item map[string]any, idx int) map[string]any {
	var textParts []string
	var reasoningParts []string
	var toolCalls []any

	if tcs, ok := item["tool_calls"].([]any); ok {
		toolCalls = append(toolCalls, tcs...)
	}

	switch cp := item["content"].(type) {
	case string:
		if cp != "" {
			textParts = append(textParts, cp)
		}
	case []any:
		for cIdx, cAny := range cp {
			cMap, ok := cAny.(map[string]any)
			if !ok || cMap == nil {
				continue
			}
			cType, _ := cMap["type"].(string)
			switch cType {
			case "output_text", "text", "input_text":
				if t, _ := cMap["text"].(string); t != "" {
					textParts = append(textParts, t)
				}
			case "reasoning", "summary_text", "thinking":
				if t, _ := cMap["text"].(string); t != "" {
					reasoningParts = append(reasoningParts, t)
				} else if t, _ := cMap["thinking"].(string); t != "" {
					reasoningParts = append(reasoningParts, t)
				}
			case "function_call", "tool_use":
				callID, _ := cMap["call_id"].(string)
				if callID == "" {
					callID, _ = cMap["id"].(string)
				}
				if callID == "" {
					callID = fmt.Sprintf("call_%d_%d", idx+1, cIdx+1)
				}
				name, _ := cMap["name"].(string)
				argsStr := "{}"
				if a, ok := cMap["arguments"].(string); ok && a != "" {
					argsStr = a
				} else if aObj, ok := cMap["arguments"].(map[string]any); ok {
					b, _ := json.Marshal(aObj)
					argsStr = string(b)
				} else if inObj, ok := cMap["input"].(map[string]any); ok {
					b, _ := json.Marshal(inObj)
					argsStr = string(b)
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": argsStr,
					},
				})
			}
		}
	}

	var content any
	if len(textParts) > 0 {
		content = strings.Join(textParts, "\n")
	}
	msg := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	if len(reasoningParts) > 0 {
		msg["reasoning_content"] = strings.Join(reasoningParts, "\n")
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg
}

func normalizeResponsesContentParts(raw any) any {
	arr, ok := raw.([]any)
	if !ok {
		return raw
	}
	out := make([]any, 0, len(arr))
	for _, pAny := range arr {
		pMap, ok := pAny.(map[string]any)
		if !ok || pMap == nil {
			out = append(out, pAny)
			continue
		}
		pType, _ := pMap["type"].(string)
		if pType == "output_text" {
			t, _ := pMap["text"].(string)
			out = append(out, map[string]any{"type": "text", "text": t})
			continue
		}
		out = append(out, pMap)
	}
	return out
}

func normalizeResponsesTools(raw any) any {
	out, _, _ := normalizeResponsesToolsWithMeta(raw)
	return out
}

func normalizeResponsesToolsWithMeta(raw any) (normalized any, customTools map[string]bool, namespaceOf map[string]string) {
	arr, ok := raw.([]any)
	if !ok {
		return raw, nil, nil
	}
	customTools = make(map[string]bool)
	namespaceOf = make(map[string]string)
	out := make([]any, 0, len(arr))
	var visit func(items []any, ns string)
	visit = func(items []any, ns string) {
		for _, tAny := range items {
			tMap, ok := tAny.(map[string]any)
			if !ok || tMap == nil {
				out = append(out, tAny)
				continue
			}
			tType, _ := tMap["type"].(string)
			if tType == "namespace" {
				nsName, _ := tMap["name"].(string)
				if subTools, ok := tMap["tools"].([]any); ok {
					visit(subTools, nsName)
				}
				continue
			}
			if fnSub, hasFn := tMap["function"].(map[string]any); hasFn && fnSub != nil {
				if fnName, _ := fnSub["name"].(string); fnName != "" && ns != "" {
					namespaceOf[fnName] = ns
				}
				if tType == "custom" {
					if fnName, _ := fnSub["name"].(string); fnName != "" {
						customTools[fnName] = true
					}
				}
				out = append(out, tMap)
				continue
			}
			name, _ := tMap["name"].(string)
			if name == "" && tType == "local_shell" {
				name = "local_shell"
			}
			if name != "" {
				if ns != "" {
					namespaceOf[name] = ns
				}
				fnObj := map[string]any{
					"name": name,
				}
				if desc, ok := tMap["description"].(string); ok {
					fnObj["description"] = desc
				} else if tType == "local_shell" {
					fnObj["description"] = "Execute a shell command in the local workspace."
				}
				if params, exists := tMap["parameters"]; exists {
					fnObj["parameters"] = params
				} else if schema, exists := tMap["input_schema"]; exists {
					fnObj["input_schema"] = schema
				} else if tType == "custom" {
					customTools[name] = true
					fnObj["parameters"] = map[string]any{
						"type": "object",
						"properties": map[string]any{
							"input": map[string]any{
								"type":        "string",
								"description": "Raw input string for " + name + ".",
							},
						},
						"required": []any{"input"},
					}
				} else if tType == "local_shell" {
					fnObj["parameters"] = map[string]any{
						"type": "object",
						"properties": map[string]any{
							"command": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "string"},
								"description": "Command and arguments to execute.",
							},
						},
						"required": []any{"command"},
					}
				}
				out = append(out, map[string]any{
					"type":     "function",
					"function": fnObj,
				})
				continue
			}
			out = append(out, tMap)
		}
	}
	visit(arr, "")
	return out, customTools, namespaceOf
}

func extractCustomToolInput(argsJSON string) string {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &parsed); err == nil && parsed != nil {
		if s, ok := parsed["input"].(string); ok {
			return s
		}
		for _, v := range parsed {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return argsJSON
}

func normalizeResponsesToolChoice(raw any) any {
	tcMap, ok := raw.(map[string]any)
	if !ok || tcMap == nil {
		return raw
	}
	if _, hasFn := tcMap["function"].(map[string]any); hasFn {
		return tcMap
	}
	tStr, _ := tcMap["type"].(string)
	name, _ := tcMap["name"].(string)
	if tStr == "function" && name != "" {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": name,
			},
		}
	}
	return raw
}

func buildResponsesUsage(chatUsage map[string]any) map[string]any {
	inTok, _ := chatUsage["prompt_tokens"].(int)
	outTok, _ := chatUsage["completion_tokens"].(int)
	totTok, _ := chatUsage["total_tokens"].(int)
	cachedTok := 0
	if pDetails, ok := chatUsage["prompt_tokens_details"].(map[string]any); ok {
		cachedTok, _ = pDetails["cached_tokens"].(int)
	}
	res := map[string]any{
		"input_tokens":  inTok,
		"output_tokens": outTok,
		"total_tokens":  totTok,
		"input_tokens_details": map[string]any{
			"cached_tokens": cachedTok,
		},
	}
	if details, ok := chatUsage["completion_tokens_details"].(map[string]any); ok {
		if rTok, ok := details["reasoning_tokens"].(int); ok && rTok > 0 {
			res["output_tokens_details"] = map[string]any{
				"reasoning_tokens": rTok,
			}
		}
	}
	return res
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HandleResponses executes an OpenAI Responses API (`POST /v1/responses`) request
// with real-time SSE streaming, reasoning summary events, tool call events, session
// continuation caching, and 1M-token context offloading.
func (s *Server) HandleResponses(ctx context.Context, w http.ResponseWriter, body map[string]any, scope AuthScope) {
	if body == nil {
		sendChatError(w, false, transport.NewUpstreamError("request body must be a JSON object", 400, "invalid_request_error", nil, nil))
		return
	}
	if mRaw, exists := body["model"]; exists && mRaw != nil {
		if _, ok := mRaw.(string); !ok {
			sendChatError(w, false, transport.NewUpstreamError("model must be a string", 400, "invalid_request_error", nil, nil))
			return
		}
	}
	_, customTools, namespaceOf := normalizeResponsesToolsWithMeta(body["tools"])
	messages, toolsRaw, toolChoiceRaw, normErr := NormalizeResponsesInput(body)
	if normErr != nil {
		sendChatError(w, false, normErr)
		return
	}
	reqStartedAt := time.Now()
	prep, prepErr := s.prepareGeneration(ctx, body, scope, messages, toolsRaw, toolChoiceRaw)
	prepareMs := time.Since(reqStartedAt).Milliseconds()
	if prepErr != nil {
		sendChatError(w, false, prepErr)
		return
	}

	accts := prep.accts
	account := prep.account
	model := prep.model
	mid := prep.mid
	requested := prep.requested
	effortStr := prep.effortStr
	hasTools := prep.hasTools
	toolsRaw = prep.toolsRaw
	stream := prep.stream
	buffered := prep.buffered
	opts := prep.opts
	fullPrompt := prep.fullPrompt
	curPrompt := prep.curPrompt
	flattenStats := prep.flattenStats
	overflowParts := prep.overflowParts
	imageParts := prep.imageParts
	fullAttachmentParts := prep.fullAttachmentParts
	attachmentParts := prep.attachmentParts
	hasher := prep.hasher
	cacheKey := prep.cacheKey
	cacheUsed := cacheKey != ""
	respID := "resp_" + randomHex(8)
	rsID := "rs_" + randomHex(6)
	msgID := "msg_" + randomHex(6)
	created := time.Now().Unix()
	eagerStream := strings.EqualFold(s.Cfg.StreamBufferMode, "eager")

	headersSent := false
	// handlerDone is set before the handler returns. Keepalive and early-SSE
	// goroutines outlive the handler, and net/http nils the response writer once
	// the handler returns; writing or flushing past that point dereferences a nil
	// bufio.Writer and crashes the whole daemon.
	var handlerDone atomic.Bool
	var ttftMs atomic.Int64
	var headerMs, identityMs atomic.Int64
	var firstReasoningMs, firstContentMs, firstToolMs atomic.Int64
	var uploadMs, accountSwitches atomic.Int64
	ttftMs.Store(-1)
	headerMs.Store(-1)
	identityMs.Store(-1)
	firstReasoningMs.Store(-1)
	firstContentMs.Store(-1)
	firstToolMs.Store(-1)
	curAttempt := 0
	lastUpstreamStatus := 0
	lastRetryReason := ""
	var (
		streamMu       sync.Mutex
		seqNum         int
		reasoningAdded bool
		messageAdded   bool
	)
	flusher, _ := w.(http.Flusher)

	// markPhase records a phase timestamp once, in milliseconds from request
	// start. A value stays -1 until that phase is actually observed.
	markPhase := func(target *atomic.Int64) {
		target.CompareAndSwap(-1, time.Since(reqStartedAt).Milliseconds())
	}
	// markMeaningfulOutput records the first client-visible model output and
	// derives ttft_ms from the same event.
	markMeaningfulOutput := func(target *atomic.Int64) {
		markPhase(target)
		markPhase(&ttftMs)
	}
	// assignAccount counts an account switch only when failover moves the request
	// to a different account. Same-account retries leave the counter untouched.
	assignAccount := func(next *AccountState) {
		if next != nil && next != account {
			accountSwitches.Add(1)
		}
		account = next
	}

	emitSSE := func(eventType string, payload map[string]any) {
		if ctx.Err() != nil {
			return
		}
		streamMu.Lock()
		defer streamMu.Unlock()
		if handlerDone.Load() {
			return
		}
		seqNum++
		ev := make(map[string]any, len(payload)+2)
		for k, v := range payload {
			ev[k] = v
		}
		ev["type"] = eventType
		ev["sequence_number"] = seqNum
		b, _ := json.Marshal(ev)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(b))
		if flusher != nil {
			flusher.Flush()
		}
	}

	var pinger *streamPinger
	var pingerMu sync.Mutex
	stopKeepalive := func() {
		pingerMu.Lock()
		p := pinger
		pinger = nil
		pingerMu.Unlock()
		p.Stop()
	}
	defer stopKeepalive()
	// Defers run LIFO, so this runs before stopKeepalive: the pinger sees
	// handlerDone and exits instead of writing after the handler returns.
	defer handlerDone.Store(true)

	// streamStarted closes as soon as the response headers are flushed, so the
	// grace timer below does not keep a goroutine alive after streaming begins.
	streamStarted := make(chan struct{})
	var streamStartedOnce sync.Once
	markStreamStarted := func() { streamStartedOnce.Do(func() { close(streamStarted) }) }

	startStreamWith := func(early bool, attempt, upstreamStatus int, retryReason string) {
		streamMu.Lock()
		if headersSent || ctx.Err() != nil || handlerDone.Load() {
			streamMu.Unlock()
			return
		}
		setGSBHeaders(w, false, account.ID, mid, attempt, upstreamStatus, retryReason)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)
		headersSent = true
		markStreamStarted()
		// header_ms is the actual response header commit, independent of any
		// early flush or first meaningful delta.
		markPhase(&headerMs)
		_, _ = io.WriteString(w, ": ping\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		streamMu.Unlock()

		pingerMu.Lock()
		if pinger == nil && !handlerDone.Load() {
			pinger = startStreamPinger(&streamMu, w, flusher, defaultKeepaliveInterval, nil)
		}
		pingerMu.Unlock()

		baseResp := map[string]any{
			"id":         respID,
			"object":     "response",
			"created_at": created,
			"model":      mid,
			"status":     "in_progress",
			"output":     []any{},
			"usage":      nil,
		}
		emitSSE("response.created", map[string]any{"response": baseResp})
		emitSSE("response.in_progress", map[string]any{"response": baseResp})
	}

	// startStream reads the caller-owned attempt diagnostics, so it must only be
	// called from the request goroutine.
	startStream := func(early bool) {
		startStreamWith(early, curAttempt, lastUpstreamStatus, lastRetryReason)
	}

	if stream && s.Cfg.EarlySSE && (eagerStream || !s.Cfg.RejectDowngrade) {
		startStream(false)
	} else if stream && buffered && s.Cfg.EarlySSEGrace > 0 && (eagerStream || !s.Cfg.RejectDowngrade) {
		go func() {
			timer := time.NewTimer(s.Cfg.EarlySSEGrace)
			defer timer.Stop()
			select {
			case <-timer.C:
				// No upstream call has completed yet, so attempt/status/retry are
				// unknown here; pass -1 to omit them instead of reading variables owned
				// by the request goroutine.
				startStreamWith(true, -1, 0, "")
			case <-ctx.Done():
			case <-streamStarted:
			}
		}()
	}

	emitReasoningDelta := func(delta string) {
		if delta == "" {
			return
		}
		markMeaningfulOutput(&firstReasoningMs)
		startStream(false)
		if !reasoningAdded {
			reasoningAdded = true
			emitSSE("response.output_item.added", map[string]any{
				"output_index": 0,
				"item": map[string]any{
					"id":      rsID,
					"type":    "reasoning",
					"summary": []any{},
				},
			})
			emitSSE("response.reasoning_summary_part.added", map[string]any{
				"item_id":       rsID,
				"output_index":  0,
				"summary_index": 0,
				"part": map[string]any{
					"type": "summary_text",
					"text": "",
				},
			})
		}
		emitSSE("response.reasoning_summary_text.delta", map[string]any{
			"item_id":       rsID,
			"output_index":  0,
			"summary_index": 0,
			"delta":         delta,
		})
	}

	emitTextDelta := func(delta string) {
		if delta == "" {
			return
		}
		markMeaningfulOutput(&firstContentMs)
		startStream(false)
		outIdx := 0
		if reasoningAdded {
			outIdx = 1
		}
		if !messageAdded {
			messageAdded = true
			emitSSE("response.output_item.added", map[string]any{
				"output_index": outIdx,
				"item": map[string]any{
					"id":      msgID,
					"type":    "message",
					"role":    "assistant",
					"status":  "in_progress",
					"content": []any{},
				},
			})
			emitSSE("response.content_part.added", map[string]any{
				"item_id":       msgID,
				"output_index":  outIdx,
				"content_index": 0,
				"part": map[string]any{
					"type":        "output_text",
					"text":        "",
					"annotations": []any{},
				},
			})
		}
		emitSSE("response.output_text.delta", map[string]any{
			"item_id":       msgID,
			"output_index":  outIdx,
			"content_index": 0,
			"delta":         delta,
		})
	}

	account.mu.RLock()
	originGroup := account.AccountGroup
	account.mu.RUnlock()

	maxAttempts := s.Cfg.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	maxUpstream := s.Cfg.MaxUpstreamAttempts
	if maxUpstream < 1 {
		maxUpstream = maxAttempts
	}
	upstreamCalls := 0
	var retryDeadline time.Time
	if s.Cfg.RetryBudget > 0 {
		retryDeadline = reqStartedAt.Add(s.Cfg.RetryBudget)
	}

	triedAccounts := map[string]bool{account.ID: true}
	freshSocketNext := false
	rebootstrapNext := false
	serverMediaRounds := 0
	for attempt := 0; attempt < maxAttempts; attempt++ {
		curAttempt = attempt
		attemptStartedAt := time.Now()
		var (
			candidateID           string
			selected              bool
			done                  bool
			text, thoughts        string
			sentText, sentThought string
			servedID, servedLabel string
			identityConflict      bool
			mediaToolUsed         bool
			lastImages            []gemini.GeneratedImage
			lastVideos            []gemini.GeneratedVideo
			lastCID, lastRID      string
			lastContextToken      string
			grounding             []gemini.GroundingSource
			attemptStateMu        sync.Mutex
		)
		observeIdentity := func(ansID, ansLabel string) {
			if ansID == "" {
				return
			}
			if servedID != "" && servedID != ansID {
				identityConflict = true
			}
			servedID = ansID
			if ansLabel != "" {
				servedLabel = ansLabel
			}
		}
		c := s.clientFor(account)
		opts.FreshSocket = freshSocketNext
		if rebootstrapNext {
			_ = c.Bootstrap(ctx)
		}
		freshSocketNext = false
		rebootstrapNext = false

		uploadStartedAt := time.Now()
		files, uploadErr := s.uploadAttachments(ctx, account.ID, c, attachmentParts)
		uploadMs.Add(time.Since(uploadStartedAt).Milliseconds())
		if len(files) > 0 {
			opts.Files = files
		}

		var upstream *gemini.StreamResponse
		var streamErr error
		if uploadErr != nil {
			streamErr = uploadErr
		} else {
			upstreamCalls++
			account.mu.Lock()
			account.activeStreams++
			account.mu.Unlock()
			upstream, streamErr = c.StreamGenerateLive(ctx, curPrompt, opts, func(update *gemini.LineParseResult) error {
				attemptStateMu.Lock()
				if update.MediaToolActive {
					mediaToolUsed = true
				}
				if mediaToolUsed {
					servedID = model.ModelID
					servedLabel = model.ModelName
					identityConflict = false
				} else {
					observeIdentity(update.AnsweredModelID, update.AnsweredModelLabel)
				}
				if servedID != "" {
					markPhase(&identityMs)
				}
				earlyMismatch := !mediaToolUsed && s.Cfg.RejectDowngrade && (servedID != "" || servedLabel != "") && !gemini.IsMatchingServedModel(*model, servedID, servedLabel)
				identityVerifiedNow := canStreamVerified(eagerStream, s.Cfg.RejectDowngrade, identityConflict, *model, servedID, servedLabel)
				attemptStateMu.Unlock()
				if earlyMismatch {
					return transport.ErrStopStreaming
				}
				if stream && buffered && ctx.Err() == nil && identityVerifiedNow {
					startStream(false)
				}

				if update.CID != "" {
					lastCID = update.CID
				}
				if update.RID != "" {
					lastRID = update.RID
				}
				if update.ContextToken != "" {
					lastContextToken = update.ContextToken
				}
				grounding = gemini.MergeGroundingSources(grounding, update.GroundingSources)
				for _, frame := range update.Frames {
					if !selected {
						candidateID = frame.RCID
						selected = true
					}
					if frame.RCID != candidateID {
						continue
					}
					attemptStateMu.Lock()
					s.rewriteFrameMedia(ctx, account.ID, &frame)
					if len(frame.GeneratedImages) > 0 || len(frame.GeneratedVideos) > 0 {
						mediaToolUsed = true
						if len(frame.GeneratedImages) > 0 {
							lastImages = frame.GeneratedImages
						}
						if len(frame.GeneratedVideos) > 0 {
							lastVideos = frame.GeneratedVideos
						}
					}
					if mediaToolUsed {
						servedID = model.ModelID
						servedLabel = model.ModelName
						identityConflict = false
					} else {
						observeIdentity(frame.AnsweredModelID, frame.AnsweredModelLabel)
					}
					if servedID != "" {
						markPhase(&identityMs)
					}
					if frame.Text != "" || text == "" {
						if !shouldPreserveTextOnErrorFrame(text, frame.Text, sentText, hasTools, toolsRaw, toolChoiceRaw, nil) {
							text = frame.Text
						}
					}
					thoughts = mergeStreamedThoughts(thoughts, frame.Thoughts)
					done = frame.Done
					frameMismatch := s.Cfg.RejectDowngrade && (servedID != "" || servedLabel != "") && !gemini.IsMatchingServedModel(*model, servedID, servedLabel)
					attemptStateMu.Unlock()
					if frameMismatch {
						return transport.ErrStopStreaming
					}

					if stream && !buffered && ctx.Err() == nil {
						if !strings.HasPrefix(text, sentText) || (s.Cfg.LiveThoughts && !strings.HasPrefix(thoughts, sentThought)) {
							return transport.NewUpstreamError("upstream revised an already streamed answer", 502, "upstream_revision", nil, nil)
						}
						if s.Cfg.LiveThoughts && len(thoughts) > len(sentThought) {
							emitReasoningDelta(thoughts[len(sentThought):])
							sentThought = thoughts
						}
						if len(text) > len(sentText) {
							emitTextDelta(text[len(sentText):])
							sentText = text
						}
					} else if stream && buffered && ctx.Err() == nil && !done {
						if sentText != "" && !hasCompatibleStreamPrefix(text, sentText) {
							return transport.NewUpstreamError("upstream revised an already streamed answer", 502, "upstream_revision", nil, nil)
						}
						// Thoughts are normalized monotonically via mergeStreamedThoughts.
						// A matching served-model id proves no downgrade is in progress, so
						// the headers no longer need to be held for an HTTP error.
						if canStreamVerified(eagerStream, s.Cfg.RejectDowngrade, identityConflict, *model, servedID, servedLabel) {
							startStream(false)
						}
						identityVerified := canStreamVerified(eagerStream, s.Cfg.RejectDowngrade, identityConflict, *model, servedID, servedLabel)
						if identityVerified && s.Cfg.LiveThoughts {
							if hasTools && !s.Cfg.KeepToolPreamble && sentText == "" {
								if pre := toolbridge.ExtractPreToolPreamble(text); pre != "" {
									thoughts = appendPreambleToThoughts(thoughts, pre)
								}
							}
							if len(thoughts) > len(sentThought) && strings.HasPrefix(thoughts, sentThought) {
								emitReasoningDelta(thoughts[len(sentThought):])
								sentThought = thoughts
							} else if servedID != "" && time.Since(attemptStartedAt) >= 5*time.Second {
								startStream(false)
							}
						}
						canStreamText := canStreamVerified(eagerStream, s.Cfg.RejectDowngrade, identityConflict, *model, servedID, servedLabel)
						if canStreamText {
							if !hasTools && strings.HasPrefix(text, sentText) && len(text) > 240 && len(text) > len(sentText) && gemini.GeminiErrorReply(text) == "" {
								emitTextDelta(text[len(sentText):])
								sentText = text
						} else if hasTools && !mediaToolUsed && len(text) > 240 {
							if safePrefix := toolbridge.SafeStreamablePrefix(text); len(safePrefix) > len(sentText) && strings.HasPrefix(safePrefix, sentText) && gemini.GeminiErrorReply(text[:240]) == "" {
									emitTextDelta(safePrefix[len(sentText):])
									sentText = safePrefix
								}
							}
						}
					}
				}
				return nil
			})
			account.mu.Lock()
			if account.activeStreams > 0 {
				account.activeStreams--
			}
			account.mu.Unlock()
		}
		if upstream != nil && upstream.Status > 0 {
			lastUpstreamStatus = upstream.Status
		}
		if upstream != nil {
			grounding = gemini.MergeGroundingSources(grounding, upstream.GroundingSources)
		}

		interpreted, runCheck := s.evaluateAttemptCompletion(
			ctx,
			account,
			*model,
			mid,
			upstream,
			streamErr,
			selected,
			done,
			text,
			sentText,
			servedID,
			servedLabel,
			identityConflict,
			hasTools,
			toolsRaw,
			toolChoiceRaw,
		)

		if runCheck != nil {
			var ue *transport.UpstreamError
			errors.As(runCheck, &ue)
			errCode := ""
			errStatus := 502
			if ue != nil {
				errCode = ue.Code
				errStatus = ue.StatusCode
				if lastUpstreamStatus == 0 && errStatus > 0 {
					lastUpstreamStatus = errStatus
				}
			}
			if errCode == "account_banned" {
				s.markAccountQuarantined(account, runCheck.Error())
			} else if errCode != "session_busy" && errCode != "client_cancelled" && errCode != "upstream_error_reply" && errCode != "tool_call_error" && errCode != "model_label_mismatch" {
				s.markAccountFailure(account, runCheck.Error())
				if errCode != "empty_upstream_response" {
					s.refreshQuotaAsync(account, c)
				}
			}
			if cacheKey != "" && len(opts.Metadata) > 0 && (errCode == "empty_upstream_response" || errCode == "incomplete_upstream_response" || errCode == "upstream_error_reply" || errCode == "upstream_error" || (errCode == "upstream_http_error" && errStatus >= 400)) {
				s.deleteSessionCache(cacheKey)
				cacheKey = ""
			}

			payloadCommitted := func() bool {
				return sentText != "" || messageAdded
			}
			responseCommitted := func() bool {
				streamMu.Lock()
				headers := headersSent
				streamMu.Unlock()
				return headers || payloadCommitted() || sentThought != ""
			}
			inStreamRetryable := (errCode == "tool_call_error" ||
				errCode == "upstream_error_reply" ||
				errCode == "empty_upstream_response" ||
				errCode == "incomplete_upstream_response" ||
				(scope.AccountID == "" && (errCode == "model_substitution" ||
					errStatus == 429 ||
					errCode == "rate_limit_error" ||
					errCode == "session_busy" ||
					errCode == "upstream_auth_error" ||
					errCode == "auth_redirect" ||
					errCode == "upstream_connect_timeout" ||
					errCode == "upstream_stalled" ||
					errCode == "upstream_timeout" ||
					errCode == "upstream_degraded" ||
					errCode == "model_identity_unknown" ||
					errCode == "model_identity_conflict"))) && !payloadCommitted()
			canRetryMore := attempt+1 < maxAttempts && upstreamCalls < maxUpstream &&
				(retryDeadline.IsZero() || time.Now().Before(retryDeadline)) &&
				(!responseCommitted() || inStreamRetryable) && ctx.Err() == nil &&
				errCode != "account_banned"
			if canRetryMore {
				if errCode == "session_busy" {
					lastRetryReason = "session_busy"
					if scope.AccountID == "" {
						if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, originGroup, requested, hasher); nextAcct != nil && nextModel != nil {
							triedAccounts[nextAcct.ID] = true
							assignAccount(nextAcct)
							model = nextModel
							opts.Model = nextModel
							opts.Metadata = nil
							opts.Files = nil
							curPrompt = fullPrompt
							attachmentParts = fullAttachmentParts
							continue
						}
					}
					waitDur := parseRetryAfterDuration(ue, 2*time.Second, retryDeadline)
					if waitDur > 0 {
						select {
						case <-time.After(waitDur):
						case <-ctx.Done():
						}
					}
					if ctx.Err() == nil {
						continue
					}
				}
				if errCode == "model_substitution" || errStatus == 429 || errCode == "rate_limit_error" {
					lastRetryReason = errCode
					if lastRetryReason == "" {
						lastRetryReason = "rate_limit_error"
					}
					// Switching accounts is a pool-only operation and can only happen
					// before any response bytes commit the current attempt.
					if scope.AccountID == "" {
						if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, originGroup, requested, hasher); nextAcct != nil && nextModel != nil {
							triedAccounts[nextAcct.ID] = true
							assignAccount(nextAcct)
							model = nextModel
							opts.Model = nextModel
							opts.Metadata = nil
							opts.Files = nil
							curPrompt = fullPrompt
							attachmentParts = fullAttachmentParts
							continue
						}
					}
				}
				if errStatus == 401 || errCode == "upstream_auth_error" || errCode == "auth_redirect" {
					lastRetryReason = errCode
					c.InvalidateAuth()
					switched := false
					if scope.AccountID == "" {
						if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, originGroup, requested, hasher); nextAcct != nil && nextModel != nil {
							triedAccounts[nextAcct.ID] = true
							assignAccount(nextAcct)
							model = nextModel
							opts.Model = nextModel
							opts.Metadata = nil
							opts.Files = nil
							curPrompt = fullPrompt
							attachmentParts = fullAttachmentParts
							switched = true
						}
					}
					if !switched {
						freshSocketNext = true
						rebootstrapNext = true
					}
					continue
				}
				if errCode == "upstream_connect_timeout" ||
					errCode == "upstream_stalled" ||
					errCode == "upstream_timeout" {
					lastRetryReason = errCode
					if scope.AccountID == "" {
						if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, originGroup, requested, hasher); nextAcct != nil && nextModel != nil {
							triedAccounts[nextAcct.ID] = true
							assignAccount(nextAcct)
							model = nextModel
							opts.Model = nextModel
							opts.Metadata = nil
							opts.Files = nil
							curPrompt = fullPrompt
							attachmentParts = fullAttachmentParts
							continue
						}
					}
				}
				if errCode == "upstream_degraded" {
					// Gemini Web failed the turn on its side while the account still
					// has quota. Prefer a different account; if the request is pinned
					// or the pool is spent, retry once on a fresh socket.
					lastRetryReason = errCode
					if scope.AccountID == "" {
						if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, originGroup, requested, hasher); nextAcct != nil && nextModel != nil {
							triedAccounts[nextAcct.ID] = true
							assignAccount(nextAcct)
							model = nextModel
							opts.Model = nextModel
							opts.Metadata = nil
							opts.Files = nil
							curPrompt = fullPrompt
							attachmentParts = fullAttachmentParts
							continue
						}
					}
					freshSocketNext = true
					rebootstrapNext = true
					opts.Metadata = nil
					opts.Files = nil
					curPrompt = fullPrompt
					attachmentParts = fullAttachmentParts
					continue
				}
				if errCode == "model_identity_unknown" || errCode == "model_identity_conflict" {
					lastRetryReason = errCode
					if scope.AccountID == "" {
						if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, originGroup, requested, hasher); nextAcct != nil && nextModel != nil {
							triedAccounts[nextAcct.ID] = true
							assignAccount(nextAcct)
							model = nextModel
							opts.Model = nextModel
							opts.Metadata = nil
							opts.Files = nil
							curPrompt = fullPrompt
							attachmentParts = fullAttachmentParts
							continue
						}
					}
				}
				if errCode == "upstream_transport_error" ||
					errCode == "upstream_error" ||
					errCode == "incomplete_upstream_response" ||
					errCode == "tool_call_error" {
					lastRetryReason = errCode
					if errCode != "tool_call_error" {
						freshSocketNext = true
						rebootstrapNext = true
					}
					opts.Metadata = nil
					opts.Files = nil
					curPrompt = fullPrompt
					if errCode == "tool_call_error" {
						curPrompt = appendToolErrorCorrection(fullPrompt, runCheck.Error())
					}
					attachmentParts = fullAttachmentParts
					continue
				}
				if errCode == "upstream_error_reply" {
					if attempt == 0 {
						lastRetryReason = errCode
						freshSocketNext = true
						rebootstrapNext = true
						opts.Metadata = nil
						opts.Files = nil
						select {
						case <-time.After(100 * time.Millisecond):
						case <-ctx.Done():
						}
						if len(fullPrompt) > 120000 {
							if compacted, extraOverflow, cErr := prompt.FlattenMessages(messages, toolsRaw, toolChoiceRaw, prompt.FlattenOptions{
								StartIndex:      0,
								MaxPromptChars:  100000,
								MaxOldToolChars: 12000,
								AllowOverflow:   s.Cfg.HistoryOffload,
							}); cErr == nil {
								curPrompt = appendErrorReplyRecoveryNote(compacted, hasTools)
								attachmentParts = make([]prompt.AttachmentPart, 0, len(extraOverflow)+len(imageParts))
								attachmentParts = append(attachmentParts, extraOverflow...)
								attachmentParts = append(attachmentParts, imageParts...)
								continue
							}
						}
						curPrompt = appendErrorReplyRecoveryNote(fullPrompt, hasTools)
						attachmentParts = fullAttachmentParts
						continue
					}
				}
				if errCode == "empty_upstream_response" {
					lastRetryReason = errCode
					wasCachedContinuation := len(opts.Metadata) > 0
					opts.Metadata = nil
					opts.Files = nil
					switched := false
					if !wasCachedContinuation && scope.AccountID == "" {
						if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, originGroup, requested, hasher); nextAcct != nil && nextModel != nil {
							triedAccounts[nextAcct.ID] = true
							assignAccount(nextAcct)
							model = nextModel
							opts.Model = nextModel
							switched = true
						}
					}
					if !switched {
						freshSocketNext = true
						rebootstrapNext = true
					}
					curPrompt = fullPrompt
					attachmentParts = fullAttachmentParts
					continue
				}
			}

			stopKeepalive()
			setGSBHeaders(w, headersSent, account.ID, mid, curAttempt, lastUpstreamStatus, lastRetryReason)
			totalMs := time.Since(reqStartedAt).Milliseconds()
			s.logRequestJSON(map[string]any{
				"req_id":             respID,
				"endpoint":           "/v1/responses",
				"model":              mid,
				"account":            account.ID,
				"effort":             effortStr,
				"prompt_bytes":       len(curPrompt),
				"header_bytes":       flattenStats.HeaderBytes,
				"body_bytes":         flattenStats.BodyBytes,
				"footer_bytes":       flattenStats.FooterBytes,
				"tool_bytes":         flattenStats.ToolSchemaBytes,
				"ttft_ms":            ttftMs.Load(),
				"total_ms":           totalMs,
				"status":             errStatus,
				"error_code":         errCode,
				"cached_tokens":      0,
				"attempts":           curAttempt + 1,
				"prepare_ms":         prepareMs,
				"cache_hit":          cacheUsed,
				"cache_prefix_split": prep.cacheSplit,
				"upload_ms":          uploadMs.Load(),
				"identity_ms":        identityMs.Load(),
				"header_ms":          headerMs.Load(),
				"first_reasoning_ms": firstReasoningMs.Load(),
				"first_content_ms":   firstContentMs.Load(),
				"first_tool_ms":      firstToolMs.Load(),
				"done_ms":            totalMs,
				"account_switches":   accountSwitches.Load(),
				"requested_model":    mid,
				"served_model":       servedID,
				"served_label":       servedLabel,
				"label_mismatch":     gemini.ServedLabelMismatch(*model, servedID, servedLabel),
			})
			if ctx.Err() == nil {
				sendChatError(w, headersSent, runCheck)
			}
			return
		}

		// Success
		var calls []toolbridge.Call
		if interpreted != nil {
			calls = interpreted.Calls
		}
		if hasServerSideMediaCalls(calls) && serverMediaRounds < 2 {
			mediaStart := time.Now()
			promptAdd, clientCalls, _ := s.executeServerSideMediaCalls(ctx, accts, scope, account, model, calls)
			if !retryDeadline.IsZero() {
				retryDeadline = retryDeadline.Add(time.Since(mediaStart))
			}
			if len(clientCalls) == 0 {
				fullPrompt = fullPrompt + promptAdd
				curPrompt = fullPrompt
				opts.Metadata = nil
				serverMediaRounds++
				attempt--
				if upstreamCalls > 0 {
					upstreamCalls--
				}
				continue
			}
			calls = clientCalls
		} else if hasTools && len(calls) == 0 && (len(lastImages) > 0 || len(lastVideos) > 0) && serverMediaRounds < 2 && sentText == "" {
			if promptAdd := formatDirectMediaToolContinuation(lastImages, lastVideos); promptAdd != "" {
				fullPrompt = fullPrompt + promptAdd
				curPrompt = fullPrompt
				opts.Metadata = nil
				serverMediaRounds++
				attempt--
				if upstreamCalls > 0 {
					upstreamCalls--
				}
				continue
			}
		}
		var contentStr string
		if len(calls) > 0 {
			if (s.Cfg.KeepToolPreamble || sentText != "") && interpreted.Text != "" {
				contentStr = interpreted.Text
			} else if s.Cfg.LiveThoughts && interpreted != nil && strings.TrimSpace(interpreted.Text) != "" {
				thoughts = appendPreambleToThoughts(thoughts, interpreted.Text)
			}
		} else {
			contentStr = text
			if sentText == "" && thoughts != "" {
				contentStr = stripLeakedThoughtsFromContent(contentStr, thoughts)
			}
		}
		// Web-search citations are appended to the answer so the client sees the
		// sources the model actually used, in the order Google returned them.
		if s.Cfg.IncludeGrounding && len(grounding) > 0 && len(calls) == 0 {
			contentStr = appendGroundingCitations(contentStr, grounding)
		}

		oaCalls := toolbridge.ToOpenAIToolCalls(calls)

		chatUsage := s.computeChatUsage(fullPrompt, curPrompt, overflowParts, messages, toolsRaw, toolChoiceRaw, text, thoughts)
		respUsage := buildResponsesUsage(chatUsage)
		s.clearModelExhausted(account, model.ModelName)
		if !opts.Temporary {
			var cachedContent any = contentStr
			if len(oaCalls) > 0 && contentStr == "" {
				cachedContent = nil
			}
			s.saveSessionContinuation(account.ID, mid, messages, toolsRaw, toolChoiceRaw, cachedContent, oaCalls, lastCID, lastRID, candidateID, lastContextToken)
		}

		stopKeepalive()
		if ctx.Err() != nil {
			return
		}

		var outputItems []map[string]any
		if s.Cfg.LiveThoughts && thoughts != "" {
			outputItems = append(outputItems, map[string]any{
				"id":   rsID,
				"type": "reasoning",
				"summary": []map[string]any{
					{
						"type": "summary_text",
						"text": thoughts,
					},
				},
			})
		}
		if contentStr != "" || len(oaCalls) == 0 {
			outputItems = append(outputItems, map[string]any{
				"id":     msgID,
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []map[string]any{
					{
						"type":        "output_text",
						"text":        contentStr,
						"annotations": []any{},
					},
				},
			})
		}
		fcIDs := make([]string, len(oaCalls))
		for idx, tc := range oaCalls {
			fcID := fmt.Sprintf("fc_%s_%d", randomHex(4), idx+1)
			fcIDs[idx] = fcID
			if customTools[tc.Function.Name] {
				item := map[string]any{
					"id":      fcID,
					"type":    "custom_tool_call",
					"call_id": tc.ID,
					"name":    tc.Function.Name,
					"input":   extractCustomToolInput(tc.Function.Arguments),
					"status":  "completed",
				}
				if ns := namespaceOf[tc.Function.Name]; ns != "" {
					item["namespace"] = ns
				}
				outputItems = append(outputItems, item)
				continue
			}
			item := map[string]any{
				"id":        fcID,
				"type":      "function_call",
				"call_id":   tc.ID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
				"status":    "completed",
			}
			if ns := namespaceOf[tc.Function.Name]; ns != "" {
				item["namespace"] = ns
			}
			outputItems = append(outputItems, item)
		}

		if stream {
			startStream(false)
			if s.Cfg.LiveThoughts && thoughts != "" {
				if len(thoughts) > len(sentThought) {
					emitReasoningDelta(thoughts[len(sentThought):])
				}
				emitSSE("response.reasoning_summary_text.done", map[string]any{
					"item_id":       rsID,
					"output_index":  0,
					"summary_index": 0,
					"text":          thoughts,
				})
				emitSSE("response.output_item.done", map[string]any{
					"output_index": 0,
					"item":         outputItems[0],
				})
			}

			nextOutIdx := 0
			if s.Cfg.LiveThoughts && thoughts != "" {
				nextOutIdx = 1
			}
			if contentStr != "" || len(oaCalls) == 0 {
				if tail, ok := remainingStreamTail(contentStr, sentText); ok && tail != "" {
					emitTextDelta(tail)
				} else if !messageAdded {
					emitTextDelta("")
				}
				msgItem := map[string]any{
					"id":     msgID,
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{
							"type":        "output_text",
							"text":        contentStr,
							"annotations": []any{},
						},
					},
				}
				emitSSE("response.output_text.done", map[string]any{
					"item_id":       msgID,
					"output_index":  nextOutIdx,
					"content_index": 0,
					"text":          contentStr,
				})
				emitSSE("response.content_part.done", map[string]any{
					"item_id":       msgID,
					"output_index":  nextOutIdx,
					"content_index": 0,
					"part": map[string]any{
						"type":        "output_text",
						"text":        contentStr,
						"annotations": []any{},
					},
				})
				emitSSE("response.output_item.done", map[string]any{
					"output_index": nextOutIdx,
					"item":         msgItem,
				})
				nextOutIdx++
			}

			for idx, tc := range oaCalls {
				fcID := fcIDs[idx]
				fcOutIdx := nextOutIdx + idx
				ns := namespaceOf[tc.Function.Name]
				if customTools[tc.Function.Name] {
					rawIn := extractCustomToolInput(tc.Function.Arguments)
					doneItem := map[string]any{
						"id":      fcID,
						"type":    "custom_tool_call",
						"call_id": tc.ID,
						"name":    tc.Function.Name,
						"input":   rawIn,
						"status":  "completed",
					}
					addedItem := map[string]any{
						"id":      fcID,
						"type":    "custom_tool_call",
						"call_id": tc.ID,
						"name":    tc.Function.Name,
						"input":   "",
						"status":  "in_progress",
					}
					if ns != "" {
						doneItem["namespace"] = ns
						addedItem["namespace"] = ns
					}
					emitSSE("response.output_item.added", map[string]any{
						"output_index": fcOutIdx,
						"item":         addedItem,
					})
					markMeaningfulOutput(&firstToolMs)
					emitSSE("response.custom_tool_call_input.delta", map[string]any{
						"item_id":      fcID,
						"output_index": fcOutIdx,
						"delta":        rawIn,
					})
					emitSSE("response.custom_tool_call_input.done", map[string]any{
						"item_id":      fcID,
						"output_index": fcOutIdx,
						"input":        rawIn,
					})
					emitSSE("response.output_item.done", map[string]any{
						"output_index": fcOutIdx,
						"item":         doneItem,
					})
					continue
				}
				doneItem := map[string]any{
					"id":        fcID,
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
					"status":    "completed",
				}
				addedItem := map[string]any{
					"id":        fcID,
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": "",
					"status":    "in_progress",
				}
				if ns != "" {
					doneItem["namespace"] = ns
					addedItem["namespace"] = ns
				}
				emitSSE("response.output_item.added", map[string]any{
					"output_index": fcOutIdx,
					"item":         addedItem,
				})
				// The argument delta is the first client-visible function-call
				// output, including calls whose argument object is empty.
				markMeaningfulOutput(&firstToolMs)
				emitSSE("response.function_call_arguments.delta", map[string]any{
					"item_id":      fcID,
					"output_index": fcOutIdx,
					"delta":        tc.Function.Arguments,
				})
				emitSSE("response.function_call_arguments.done", map[string]any{
					"item_id":      fcID,
					"output_index": fcOutIdx,
					"arguments":    tc.Function.Arguments,
				})
				emitSSE("response.output_item.done", map[string]any{
					"output_index": fcOutIdx,
					"item":         doneItem,
				})
			}

			emitSSE("response.completed", map[string]any{
				"response": map[string]any{
					"id":         respID,
					"object":     "response",
					"created_at": created,
					"status":     "completed",
					"model":      mid,
					"output":     outputItems,
					"usage":      respUsage,
				},
			})
		} else {
			payload := map[string]any{
				"id":         respID,
				"object":     "response",
				"created_at": created,
				"status":     "completed",
				"model":      mid,
				"output":     outputItems,
				"usage":      respUsage,
			}
			setGSBHeaders(w, headersSent, account.ID, mid, curAttempt, lastUpstreamStatus, lastRetryReason)
			markPhase(&headerMs)
			sendJSON(w, 200, payload, nil)
		}

		cachedTok := 0
		if d, ok := chatUsage["prompt_tokens_details"].(map[string]any); ok {
			cachedTok, _ = d["cached_tokens"].(int)
		}
		totalMs := time.Since(reqStartedAt).Milliseconds()
		s.logRequestJSON(map[string]any{
			"req_id":             respID,
			"endpoint":           "/v1/responses",
			"model":              mid,
			"account":            account.ID,
			"effort":             effortStr,
			"prompt_bytes":       len(curPrompt),
			"header_bytes":       flattenStats.HeaderBytes,
			"body_bytes":         flattenStats.BodyBytes,
			"footer_bytes":       flattenStats.FooterBytes,
			"tool_bytes":         flattenStats.ToolSchemaBytes,
			"ttft_ms":            ttftMs.Load(),
			"total_ms":           totalMs,
			"status":             200,
			"error_code":         "",
			"cached_tokens":      cachedTok,
			"attempts":           curAttempt + 1,
			"prepare_ms":         prepareMs,
			"cache_hit":          cacheUsed,
			"cache_prefix_split": prep.cacheSplit,
			"upload_ms":          uploadMs.Load(),
			"identity_ms":        identityMs.Load(),
			"header_ms":          headerMs.Load(),
			"first_reasoning_ms": firstReasoningMs.Load(),
			"first_content_ms":   firstContentMs.Load(),
			"first_tool_ms":      firstToolMs.Load(),
			"done_ms":            totalMs,
			"account_switches":   accountSwitches.Load(),
			"requested_model":    mid,
			"served_model":       servedID,
			"served_label":       servedLabel,
			"label_mismatch":     gemini.ServedLabelMismatch(*model, servedID, servedLabel),
		})

		account.mu.Lock()
		account.Fails = 0
		account.LastUsed = s.now()
		account.mu.Unlock()
		s.refreshQuotaAsync(account, c)
		return
	}
}
