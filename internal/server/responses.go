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
	if estimated, _ := chatUsage["estimated"].(bool); estimated {
		res["estimated"] = true
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
	cfg := s.cfgSnapshot()
	respID := "resp_" + randomHex(8)
	rsID := "rs_" + randomHex(6)
	msgID := "msg_" + randomHex(6)
	created := time.Now().Unix()
	eagerStream := strings.EqualFold(cfg.StreamBufferMode, "eager")

	type streamTarget struct{ accountID, modelName string }
	var activeTarget atomic.Pointer[streamTarget]
	activeTarget.Store(&streamTarget{account.ID, mid})

	headersSent := false
	// handlerDone is set before the handler returns. Keepalive and early-SSE
	// goroutines outlive the handler, and net/http nils the response writer once
	// the handler returns; writing or flushing past that point dereferences a nil
	// bufio.Writer and crashes the whole daemon.
	var handlerDone atomic.Bool
	var ttftMs atomic.Int64
	var headerMs, identityMs atomic.Int64
	var firstReasoningMs, firstContentMs, firstToolMs atomic.Int64
	var preDispatchMs, firstUpstreamFrameMs atomic.Int64
	var uploadMs, accountSwitches atomic.Int64
	ttftMs.Store(-1)
	headerMs.Store(-1)
	identityMs.Store(-1)
	firstReasoningMs.Store(-1)
	firstContentMs.Store(-1)
	firstToolMs.Store(-1)
	preDispatchMs.Store(-1)
	firstUpstreamFrameMs.Store(-1)
	curAttempt := 0
	lastUpstreamStatus := 0
	lastRetryReason := ""
	var (
		streamMu       sync.Mutex
		seqNum         int
		reasoningAdded bool
		messageAdded   bool
	)
	var streamedReasoning strings.Builder
	var reasoningIndex, messageIndex, nextOutputIndex int
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
	assignAccount := func(next *AccountState, nextModel *gemini.ModelInfo) {
		if next != nil && next != account {
			accountSwitches.Add(1)
		}
		account, model, opts.Model = next, nextModel, nextModel
		mid = nextModel.ModelName
		activeTarget.Store(&streamTarget{next.ID, mid})
	}

	emitSSELocked := func(eventType string, payload map[string]any) {
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
	emitSSE := func(eventType string, payload map[string]any) {
		if ctx.Err() != nil {
			return
		}
		streamMu.Lock()
		defer streamMu.Unlock()
		emitSSELocked(eventType, payload)
	}

	// streamStarted closes as soon as the response headers are flushed, so the
	// grace timer below does not keep a goroutine alive after streaming begins.
	streamStarted := make(chan struct{})
	var streamStartedOnce sync.Once
	markStreamStarted := func() { streamStartedOnce.Do(func() { close(streamStarted) }) }

	var graceWG sync.WaitGroup
	stopGrace := func() {
		markStreamStarted()
		graceWG.Wait()
	}

	var pinger *streamPinger
	var pingerMu sync.Mutex
	keepaliveStopped := false
	stopKeepalive := func() {
		pingerMu.Lock()
		keepaliveStopped = true
		p := pinger
		pinger = nil
		pingerMu.Unlock()
		p.Stop()
	}
	defer func() {
		streamMu.Lock()
		handlerDone.Store(true)
		streamMu.Unlock()
		stopGrace()
		stopKeepalive()
	}()

	startStreamWith := func(early bool, attempt, upstreamStatus int, retryReason string) {
		streamMu.Lock()
		if headersSent || ctx.Err() != nil || handlerDone.Load() {
			streamMu.Unlock()
			return
		}
		target := activeTarget.Load()
		setGSBHeaders(w, false, target.accountID, target.modelName, attempt, upstreamStatus, retryReason)
		setGSBPerfHeaders(w, false, computePerfTelemetry(cacheUsed, preDispatchMs.Load(), firstUpstreamFrameMs.Load(), ttftMs.Load(), time.Since(reqStartedAt).Milliseconds(), nil, 0))
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
		baseResp := map[string]any{
			"id":         respID,
			"object":     "response",
			"created_at": created,
			"model":      target.modelName,
			"status":     "in_progress",
			"output":     []any{},
			"usage":      nil,
		}
		emitSSELocked("response.created", map[string]any{"response": baseResp})
		emitSSELocked("response.in_progress", map[string]any{"response": baseResp})
		streamMu.Unlock()

		pingerMu.Lock()
		if pinger == nil && !keepaliveStopped && !handlerDone.Load() {
			pinger = startStreamPinger(&streamMu, w, flusher, defaultKeepaliveInterval, nil)
		}
		pingerMu.Unlock()
	}

	// startStream reads the caller-owned attempt diagnostics, so it must only be
	// called from the request goroutine.
	startStream := func(early bool) {
		startStreamWith(early, curAttempt, lastUpstreamStatus, lastRetryReason)
	}

	if stream && cfg.EarlySSE && (eagerStream || !cfg.RejectDowngrade) {
		startStream(false)
	} else if stream && cfg.EarlySSEGrace > 0 {
		graceDur := cfg.EarlySSEGrace
		graceWG.Add(1)
		go func() {
			defer graceWG.Done()
			timer := time.NewTimer(graceDur)
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
			reasoningIndex = nextOutputIndex
			nextOutputIndex++
			reasoningAdded = true
			emitSSE("response.output_item.added", map[string]any{
				"output_index": reasoningIndex,
				"item": map[string]any{
					"id":      rsID,
					"type":    "reasoning",
					"summary": []any{},
				},
			})
			emitSSE("response.reasoning_summary_part.added", map[string]any{
				"item_id":       rsID,
				"output_index":  reasoningIndex,
				"summary_index": 0,
				"part": map[string]any{
					"type": "summary_text",
					"text": "",
				},
			})
		}
		streamedReasoning.WriteString(delta)
		emitSSE("response.reasoning_summary_text.delta", map[string]any{
			"item_id":       rsID,
			"output_index":  reasoningIndex,
			"summary_index": 0,
			"delta":         delta,
		})
	}

	emitTextDelta := func(delta string) {
		startStream(false)
		if !messageAdded {
			messageAdded = true
			messageIndex = nextOutputIndex
			nextOutputIndex++
			emitSSE("response.output_item.added", map[string]any{
				"output_index": messageIndex,
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
				"output_index":  messageIndex,
				"content_index": 0,
				"part": map[string]any{
					"type":        "output_text",
					"text":        "",
					"annotations": []any{},
				},
			})
		}
		if delta == "" {
			return
		}
		markMeaningfulOutput(&firstContentMs)
		emitSSE("response.output_text.delta", map[string]any{
			"item_id":       msgID,
			"output_index":  messageIndex,
			"content_index": 0,
			"delta":         delta,
		})
	}

	account.mu.RLock()
	originGroup := account.AccountGroup
	account.mu.RUnlock()

	maxAttempts := cfg.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	maxUpstream := cfg.MaxUpstreamAttempts
	if maxUpstream < 1 {
		maxUpstream = maxAttempts
	}
	upstreamCalls := 0
	var retryDeadline time.Time
	if cfg.RetryBudget > 0 {
		retryDeadline = reqStartedAt.Add(cfg.RetryBudget)
	}

	triedAccounts := map[string]bool{account.ID: true}
	var stallTracker requestStallTracker
	errorReplyRetried := false
	freshSocketNext := false
	rebootstrapNext := false
	serverMediaRounds := 0
	for attempt := 0; attempt < maxAttempts; attempt++ {
		curAttempt = attempt
		var limitFinish, limitedText string
		attemptStartedAt := time.Now()
		attemptReasoningStarted := false
		emitAttemptReasoning := func(delta string) {
			if delta == "" {
				return
			}
			if !attemptReasoningStarted && streamedReasoning.Len() > 0 {
				delta = "\n\n" + delta
			}
			attemptReasoningStarted = true
			emitReasoningDelta(delta)
		}
		var (
			candidateID           string
			selected              bool
			done                  bool
			text, thoughts        string
			sentText, sentThought string
			lastActivity          string
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
			if ansLabel != "" && (ansID != "" || account.Guest) {
				servedLabel = ansLabel
			}
			if ansID == "" {
				return
			}
			if servedID != "" && servedID != ansID {
				identityConflict = true
			}
			servedID = ansID
		}
		c := s.clientFor(account)
		opts.FreshSocket = freshSocketNext
		if rebootstrapNext && !account.Guest {
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
		var releaseStream func()
		if uploadErr != nil {
			streamErr = uploadErr
		} else {
			releaseStream, streamErr = s.beginAuxStream(account)
		}
		if streamErr == nil {
			upstreamCalls++
			preDispatchMs.Store(time.Since(reqStartedAt).Milliseconds())
			firstUpstreamFrameMs.Store(-1)
			upstream, streamErr = c.StreamGenerateLive(ctx, curPrompt, opts, func(update *gemini.LineParseResult) error {
				if account.Guest && update.MediaToolActive {
					return transport.NewUpstreamError("guest-free does not support media generation", 400, "unsupported_guest_feature", nil, nil)
				}
				if len(update.Frames) > 0 || update.AnsweredModelID != "" || strings.TrimSpace(update.Activity) != "" {
					markPhase(&firstUpstreamFrameMs)
				}
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
				earlyMismatch := shouldAbortForModelRejection(cfg, mediaToolUsed, *model, servedID, servedLabel)
				identityVerifiedNow := canStreamVerifiedWithConfig(cfg, eagerStream, identityConflict, *model, servedID, servedLabel)
				attemptStateMu.Unlock()
				if earlyMismatch {
					return transport.ErrStopStreaming
				}
				if stream && cfg.LiveThoughts && ctx.Err() == nil && !identityConflict {
					if activity := strings.TrimSpace(toolbridge.StripToolGuard(update.Activity)); activity != "" && activity != lastActivity {
						prefix := ""
						if sentThought != "" {
							prefix = "\n\n"
						}
						emitAttemptReasoning(prefix + activity + "\n\n")
						lastActivity = activity
					}
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
					if account.Guest && (len(frame.GeneratedImages) > 0 || len(frame.GeneratedVideos) > 0) {
						return transport.NewUpstreamError("guest-free does not support media generation", 400, "unsupported_guest_feature", nil, nil)
					}
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
					thoughts = toolbridge.StripToolGuard(mergeStreamedThoughts(thoughts, frame.Thoughts))
					done = done || frame.Done
					frameMismatch := shouldAbortForModelRejection(cfg, mediaToolUsed, *model, servedID, servedLabel)
					attemptStateMu.Unlock()
					if frameMismatch {
						return transport.ErrStopStreaming
					}
					if stream && cfg.LiveThoughts && ctx.Err() == nil && !done && !identityConflict && len(thoughts) > len(sentThought) && strings.HasPrefix(thoughts, sentThought) {
						emitAttemptReasoning(thoughts[len(sentThought):])
						sentThought = thoughts
					}

					if stream && !buffered && prep.limits.streamable() && limitFinish == "" && ctx.Err() == nil {
						clean := toolbridge.StripToolGuard(text)
						if !hasCompatibleStreamPrefix(clean, sentText) || (cfg.LiveThoughts && !strings.HasPrefix(thoughts, sentThought)) {
							return transport.NewUpstreamError("upstream revised an already streamed answer", 502, "upstream_revision", nil, nil)
						}
						if hasTools && sentText == "" {
							if toolbridge.IsToolProtocolRefusal(clean) {
								return transport.NewUpstreamError(toolbridge.RefusalErrorText, 502, "tool_call_error", nil, nil)
							}
							if len(clean) <= toolbridge.RefusalWindowBytes {
								continue
							}
						}
						if done && sentText == "" && gemini.GeminiErrorReply(clean) != "" {
							continue
						}
						if len(clean) > len(sentText) && gemini.GeminiErrorReply(clean) == "" {
							visible := clean
							if !hasTools {
								var hit string
								visible, hit = prep.limits.visibleAfter(clean, len(sentText))
								if hit != "" {
									limitFinish, limitedText = hit, visible
								}
							}
							if tail, ok := remainingStreamTail(visible, sentText); ok && tail != "" {
								emitTextDelta(tail)
								sentText = visible
							}
						}
					} else if stream && buffered && limitFinish == "" && ctx.Err() == nil && !done {
						clean := toolbridge.StripToolGuard(text)
						if sentText != "" && !hasCompatibleStreamPrefix(clean, sentText) {
							return transport.NewUpstreamError("upstream revised an already streamed answer", 502, "upstream_revision", nil, nil)
						}
						// Answer text still waits for model verification in strict mode.
						identityVerified := canStreamVerifiedWithConfig(cfg, eagerStream, identityConflict, *model, servedID, servedLabel)
						if identityVerified && cfg.LiveThoughts {
							if hasTools && !cfg.KeepToolPreamble && sentText == "" {
								if pre := toolbridge.ExtractPreToolPreamble(text); pre != "" {
									thoughts = appendPreambleToThoughts(thoughts, pre)
								}
							}
							if len(thoughts) > len(sentThought) && strings.HasPrefix(thoughts, sentThought) {
								emitAttemptReasoning(thoughts[len(sentThought):])
								sentThought = thoughts
							} else if servedID != "" && time.Since(attemptStartedAt) >= 5*time.Second {
								startStream(false)
							}
						}
						if identityVerified && prep.limits.streamable() {
							if hasTools && sentText == "" && toolbridge.IsToolProtocolRefusal(clean) {
								return transport.NewUpstreamError(toolbridge.RefusalErrorText, 502, "tool_call_error", nil, nil)
							}
							if !hasTools {
								visible, hit := prep.limits.visibleAfter(clean, len(sentText))
								if hit != "" {
									limitFinish, limitedText = hit, visible
								}
								if len(visible) > 240 && gemini.GeminiErrorReply(clean) == "" {
									if tail, ok := remainingStreamTail(visible, sentText); ok && tail != "" {
										emitTextDelta(tail)
										sentText = visible
									}
								}
							} else if !mediaToolUsed && len(clean) > 240 && !toolbridge.IsToolProtocolRefusal(clean) && gemini.GeminiErrorReply(clean) == "" {
								if safePrefix := toolbridge.SafeStreamablePrefix(clean); len(safePrefix) > len(sentText) {
									if tail, ok := remainingStreamTail(safePrefix, sentText); ok && tail != "" {
										emitTextDelta(tail)
										sentText = safePrefix
									}
								}
							}
						}
					}
				}
				return nil
			})
			releaseStream()
		}
		if upstream != nil && upstream.Status > 0 {
			lastUpstreamStatus = upstream.Status
		}
		if upstream != nil {
			grounding = gemini.MergeGroundingSources(grounding, upstream.GroundingSources)
		}

		rawText := text
		evalText, evalSent := text, sentText
		if limitFinish != "" && !hasTools {
			evalText = limitedText
			if sentText != "" {
				evalSent = limitedText
			}
			if evalText == "" && rawText != "" {
				evalText = " "
			}
		}
		interpreted, runCheck := s.evaluateAttemptCompletion(
			ctx,
			cfg,
			account,
			*model,
			mid,
			upstream,
			streamErr,
			selected,
			done,
			evalText,
			evalSent,
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
			if uploadErr == nil && releaseStream == nil {
				// A local reservation rejection is not an upstream account failure.
			} else if errCode == "account_banned" {
				s.markAccountQuarantined(account, runCheck.Error())
			} else if stallTracker.recordError(s, account, errCode, runCheck.Error()) {
				if !stallTracker.poisoned {
					s.refreshQuotaAsync(account, c)
				}
			} else if errCode != "session_busy" && errCode != "client_cancelled" && errCode != "upstream_error_reply" && errCode != "tool_call_error" && errCode != "model_label_mismatch" {
				s.markAccountFailure(account, runCheck.Error())
				if errCode != "empty_upstream_response" {
					s.refreshQuotaAsync(account, c)
				}
			}
			if cacheKey != "" && len(opts.Metadata) > 0 && shouldEvictSessionCacheOnError(errCode, errStatus) {
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
				errCode == "upstream_transient" ||
				errCode == "upstream_degraded" ||
				(sentThought == "" && (errCode == "upstream_transport_error" ||
					errCode == "upstream_error" ||
					errCode == "upstream_http_error" ||
					errCode == "upstream_upload_error" ||
					errCode == "upstream_parse_error")) ||
				errCode == "empty_upstream_response" ||
				errCode == "incomplete_upstream_response" ||
				errCode == "session_busy" ||
				errCode == "upstream_auth_error" ||
				errCode == "auth_redirect" ||
				(len(opts.Metadata) > 0 && (errCode == "upstream_stalled" || errCode == "upstream_timeout")) ||
				(scope.AccountID == "" && (errCode == "account_disabled" ||
					errCode == "model_substitution" ||
					errCode == "model_label_mismatch" ||
					errStatus == 429 ||
					errCode == "rate_limit_error" ||
					errCode == "upstream_connect_timeout" ||
					errCode == "upstream_stalled" ||
					errCode == "upstream_timeout" ||
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
							assignAccount(nextAcct, nextModel)
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
							assignAccount(nextAcct, nextModel)
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
							assignAccount(nextAcct, nextModel)
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
					if !stallTracker.poisoned {
						if scope.AccountID == "" {
							if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, originGroup, requested, hasher); nextAcct != nil && nextModel != nil {
								triedAccounts[nextAcct.ID] = true
								assignAccount(nextAcct, nextModel)
								opts.Metadata = nil
								opts.Files = nil
								curPrompt = fullPrompt
								attachmentParts = fullAttachmentParts
								continue
							}
						}
						if len(opts.Metadata) > 0 {
							// The continuation itself stalled; retry once as a fresh
							// conversation even on a single or pinned account.
							opts.Metadata = nil
							opts.Files = nil
							curPrompt = fullPrompt
							attachmentParts = fullAttachmentParts
							continue
						}
					}
				}
				if errCode == "upstream_degraded" || errCode == "upstream_transient" {
					// Prefer another eligible account; a pinned request stays pinned.
					lastRetryReason = errCode
					if scope.AccountID == "" {
						if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, originGroup, requested, hasher); nextAcct != nil && nextModel != nil {
							triedAccounts[nextAcct.ID] = true
							assignAccount(nextAcct, nextModel)
							opts.Metadata = nil
							opts.Files = nil
							curPrompt = fullPrompt
							attachmentParts = fullAttachmentParts
							continue
						}
					}
					freshSocketNext = errCode == "upstream_degraded"
					rebootstrapNext = freshSocketNext
					opts.Metadata = nil
					opts.Files = nil
					curPrompt = fullPrompt
					attachmentParts = fullAttachmentParts
					continue
				}
				if errCode == "model_identity_unknown" || errCode == "model_identity_conflict" ||
					errCode == "model_label_mismatch" {
					lastRetryReason = errCode
					if scope.AccountID == "" {
						if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, originGroup, requested, hasher); nextAcct != nil && nextModel != nil {
							triedAccounts[nextAcct.ID] = true
							assignAccount(nextAcct, nextModel)
							opts.Metadata = nil
							opts.Files = nil
							curPrompt = fullPrompt
							attachmentParts = fullAttachmentParts
							continue
						}
					}
				}
				if errCode == "account_disabled" {
					lastRetryReason = errCode
					if scope.AccountID == "" {
						if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, originGroup, requested, hasher); nextAcct != nil && nextModel != nil {
							triedAccounts[nextAcct.ID] = true
							assignAccount(nextAcct, nextModel)
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
					errCode == "upstream_http_error" ||
					errCode == "upstream_upload_error" ||
					errCode == "upstream_parse_error" ||
					errCode == "incomplete_upstream_response" ||
					errCode == "tool_call_error" {
					lastRetryReason = errCode
					if errCode != "tool_call_error" && scope.AccountID == "" {
						if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, originGroup, requested, hasher); nextAcct != nil && nextModel != nil {
							triedAccounts[nextAcct.ID] = true
							assignAccount(nextAcct, nextModel)
							opts.Metadata = nil
							opts.Files = nil
							curPrompt = fullPrompt
							attachmentParts = fullAttachmentParts
							continue
						}
					}
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
					if !errorReplyRetried {
						errorReplyRetried = true
						freshSocketNext = true
						rebootstrapNext = true
						opts.Metadata = nil
						opts.Files = nil
						select {
						case <-time.After(100 * time.Millisecond):
						case <-ctx.Done():
						}
						if !account.Guest && len(fullPrompt) > 120000 {
							if compacted, extraOverflow, cErr := prompt.FlattenMessages(messages, toolsRaw, toolChoiceRaw, prompt.FlattenOptions{
								StartIndex:      0,
								MaxPromptChars:  100000,
								MaxOldToolChars: 12000,
								AllowOverflow:   cfg.HistoryOffload,
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
							assignAccount(nextAcct, nextModel)
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

			stopGrace()
			stopKeepalive()
			streamMu.Lock()
			sent := headersSent
			setGSBHeaders(w, sent, account.ID, mid, curAttempt, lastUpstreamStatus, lastRetryReason)
			streamMu.Unlock()
			totalMs := time.Since(reqStartedAt).Milliseconds()
			pt := computePerfTelemetry(cacheUsed, prepareMs+uploadMs.Load(), firstUpstreamFrameMs.Load(), ttftMs.Load(), totalMs, upstream, 0)
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
				"retry_reason":       lastRetryReason,
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
				"pre_request_ms":     pt.PreRequestMs,
				"bootstrap_ms":       pt.BootstrapMs,
				"upstream_ttfb_ms":   pt.UpstreamTTFBMs,
				"first_frame_ms":     pt.FirstFrameMs,
				"bridge_hold_ms":     pt.BridgeHoldMs,
				"bridge_overhead_ms": pt.BridgeOverheadMs,
				"gen_window_ms":      pt.GenWindowMs,
				"frame_count":        pt.FrameCount,
			})
			if ctx.Err() == nil {
				if sent {
					emitSSE("error", map[string]any{"code": errCode, "message": runCheck.Error(), "param": nil})
				} else {
					sendChatError(w, false, runCheck)
				}
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
				opts.Files = nil
				attachmentParts = fullAttachmentParts
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
				opts.Files = nil
				attachmentParts = fullAttachmentParts
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
			if (cfg.KeepToolPreamble || sentText != "") && interpreted.Text != "" {
				contentStr = interpreted.Text
			} else if cfg.LiveThoughts && interpreted != nil && strings.TrimSpace(interpreted.Text) != "" {
				thoughts = appendPreambleToThoughts(thoughts, interpreted.Text)
			}
		} else {
			contentStr = toolbridge.StripToolGuard(text)
			if sentText == "" && thoughts != "" {
				contentStr = stripLeakedThoughtsFromContent(contentStr, thoughts)
			}
			if limitFinish != "" {
				contentStr = limitedText
			} else {
				contentStr, limitFinish = prep.limits.apply(contentStr)
			}
			if prep.limits.wantJSON {
				contentStr = jsonObjectText(contentStr)
			}
		}
		contentStr = toolbridge.StripToolGuard(contentStr)
		thoughts = toolbridge.StripToolGuard(thoughts)
		responseStatus := "completed"
		var incompleteDetails any
		if len(calls) == 0 && limitFinish == "length" {
			responseStatus = "incomplete"
			incompleteDetails = map[string]any{"reason": "max_output_tokens"}
		}
		// Web-search citations are appended to the answer so the client sees the
		// sources the model actually used, in the order Google returned them.
		if cfg.IncludeGrounding && len(grounding) > 0 && len(calls) == 0 && !prep.limits.enabled() {
			contentStr = appendGroundingCitations(contentStr, grounding)
		}

		oaCalls := toolbridge.ToOpenAIToolCalls(calls)

		var chatUsage map[string]any
		if account.Guest {
			chatUsage = prompt.BuildUsage(curPrompt, text, thoughts)
			chatUsage["estimated"] = true
		} else {
			chatUsage = s.computeChatUsage(fullPrompt, curPrompt, overflowParts, messages, toolsRaw, toolChoiceRaw, contentStr, thoughts)
		}
		respUsage := buildResponsesUsage(chatUsage)
		s.clearModelExhausted(account, model.ModelName)
		if !opts.Temporary {
			var cachedContent any = contentStr
			if len(oaCalls) > 0 && contentStr == "" {
				cachedContent = nil
			}
			s.saveSessionContinuation(account.ID, mid, messages, toolsRaw, toolChoiceRaw, cachedContent, oaCalls, lastCID, lastRID, candidateID, lastContextToken)
		}

		stopGrace()
		stopKeepalive()
		if ctx.Err() != nil {
			return
		}

		reasoningText := thoughts
		if stream && cfg.LiveThoughts {
			if tail, ok := remainingStreamTail(thoughts, sentThought); ok {
				emitAttemptReasoning(tail)
			}
			reasoningText = streamedReasoning.String()
		}
		var outputItems []map[string]any
		if cfg.LiveThoughts && reasoningText != "" {
			outputItems = append(outputItems, map[string]any{
				"id":   rsID,
				"type": "reasoning",
				"summary": []map[string]any{
					{
						"type": "summary_text",
						"text": reasoningText,
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
			if reasoningAdded {
				emitSSE("response.reasoning_summary_text.done", map[string]any{
					"item_id":       rsID,
					"output_index":  reasoningIndex,
					"summary_index": 0,
					"text":          reasoningText,
				})
				emitSSE("response.reasoning_summary_part.done", map[string]any{
					"item_id":       rsID,
					"output_index":  reasoningIndex,
					"summary_index": 0,
					"part": map[string]any{
						"type": "summary_text",
						"text": reasoningText,
					},
				})
				emitSSE("response.output_item.done", map[string]any{
					"output_index": reasoningIndex,
					"item":         outputItems[0],
				})
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
					"output_index":  messageIndex,
					"content_index": 0,
					"text":          contentStr,
				})
				emitSSE("response.content_part.done", map[string]any{
					"item_id":       msgID,
					"output_index":  messageIndex,
					"content_index": 0,
					"part": map[string]any{
						"type":        "output_text",
						"text":        contentStr,
						"annotations": []any{},
					},
				})
				emitSSE("response.output_item.done", map[string]any{
					"output_index": messageIndex,
					"item":         msgItem,
				})
				if reasoningAdded && messageIndex < reasoningIndex {
					outputItems[0], outputItems[1] = outputItems[1], outputItems[0]
				}
			}

			for idx, tc := range oaCalls {
				fcID := fcIDs[idx]
				fcOutIdx := nextOutputIndex + idx
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

		}
		payload := map[string]any{
			"id":         respID,
			"object":     "response",
			"created_at": created,
			"status":     responseStatus,
			"model":      mid,
			"output":     outputItems,
			"usage":      respUsage,
		}
		if account.Guest && (servedID != "" || servedLabel != "") {
			payload["requested_model"] = mid
			payload["model_served"] = map[string]any{"id": servedID, "label": servedLabel}
		}
		if incompleteDetails != nil {
			payload["incomplete_details"] = incompleteDetails
		}
		outTok, _ := chatUsage["completion_tokens"].(int)
		totalMs := time.Since(reqStartedAt).Milliseconds()
		pt := computePerfTelemetry(cacheUsed, preDispatchMs.Load(), firstUpstreamFrameMs.Load(), ttftMs.Load(), totalMs, upstream, outTok)
		if stream {
			emitSSE("response."+responseStatus, map[string]any{"response": payload})
		} else {
			setGSBHeaders(w, headersSent, account.ID, mid, curAttempt, lastUpstreamStatus, lastRetryReason)
			setGSBPerfHeaders(w, headersSent, pt)
			markPhase(&headerMs)
			sendJSON(w, 200, payload, nil)
		}

		cachedTok := 0
		if d, ok := chatUsage["prompt_tokens_details"].(map[string]any); ok {
			cachedTok, _ = d["cached_tokens"].(int)
		}
		s.logRequestJSON(map[string]any{
			"req_id":                respID,
			"endpoint":              "/v1/responses",
			"model":                 mid,
			"account":               account.ID,
			"effort":                effortStr,
			"prompt_bytes":          len(curPrompt),
			"header_bytes":          flattenStats.HeaderBytes,
			"body_bytes":            flattenStats.BodyBytes,
			"footer_bytes":          flattenStats.FooterBytes,
			"tool_bytes":            flattenStats.ToolSchemaBytes,
			"ttft_ms":               ttftMs.Load(),
			"total_ms":              totalMs,
			"status":                200,
			"error_code":            "",
			"cached_tokens":         cachedTok,
			"attempts":              curAttempt + 1,
			"retry_reason":          lastRetryReason,
			"prepare_ms":            prepareMs,
			"cache_hit":             cacheUsed,
			"cache_prefix_split":    prep.cacheSplit,
			"upload_ms":             uploadMs.Load(),
			"identity_ms":           identityMs.Load(),
			"header_ms":             headerMs.Load(),
			"first_reasoning_ms":    firstReasoningMs.Load(),
			"first_content_ms":      firstContentMs.Load(),
			"first_tool_ms":         firstToolMs.Load(),
			"done_ms":               totalMs,
			"account_switches":      accountSwitches.Load(),
			"requested_model":       mid,
			"served_model":          servedID,
			"served_label":          servedLabel,
			"label_mismatch":        gemini.ServedLabelMismatch(*model, servedID, servedLabel),
			"pre_request_ms":        pt.PreRequestMs,
			"bootstrap_ms":          pt.BootstrapMs,
			"upstream_ttfb_ms":      pt.UpstreamTTFBMs,
			"first_frame_ms":        pt.FirstFrameMs,
			"bridge_hold_ms":        pt.BridgeHoldMs,
			"bridge_overhead_ms":    pt.BridgeOverheadMs,
			"gen_window_ms":         pt.GenWindowMs,
			"frame_count":           pt.FrameCount,
			"output_tokens":         pt.OutputTokens,
			"sustained_tok_per_sec": pt.SustainedTokPerSec,
			"e2e_tok_per_sec":       pt.E2ETokPerSec,
		})

		account.mu.Lock()
		account.Fails = 0
		account.LastFail = time.Time{}
		account.LastError = ""
		account.ModelsError = nil
		account.LastUsed = s.now()
		account.mu.Unlock()
		s.refreshQuotaAsync(account, c)
		return
	}
}
