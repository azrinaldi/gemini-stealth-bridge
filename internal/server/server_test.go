package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

var (
	testModel = gemini.ModelInfo{
		ModelName:   "gemini-pro",
		ModelID:     "aaaaaaaaaaaaaaaa",
		DisplayName: "Pro",
		Capacity:    2,
		ModelNumber: 1,
	}
	lowerModelID = "bbbbbbbbbbbbbbbb"
)

type mockUpstream struct {
	streamFn func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error)
	modelsFn func(ctx context.Context, force bool) (*gemini.AvailableModelsResult, error)
	quotaFn  func(ctx context.Context) []gemini.QuotaBucket
	uploadFn func(ctx context.Context, data []byte, filename, contentType string) (gemini.UploadedFile, error)
	bootFn   func(ctx context.Context) error
}

func (m *mockUpstream) Bootstrap(ctx context.Context) error {
	if m.bootFn != nil {
		return m.bootFn(ctx)
	}
	return nil
}

func (m *mockUpstream) AvailableModels(ctx context.Context, force bool) (*gemini.AvailableModelsResult, error) {
	if m.modelsFn != nil {
		return m.modelsFn(ctx, force)
	}
	return &gemini.AvailableModelsResult{
		Status:   200,
		Models:   []gemini.ModelInfo{testModel},
		Capacity: 2,
	}, nil
}

func (m *mockUpstream) GetQuota(ctx context.Context) []gemini.QuotaBucket {
	if m.quotaFn != nil {
		return m.quotaFn(ctx)
	}
	return nil
}

func (m *mockUpstream) UploadFile(ctx context.Context, data []byte, filename, contentType string) (gemini.UploadedFile, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, data, filename, contentType)
	}
	return gemini.UploadedFile{Path: "/contrib_service/ttl_1d/" + filename, Filename: filename}, nil
}

func (m *mockUpstream) StreamGenerateLive(ctx context.Context, promptText string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
	return m.streamFn(ctx, promptText, opts, onFrame)
}

func (m *mockUpstream) RotateCookies(ctx context.Context) (*cookies.RotateResult, error) {
	return &cookies.RotateResult{Status: 200, Rotated: false}, nil
}

func (m *mockUpstream) ReloadFromDiskIfNewer() bool { return false }
func (m *mockUpstream) InvalidateAuth()             {}

func newTestHarness(relaxed, sessionCache bool) (*Server, *AccountState) {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.AccountTokens = map[string]string{"audit": "acct"}
	cfg.RejectDowngrade = !relaxed
	cfg.SessionCache = sessionCache
	cfg.MaxPromptChars = 850000
	cfg.MaxOldToolChars = 32000

	srv := New(cfg)
	acct := &AccountState{
		ID:       "acct",
		Email:    "acct@example.com",
		Models:   []gemini.ModelInfo{testModel},
		Capacity: 2,
		ModelsAt: time.Now(),
	}
	srv.SetAccounts([]*AccountState{acct})
	return srv, acct
}

func runChat(t *testing.T, srv *Server, acct *AccountState, client UpstreamClient, extra map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	acct.mu.Lock()
	acct.Client = client
	acct.mu.Unlock()
	body := map[string]any{
		"model": testModel.ModelName,
		"messages": []any{
			map[string]any{"role": "user", "content": "synthetic"},
		},
	}
	for k, v := range extra {
		body[k] = v
	}
	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, body, AuthScope{OK: true, AccountID: "acct"})
	return rec
}

func makeFrame(text, thoughts string, done bool, ansID string) gemini.Frame {
	return gemini.Frame{
		RCID:            "candidate",
		Text:            text,
		Thoughts:        thoughts,
		Done:            done,
		AnsweredModelID: ansID,
	}
}

func clientFromFrames(frames []gemini.Frame) *mockUpstream {
	return &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			for _, f := range frames {
				if err := onFrame(&gemini.LineParseResult{
					CID:             "synthetic-cid",
					RID:             "synthetic-rid",
					AnsweredModelID: f.AnsweredModelID,
					Frames:          []gemini.Frame{f},
				}); err != nil {
					return nil, err
				}
			}
			return &gemini.StreamResponse{Status: 200, CID: "synthetic-cid", RID: "synthetic-rid"}, nil
		},
	}
}

func parseSSEEvents(output string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "data: {") {
			var obj map[string]any
			if err := json.Unmarshal([]byte(line[6:]), &obj); err == nil {
				out = append(out, obj)
			}
		}
	}
	return out
}

func TestStrictStreamingRejectsModelSubstitution(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	client := clientFromFrames([]gemini.Frame{
		makeFrame("```json\n{\"tool_calls\":[{\"name\":\"read\"}]}\n```", "", false, ""),
		makeFrame("```json\n{\"tool_calls\":[{\"name\":\"read\"}]}\n```", "", true, lowerModelID),
	})
	rec := runChat(t, srv, acct, client, map[string]any{"stream": true})
	if rec.Code != 429 {
		t.Fatalf("expected 429 on model substitution even when code block starts on frame 1, got %d (%s)", rec.Code, rec.Body.String())
	}
	var payload map[string]map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	if payload["error"]["code"] != "model_substitution" {
		t.Fatalf("expected model_substitution code, got %+v", payload)
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("expected no SSE data emitted before rejection, got %s", rec.Body.String())
	}
	// Subsequent request on the same account for the same model must fail pre-flight with 429
	rec2 := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame("ok", "", true, testModel.ModelID),
	}), map[string]any{"stream": true})
	if rec2.Code != 429 {
		t.Fatalf("expected pre-flight 429 after recorded model substitution, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestStrictStreamingFailsClosedWhenIdentityAbsent(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	client := clientFromFrames([]gemini.Frame{
		makeFrame("answer", "", true, ""),
	})
	rec := runChat(t, srv, acct, client, map[string]any{"stream": true})
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "model_identity_unknown") {
		t.Fatalf("expected 502 model_identity_unknown, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRelaxedPartialStreamingEndsWithErrorAndNoDone(t *testing.T) {
	srv, acct := newTestHarness(true, false)
	client := clientFromFrames([]gemini.Frame{
		makeFrame("partial", "", false, testModel.ModelID),
	})
	rec := runChat(t, srv, acct, client, map[string]any{"stream": true})
	if rec.Code != 200 {
		t.Fatalf("expected 200 SSE stream, got %d", rec.Code)
	}
	evts := parseSSEEvents(rec.Body.String())
	if len(evts) == 0 {
		t.Fatalf("expected SSE events, got %s", rec.Body.String())
	}
	last := evts[len(evts)-1]
	errObj, _ := last["error"].(map[string]any)
	if errObj["code"] != "incomplete_upstream_response" {
		t.Fatalf("expected incomplete_upstream_response at end of stream, got %+v", last)
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatal("expected no [DONE] terminator on incomplete stream")
	}
}

func Test429StatusAndCooldownHeadersSurvive(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	calls := 0
	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			calls++
			return &gemini.StreamResponse{
				Status: 429,
				Headers: map[string]string{
					"retry-after": "3600",
					"x-resets-at": "2099-01-01T00:00:00Z",
				},
			}, nil
		},
	}
	rec := runChat(t, srv, acct, client, nil)
	if rec.Code != 429 || calls != 1 {
		t.Fatalf("expected single 429 call, got status=%d calls=%d", rec.Code, calls)
	}
	if rec.Header().Get("retry-after") != "3600" || rec.Header().Get("x-resets-at") != "2099-01-01T00:00:00Z" {
		t.Fatalf("expected cooldown headers preserved, got %+v", rec.Header())
	}
}

func TestPoolQuotaErrorSummarizesUnavailableAccounts(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.AccountTokens = nil
	cfg.QuotaPreflight = "strict"
	cfg.MaxConcurrentPerAccount = 0
	cfg.MaxRPMPerAccount = 0
	srv := New(cfg)
	resetAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	quota := []gemini.QuotaBucket{{
		ID:        "gemini-pro",
		Label:     "Gemini Pro",
		Total:     100,
		Remaining: 0,
		ResetAt:   resetAt,
	}}
	a1 := &AccountState{ID: "acct1", Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now(), Quota: quota}
	a2 := &AccountState{ID: "acct2", Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now(), Quota: quota}
	srv.SetAccounts([]*AccountState{a1, a2})
	body := map[string]any{
		"model":    testModel.ModelName,
		"messages": []any{map[string]any{"role": "user", "content": "synthetic"}},
	}

	poolRec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), poolRec, body, AuthScope{OK: true})
	if poolRec.Code != 429 {
		t.Fatalf("expected pool quota error, got %d: %s", poolRec.Code, poolRec.Body.String())
	}
	poolBody := poolRec.Body.String()
	if !strings.Contains(poolBody, "no pool account can serve gemini-pro") || !strings.Contains(poolBody, "exhausted=2") {
		t.Fatalf("expected aggregate pool availability details, got %s", poolBody)
	}
	if strings.Contains(poolBody, "on account acct1") {
		t.Fatalf("pool error leaked a single fallback account: %s", poolBody)
	}

	pinnedRec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), pinnedRec, body, AuthScope{OK: true, AccountID: "acct1"})
	if pinnedRec.Code != 429 || !strings.Contains(pinnedRec.Body.String(), "on account acct1") {
		t.Fatalf("expected pinned account error to remain account-specific, got %d: %s", pinnedRec.Code, pinnedRec.Body.String())
	}
}

func TestApologyDetectionAndGenuineQuotes(t *testing.T) {
	srv, acct := newTestHarness(false, false)

	// 1. Short apology -> 502 upstream_error_reply
	rec1 := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame("Sorry, something went wrong. Please try your request again.", "", true, testModel.ModelID),
	}), nil)
	if rec1.Code != 502 || !strings.Contains(rec1.Body.String(), "upstream_error_reply") {
		t.Fatalf("expected 502 upstream_error_reply, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// 2. Genuine answer quoting the apology -> 200 OK
	quote := `The upstream returned "Sorry, something went wrong. Please try your request again." earlier, so I retried the request and it succeeded.`
	rec2 := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame(quote, "", true, testModel.ModelID),
	}), nil)
	if rec2.Code != 200 || !strings.Contains(rec2.Body.String(), "retried the request") {
		t.Fatalf("expected 200 OK for quoted apology, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestThinkingLevelMappingAndToolCalling(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	var seenLevels []int
	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			seenLevels = append(seenLevels, opts.ThinkingLevel)
			_ = onFrame(&gemini.LineParseResult{
				CID:    "c1",
				RID:    "r1",
				Frames: []gemini.Frame{makeFrame(`{"tool_calls":[{"name":"read","arguments":{"path":"/tmp/x"}}]}`, "thinking", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}

	for _, eff := range []any{"low", "medium", "high", nil} {
		extra := map[string]any{}
		if eff != nil {
			extra["reasoning_effort"] = eff
		}
		_ = runChat(t, srv, acct, client, extra)
	}
	expected := []int{1, 4, 2, 1}
	for i := range expected {
		if seenLevels[i] != expected[i] {
			t.Fatalf("expected thinking levels %v, got %v", expected, seenLevels)
		}
	}

	// Tool call validation + preamble suppression
	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			},
		},
	}
	rec := runChat(t, srv, acct, client, map[string]any{"tools": tools, "tool_choice": "required"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected 200 tool_calls completion, got %d: %s", rec.Code, rec.Body.String())
	}
}

func Test1MContextOverflowOffloadingAndSessionCache(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	var uploaded []string
	var capturedPrompt string
	var capturedFiles []gemini.UploadedFile

	client := &mockUpstream{
		uploadFn: func(ctx context.Context, data []byte, filename, contentType string) (gemini.UploadedFile, error) {
			if contentType != "text/plain" || len(data) > 340000 {
				t.Errorf("unexpected overflow attachment %s (%s, %d bytes)", filename, contentType, len(data))
			}
			uploaded = append(uploaded, string(data))
			return gemini.UploadedFile{Path: "/contrib_service/ttl_1d/" + filename, Filename: filename}, nil
		},
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			capturedPrompt = prompt
			capturedFiles = opts.Files
			_ = onFrame(&gemini.LineParseResult{
				CID:          "c_turn1",
				RID:          "r_turn1",
				ContextToken: "ctx_1",
				Frames:       []gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}

	var msgs []any
	msgs = append(msgs, map[string]any{"role": "user", "content": "INITIAL_ANCHOR"})
	for i := range 60 {
		msgs = append(msgs, map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{
				map[string]any{
					"id":   fmt.Sprintf("call_%d", i),
					"type": "function",
					"function": map[string]any{
						"name":      "read",
						"arguments": fmt.Sprintf(`{"path":"file_%d.go"}`, i),
					},
				},
			},
		})
		msgs = append(msgs, map[string]any{
			"role":         "tool",
			"tool_call_id": fmt.Sprintf("call_%d", i),
			"content":      fmt.Sprintf("OLD_VERBATIM_%d_", i) + strings.Repeat("x", 20000) + fmt.Sprintf("_END_%d", i),
		})
	}
	msgs = append(msgs, map[string]any{
		"role":    "assistant",
		"content": nil,
		"tool_calls": []any{
			map[string]any{
				"id":       "call_latest",
				"type":     "function",
				"function": map[string]any{"name": "read", "arguments": `{"path":"latest.go"}`},
			},
		},
	})
	recentPayload := "RECENT_INTACT_" + strings.Repeat("y", 28000) + "_TAIL"
	msgs = append(msgs, map[string]any{
		"role":         "tool",
		"tool_call_id": "call_latest",
		"content":      recentPayload,
	})

	rec := runChat(t, srv, acct, client, map[string]any{"messages": msgs})
	if rec.Code != 200 {
		t.Fatalf("expected 200 OK on 1.3MB transcript, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(capturedPrompt) > 850000 {
		t.Fatalf("expected inline prompt <= 850KB, got %d", len(capturedPrompt))
	}
	if len(uploaded) < 2 || len(capturedFiles) != len(uploaded) {
		t.Fatalf("expected >= 2 uploaded overflow files, got %d", len(uploaded))
	}
	if !strings.Contains(uploaded[0], "OLD_VERBATIM_0_"+strings.Repeat("x", 20000)+"_END_0") {
		t.Fatal("expected first tool output preserved verbatim inside conversation_history_part_1.txt")
	}
	if !strings.Contains(capturedPrompt, recentPayload) {
		t.Fatal("expected recent tool output preserved intact inline")
	}
}

func TestHTTPHandlerEndpoints(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	acct.Client = clientFromFrames([]gemini.Frame{
		makeFrame("hello", "", true, testModel.ModelID),
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"gemini-stealth-bridge"`) {
		t.Fatalf("unexpected /health response: %d %s", rec.Code, rec.Body.String())
	}

	reqModels := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	reqModels.Header.Set("Authorization", "Bearer audit")
	recModels := httptest.NewRecorder()
	srv.ServeHTTP(recModels, reqModels)
	if recModels.Code != 200 || !strings.Contains(recModels.Body.String(), `"gemini-pro"`) {
		t.Fatalf("unexpected /v1/models response: %d %s", recModels.Code, recModels.Body.String())
	}
}

func TestGsbAuthTokensPrefixModelResolutionAndBudgetThinking(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = t.TempDir()
	cfg.AccountTokens = nil // test automatic gsb-<id> and raw <id> registration
	srv := New(cfg)
	t.Cleanup(func() { srv.FlushSessionCacheNow() })

	var capturedLevel int
	mock := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			capturedLevel = opts.ThinkingLevel
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c1",
				RID:             "r1",
				AnsweredModelID: testModel.ModelID,
				Frames:          []gemini.Frame{makeFrame("pong", "thought", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
		},
	}
	acct := &AccountState{
		ID:       "acc1",
		Email:    "acc1@example.com",
		Client:   mock,
		Models:   []gemini.ModelInfo{testModel},
		Capacity: 2,
		ModelsAt: time.Now(),
	}
	srv.SetAccounts([]*AccountState{acct})

	// 1. Auth via gsb-<id>, raw <id>, x-api-key, and model prefix gsb/ + @think=high
	for _, token := range []string{"gsb-acc1", "acc1"} {
		reqBody := `{"model":"gsb/gemini-pro@think=high","messages":[{"role":"user","content":"ping"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
		req.Header.Set("x-api-key", token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("token %q failed with %d: %s", token, rec.Code, rec.Body.String())
		}
		if capturedLevel != 2 {
			t.Fatalf("expected @think=high -> ThinkingLevel 2, got %d", capturedLevel)
		}
	}

	// 2. Thinking budget_tokens -> medium (4)
	reqBudget := `{"model":"gsb/gemini-pro","thinking":{"type":"enabled","budget_tokens":10000},"messages":[{"role":"user","content":"ping"}]}`
	reqB := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBudget))
	reqB.Header.Set("Authorization", "Bearer gsb-acc1")
	recB := httptest.NewRecorder()
	srv.ServeHTTP(recB, reqB)
	if recB.Code != 200 || capturedLevel != 4 {
		t.Fatalf("expected budget_tokens 10000 -> ThinkingLevel 4 (medium), got status=%d level=%d", recB.Code, capturedLevel)
	}
}

func TestResponsesEndpointStreamingAndNonStreaming(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	acct.mu.Lock()
	acct.Client = clientFromFrames([]gemini.Frame{
		makeFrame("The answer is 42.", "Calculating 6 * 7", true, testModel.ModelID),
	})
	acct.mu.Unlock()
	// 1. Non-streaming POST /v1/responses
	reqBody := `{
		"model": "gsb/gemini-pro",
		"instructions": "Be concise.",
		"reasoning": {"effort": "high"},
		"input": "What is 6 * 7?"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer audit")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST /v1/responses non-stream failed: %d %s", rec.Code, rec.Body.String())
	}
	var respObj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &respObj); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if respObj["object"] != "response" || respObj["status"] != "completed" {
		t.Fatalf("unexpected response envelope: %+v", respObj)
	}
	outputArr, _ := respObj["output"].([]any)
	if len(outputArr) != 2 {
		t.Fatalf("expected 2 output items (reasoning + message), got %d: %+v", len(outputArr), outputArr)
	}
	// 2. Streaming POST /v1/responses with tool call
	acct.mu.Lock()
	acct.Client = clientFromFrames([]gemini.Frame{
		makeFrame("```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/etc/hostname\"}}]}\n```", "Need to read /etc/hostname", true, testModel.ModelID),
	})
	acct.mu.Unlock()
	streamReqBody := `{
		"model": "gsb/gemini-pro",
		"stream": true,
		"tools": [{
			"type": "function",
			"name": "read_file",
			"description": "Read a file",
			"parameters": {
				"type": "object",
				"properties": {"path": {"type": "string"}},
				"required": ["path"]
			}
		}],
		"input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "Read /etc/hostname"}]}]
	}`
	sReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(streamReqBody))
	sReq.Header.Set("Authorization", "Bearer audit")
	sRec := httptest.NewRecorder()
	srv.ServeHTTP(sRec, sReq)
	if sRec.Code != 200 {
		t.Fatalf("POST /v1/responses stream failed: %d %s", sRec.Code, sRec.Body.String())
	}
	sseOut := sRec.Body.String()
	for _, expectedEvent := range []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.reasoning_summary_text.delta",
		"event: response.function_call_arguments.delta",
		"event: response.completed",
	} {
		if !strings.Contains(sseOut, expectedEvent) {
			t.Fatalf("expected SSE stream to contain %q, got:\n%s", expectedEvent, sseOut)
		}
	}
}

func TestRemoteImageURLFetchAndMagicByteMIME(t *testing.T) {
	t.Setenv("BRIDGE_ALLOW_PRIVATE_IMAGE_URLS", "1")
	// Minimal JPEG magic bytes (\xff\xd8\xff\xe0...)
	jpegBytes := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0xff, 0xd9}
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(jpegBytes)
	}))
	defer imgSrv.Close()

	var uploadedMIME, uploadedName string
	srv, acct := newTestHarness(false, false)
	client := &mockUpstream{
		uploadFn: func(ctx context.Context, data []byte, filename, contentType string) (gemini.UploadedFile, error) {
			uploadedMIME = contentType
			uploadedName = filename
			return gemini.UploadedFile{Path: "/contrib_service/ttl_1d/" + filename, Filename: filename}, nil
		},
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c_img",
				RID:             "r_img",
				AnsweredModelID: testModel.ModelID,
				Frames:          []gemini.Frame{makeFrame("nice jpeg", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}

	rec := runChat(t, srv, acct, client, map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "What is in this image?"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": imgSrv.URL + "/photo.bin"}},
				},
			},
		},
	})
	if rec.Code != 200 {
		t.Fatalf("expected 200 OK with remote image URL, got %d: %s", rec.Code, rec.Body.String())
	}
	if uploadedMIME != "image/jpeg" || !strings.HasSuffix(uploadedName, ".jpg") {
		t.Fatalf("expected magic-byte detection to set image/jpeg and .jpg, got mime=%q name=%q", uploadedMIME, uploadedName)
	}
}

func TestBufferedStreamRevisionLiveThoughtsAndCrossModelQuota(t *testing.T) {
	// 1. Buffered stream revision after >240 chars must emit upstream_revision error and no [DONE]
	srv, acct := newTestHarness(false, false)
	revClient := clientFromFrames([]gemini.Frame{
		makeFrame(strings.Repeat("A", 300), "", false, testModel.ModelID),
		makeFrame("Corrected final answer", "", true, testModel.ModelID),
	})
	revRec := runChat(t, srv, acct, revClient, map[string]any{"stream": true})
	if !strings.Contains(revRec.Body.String(), "upstream_revision") || strings.Contains(revRec.Body.String(), "[DONE]") {
		t.Fatalf("expected upstream_revision error and no [DONE], got:\n%s", revRec.Body.String())
	}

	// 2. LiveThoughts=false must suppress reasoning_content in chat responses
	srv.Cfg.LiveThoughts = false
	noThinkRec := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame("visible reply", "secret reasoning", true, testModel.ModelID),
	}), nil)
	if noThinkRec.Code != 200 || strings.Contains(noThinkRec.Body.String(), "secret reasoning") {
		t.Fatalf("expected reasoning suppressed when LiveThoughts=false, got %d: %s", noThinkRec.Code, noThinkRec.Body.String())
	}
	srv.Cfg.LiveThoughts = true

	// 3. Exhausted Flash quota bucket must not block Pro model requests
	acct.mu.Lock()
	acct.Quota = []gemini.QuotaBucket{
		{
			ID:        "1-2",
			Label:     "Flash",
			Total:     10,
			Remaining: 0,
			ResetAt:   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
	}
	acct.mu.Unlock()
	proRec := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame("pro works", "", true, testModel.ModelID),
	}), nil)
	if proRec.Code != 200 {
		t.Fatalf("expected Pro request to succeed when only Flash quota is spent, got %d: %s", proRec.Code, proRec.Body.String())
	}

	// 4. Streaming /v1/responses function_call item.id must match between output_item.added and response.completed
	acct.mu.Lock()
	acct.Client = clientFromFrames([]gemini.Frame{
		makeFrame("```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/a\"}}]}\n```", "", true, testModel.ModelID),
	})
	acct.mu.Unlock()
	sReqBody := `{
		"model": "gsb/gemini-pro",
		"stream": true,
		"tools": [{
			"type": "function",
			"name": "read_file",
			"parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}
		}],
		"input": {"type": "message", "role": "user", "content": "read /tmp/a"}
	}`
	sReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(sReqBody))
	sReq.Header.Set("Authorization", "Bearer audit")
	sRec := httptest.NewRecorder()
	srv.ServeHTTP(sRec, sReq)
	if sRec.Code != 200 {
		t.Fatalf("expected 200 on /v1/responses, got %d: %s", sRec.Code, sRec.Body.String())
	}
	var addedID, completedID string
	for _, line := range strings.Split(sRec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if ev["type"] == "response.output_item.added" {
			if item, ok := ev["item"].(map[string]any); ok && item["type"] == "function_call" {
				addedID, _ = item["id"].(string)
			}
		}
		if ev["type"] == "response.completed" {
			if resp, ok := ev["response"].(map[string]any); ok {
				if outArr, ok := resp["output"].([]any); ok {
					for _, v := range outArr {
						if item, ok := v.(map[string]any); ok && item["type"] == "function_call" {
							completedID, _ = item["id"].(string)
						}
					}
				}
			}
		}
	}
	if addedID == "" || addedID != completedID {
		t.Fatalf("expected identical function_call item.id in added (%q) and completed (%q)", addedID, completedID)
	}
}

func TestStreamingToolCallNeverLeaksThinkingProcessOrSplitsPreambleAcrossToolCalls(t *testing.T) {
	srv, acct := newTestHarness(false, false)

	// Exact reproduction of session 01a14223 entry [50]: long Thinking Process containing
	// inline `{ return this.isMuted; }` and `< 12,000 triangles` followed by fenced tool_calls.
	thinkingChunk1 := "Thinking Process:\n\n1.  **Analyze Current State:**\n    *   `WaterAndSky.ts` was written cleanly.\n    *   The rule violation was `public get muted(): boolean { return this.isMuted; }` (a tiny 1-line getter) and `(window as unknown as any).webkitAudioContext`.\n" + strings.Repeat("    *    verifying `< 12,000 triangles` and `[x, z]` coordinates.\n", 8)
	thinkingFull := thinkingChunk1 + "    *   Let's fix `NatureAudioEngine.ts` now.\n"
	toolBlock := "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"src/audio/NatureAudioEngine.ts\"}}]}\n```"

	rawFrame := func(rawText string, done bool) gemini.Frame {
		status := 1
		if done {
			status = 2
		}
		q, _ := json.Marshal(rawText)
		inner := fmt.Sprintf(`[null,["c_leak","r_leak"],null,null,[["rc_1",[%s],null,null,null,null,null,null,[%d]]],{"40":%q,"43":"Pro"}]`, string(q), status, testModel.ModelID)
		esc, _ := json.Marshal(inner)
		parsed, err := gemini.ParseFrameLine(`[["wrb.fr",null,` + string(esc) + `]]`)
		if err != nil || len(parsed.Frames) != 1 {
			t.Fatalf("ParseFrameLine failed: %v", err)
		}
		return parsed.Frames[0]
	}

	acct.mu.Lock()
	acct.Client = clientFromFrames([]gemini.Frame{
		rawFrame(thinkingChunk1, false),
		rawFrame(thinkingFull+"```json", false),
		rawFrame(thinkingFull+toolBlock, true),
	})
	acct.mu.Unlock()

	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			},
		},
	}

	rec := runChat(t, srv, acct, acct.Client, map[string]any{
		"stream": true,
		"tools":  tools,
	})
	if rec.Code != 200 {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	events := parseSSEEvents(rec.Body.String())
	var streamedReasoning, streamedContent string
	sawToolCall := false
	var reasoningTokens float64
	for _, ev := range events {
		if u, ok := ev["usage"].(map[string]any); ok {
			if d, ok := u["completion_tokens_details"].(map[string]any); ok {
				reasoningTokens, _ = d["reasoning_tokens"].(float64)
			}
		}
		choices, _ := ev["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		c0, _ := choices[0].(map[string]any)
		delta, _ := c0["delta"].(map[string]any)
		if r, ok := delta["reasoning_content"].(string); ok && r != "" {
			if sawToolCall {
				t.Fatalf("reasoning_content delta %q emitted AFTER tool_calls started", r)
			}
			streamedReasoning += r
		}
		if c, ok := delta["content"].(string); ok && c != "" {
			streamedContent += c
		}
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			sawToolCall = true
		}
	}

	if streamedContent != "" {
		t.Fatalf("expected zero content leaked on tool_calls turn, got %q", streamedContent)
	}
	if !sawToolCall {
		t.Fatal("expected tool_calls delta in stream")
	}
	if !strings.Contains(streamedReasoning, "public get muted(): boolean { return this.isMuted; }") {
		t.Fatalf("expected Thinking Process streamed cleanly via reasoning_content, got %q", streamedReasoning)
	}
	if reasoningTokens <= 0 {
		t.Fatalf("expected reasoning_tokens > 0 in usage, got %v", reasoningTokens)
	}
}

func TestDefaultToolStreamingBuffersUntilFinalValidation(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			},
		},
	}
	partial := `{"tool_calls":[{"name":"read_file","arguments":{"path":"` + strings.Repeat("a", 180)
	final := `{"tool_calls":[{"name":"read_file","arguments":{"path":"/tmp/final"}}]}`
	rec := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame(partial, "", false, testModel.ModelID),
		makeFrame(final, "", true, testModel.ModelID),
	}), map[string]any{"stream": true, "tools": tools})
	if rec.Code != 200 {
		t.Fatalf("expected final validated tool call to succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "upstream_revision") || !strings.Contains(rec.Body.String(), "/tmp/final") {
		t.Fatalf("expected only authoritative final arguments, got %s", rec.Body.String())
	}
}

func TestToolEnabledLiveProseAndPreambleReasoningStreaming(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			},
		},
	}

	// 1. Plain-text answer with tools present and KeepToolPreamble=false must stream incremental content before done=true.
	chunk1 := strings.Repeat("Streaming plain prose answer while tools are declared in the request. ", 6) // 420 bytes
	chunk2 := chunk1 + "Final concluding sentence."
	var midStreamDeltas int
	clientProse := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c1",
				RID:             "r1",
				AnsweredModelID: testModel.ModelID,
				Frames:          []gemini.Frame{makeFrame(chunk1, "", false, testModel.ModelID)},
			})
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c1",
				RID:             "r1",
				AnsweredModelID: testModel.ModelID,
				Frames:          []gemini.Frame{makeFrame(chunk2, "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	recProse := runChat(t, srv, acct, clientProse, map[string]any{"stream": true, "tools": tools})
	if recProse.Code != 200 {
		t.Fatalf("expected 200 OK, got %d: %s", recProse.Code, recProse.Body.String())
	}
	var fullContent string
	for _, ev := range parseSSEEvents(recProse.Body.String()) {
		choices, _ := ev["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		c0, _ := choices[0].(map[string]any)
		delta, _ := c0["delta"].(map[string]any)
		if c, ok := delta["content"].(string); ok && c != "" {
			midStreamDeltas++
			fullContent += c
		}
	}
	if midStreamDeltas < 2 || fullContent != chunk2 {
		t.Fatalf("expected >=2 incremental content chunks totaling full prose, got chunks=%d content=%q", midStreamDeltas, fullContent)
	}

	// 2. Pre-tool preamble before ```json tool_calls streams live into reasoning_content on non-final frame.
	preFrame := "Inspecting configuration before reading file.\n```json\n{\"tool_calls\":[{\"name\":\"read_file\""
	finalToolFrame := "Inspecting configuration before reading file.\n```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/cfg\"}}]}\n```"
	recTool := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame(preFrame, "", false, testModel.ModelID),
		makeFrame(finalToolFrame, "", true, testModel.ModelID),
	}), map[string]any{"stream": true, "tools": tools})
	if recTool.Code != 200 {
		t.Fatalf("expected 200 OK, got %d: %s", recTool.Code, recTool.Body.String())
	}
	var reasoningBeforeTool string
	sawTool := false
	for _, ev := range parseSSEEvents(recTool.Body.String()) {
		choices, _ := ev["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		c0, _ := choices[0].(map[string]any)
		delta, _ := c0["delta"].(map[string]any)
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			sawTool = true
		}
		if r, ok := delta["reasoning_content"].(string); ok && r != "" && !sawTool {
			reasoningBeforeTool += r
		}
		if c, ok := delta["content"].(string); ok && c != "" {
			t.Fatalf("expected no content delta on pre-tool preamble turn, got %q", c)
		}
	}
	if !sawTool || !strings.Contains(reasoningBeforeTool, "Inspecting configuration before reading file.") {
		t.Fatalf("expected preamble streamed via reasoning_content before tool_calls, got reasoning=%q sawTool=%v", reasoningBeforeTool, sawTool)
	}
}

func TestToolCallPreservedWhenFinalFrameOverwritesWithGeminiErrorReply(t *testing.T) {
	srv, acct := newTestHarness(false, false)

	toolJSON := "```json\n{\"tool_calls\":[{\"name\":\"bash\",\"arguments\":{\"command\":\"npm install @vitejs/plugin-react@^4.3.3\",\"i\":\"Installing compatible Vite 5 React plugin\"}},{\"name\":\"read_file\",\"arguments\":{\"path\":\"src\"}}]}\n```"
	acct.mu.Lock()
	acct.Client = clientFromFrames([]gemini.Frame{
		makeFrame(toolJSON, "Resolving peer dependency conflict for Vite 5.", false, testModel.ModelID),
		makeFrame("I seem to be encountering an error right now. Please try again later.", "Resolving peer dependency conflict for Vite 5.", true, testModel.ModelID),
	})
	acct.mu.Unlock()

	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "bash",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
						"i":       map[string]any{"type": "string"},
					},
					"required": []any{"command"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			},
		},
	}

	rec := runChat(t, srv, acct, acct.Client, map[string]any{
		"stream": true,
		"tools":  tools,
	})
	if rec.Code != 200 {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "upstream_error_reply") || strings.Contains(rec.Body.String(), "i seem to be encountering an error") {
		t.Fatalf("expected trailing error replacement frame to be ignored when valid tool calls already parsed, got: %s", rec.Body.String())
	}
	events := parseSSEEvents(rec.Body.String())
	toolCallCount := 0
	sawFinishToolCalls := false
	for _, ev := range events {
		choices, _ := ev["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		c0, _ := choices[0].(map[string]any)
		if c0["finish_reason"] == "tool_calls" {
			sawFinishToolCalls = true
		}
		delta, _ := c0["delta"].(map[string]any)
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tcAny := range tcs {
				tc, _ := tcAny.(map[string]any)
				if fn, ok := tc["function"].(map[string]any); ok && fn["name"] != nil {
					toolCallCount++
				}
			}
		}
	}
	if toolCallCount != 2 || !sawFinishToolCalls {
		t.Fatalf("expected 2 tool calls and finish_reason=tool_calls, got count=%d finish=%v body=%s", toolCallCount, sawFinishToolCalls, rec.Body.String())
	}
}

func TestSessionCacheDiskPersistenceAndAccountAffinity(t *testing.T) {
	tmpHome := t.TempDir()
	cfg := config.Load()
	cfg.HomeDir = tmpHome
	cfg.SessionCache = true
	cfg.RejectDowngrade = true
	cfg.AccountTokens = nil

	srv1 := New(cfg)
	acctA := &AccountState{
		ID:       "acctA",
		Email:    "a@example.com",
		Models:   []gemini.ModelInfo{testModel},
		ModelsAt: time.Now(),
	}
	acctB := &AccountState{
		ID:       "acctB",
		Email:    "b@example.com",
		Models:   []gemini.ModelInfo{testModel},
		ModelsAt: time.Now(),
	}
	srv1.SetAccounts([]*AccountState{acctA, acctB})

	history := []map[string]any{
		{"role": "user", "content": "Hello"},
		{"role": "assistant", "content": "Hi there"},
		{"role": "user", "content": "Follow up"},
	}
	srv1.saveSessionContinuation("acctB", testModel.ModelName, history[:1], nil, nil, "Hi there", nil, "cid_b", "rid_b", "rcid_b", "ctx_b")

	// Wait briefly for async disk flush to complete
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(srv1.sessionCacheDiskPath()); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// New server instance with same HomeDir must reload session-cache.json and route pool requests to acctB via affinity
	srv2 := New(cfg)
	srv2.SetAccounts([]*AccountState{acctA, acctB})
	hasher := prompt.NewHistoryHasher(history, nil, nil)
	chosen := srv2.resolveChatAccount(srv2.Accounts(), AuthScope{OK: true}, "gemini-pro", hasher)
	if chosen == nil || chosen.ID != "acctB" {
		t.Fatalf("expected SessionCache affinity to pick acctB after disk reload, got %+v", chosen)
	}
}

func TestDeleteSessionCacheRemovesEntryAndOrder(t *testing.T) {
	srv, _ := newTestHarness(false, true)
	srv.putSessionCache("old", []any{"metadata"})
	srv.putSessionCache("keep", []any{"metadata"})
	if len(srv.peekSessionCache("old")) == 0 {
		t.Fatal("expected cached entry before invalidation")
	}
	srv.deleteSessionCache("old")
	if got := srv.peekSessionCache("old"); len(got) != 0 {
		t.Fatalf("expected invalidated entry to be absent, got %#v", got)
	}
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	for _, key := range srv.sessionOrder {
		if key == "old" {
			t.Fatal("invalidated key remained in session order")
		}
	}
	if len(srv.sessionCache["keep"]) == 0 {
		t.Fatal("unrelated cache entry was removed")
	}
}

func BenchmarkServerHandleChatSSE_50FramesWithTools(b *testing.B) {
	srv, acct := newTestHarness(false, false)
	chunk := strings.Repeat("The quick brown fox jumps over the lazy dog in Three.js shader pipeline. ", 8)
	frames := make([]gemini.Frame, 50)
	accText := ""
	accThought := ""
	for i := range 50 {
		accThought += "Analyzing step " + strings.Repeat("x", 20) + "\n"
		if i < 49 {
			accText += chunk
			frames[i] = makeFrame(accText, accThought, false, testModel.ModelID)
		} else {
			finalTxt := "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"src/main.ts\"}}]}\n```"
			frames[i] = makeFrame(finalTxt, accThought, true, testModel.ModelID)
		}
	}
	acct.Client = clientFromFrames(frames)
	reqBody := map[string]any{
		"model":  "gemini-pro",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "Inspect src/main.ts"},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "read_file",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path": map[string]any{"type": "string"},
						},
						"required": []any{"path"},
					},
				},
			},
		},
	}
	rawJSON, _ := json.Marshal(reqBody)
	b.ReportAllocs()
	for b.Loop() {
		var parsed map[string]any
		_ = json.Unmarshal(rawJSON, &parsed)
		rec := httptest.NewRecorder()
		srv.HandleChat(context.Background(), rec, parsed, AuthScope{OK: true, AccountID: acct.ID})
	}
}

func TestModelsContract(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	flashModel := gemini.ModelInfo{
		ModelName:   "gemini-flash",
		ModelID:     "cccccccccccccccc",
		DisplayName: "2.5 Flash",
		Description: "Fast reasoning model",
		Aliases:     []string{"gemini-2.5-flash"},
		Capacity:    2,
		ModelNumber: 2,
	}
	liteModel := gemini.ModelInfo{
		ModelName:   "gemini-flash-lite",
		ModelID:     "dddddddddddddddd",
		DisplayName: "2.5 Flash Lite",
		Description: "Lightweight model",
		Capacity:    1,
		ModelNumber: 1,
	}
	acct.mu.Lock()
	acct.Models = []gemini.ModelInfo{flashModel, liteModel}
	acct.ModelsAt = time.Now()
	acct.mu.Unlock()

	req1 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req1.Header.Set("Authorization", "Bearer audit")
	rec1 := httptest.NewRecorder()
	srv.ServeHTTP(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("expected 200 on first /v1/models, got %d", rec1.Code)
	}
	etag := rec1.Header().Get("ETag")
	if etag == "" || !strings.Contains(rec1.Header().Get("Cache-Control"), "max-age=60") {
		t.Fatalf("expected ETag and Cache-Control headers, got ETag=%q CC=%q", etag, rec1.Header().Get("Cache-Control"))
	}

	var p1 map[string]any
	if err := json.Unmarshal(rec1.Body.Bytes(), &p1); err != nil {
		t.Fatalf("unmarshal /v1/models: %v", err)
	}
	data1, _ := p1["data"].([]any)
	if len(data1) != 2 {
		t.Fatalf("expected 2 models, got %d", len(data1))
	}
	mFlash, _ := data1[0].(map[string]any)
	mLite, _ := data1[1].(map[string]any)

	if mFlash["reasoning"] != true || mFlash["context_length"] != float64(1048576) || mFlash["max_completion_tokens"] != float64(65536) || mFlash["max_output_tokens"] != float64(65536) {
		t.Fatalf("unexpected flash metadata: %+v", mFlash)
	}
	if mFlash["display_name"] != "2.5 Flash" || mFlash["top_provider"] == nil || mFlash["architecture"] == nil {
		t.Fatalf("missing display_name, top_provider, or architecture on flash: %+v", mFlash)
	}
	if mLite["reasoning"] != false {
		t.Fatalf("expected gemini-flash-lite reasoning=false, got %v", mLite["reasoning"])
	}
	liteParams, _ := mLite["supported_parameters"].([]any)
	for _, p := range liteParams {
		if p == "reasoning_effort" || p == "reasoning" {
			t.Fatalf("expected gemini-flash-lite supported_parameters not to include %v: %v", p, liteParams)
		}
	}

	// Second request without If-None-Match -> identical created timestamp
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer audit")
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	var p2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &p2)
	d2, _ := p2["data"].([]any)
	if d2[0].(map[string]any)["created"] != mFlash["created"] {
		t.Fatalf("expected stable created timestamp across /v1/models calls")
	}

	// Conditional request with If-None-Match -> 304 Not Modified
	req3 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req3.Header.Set("Authorization", "Bearer audit")
	req3.Header.Set("If-None-Match", etag)
	rec3 := httptest.NewRecorder()
	srv.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified with matching ETag, got %d", rec3.Code)
	}
}

func TestContinuationCachedTokens(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	longTurn1 := "First turn context: " + strings.Repeat("alpha beta gamma delta ", 60)
	acct.Client = clientFromFrames([]gemini.Frame{
		makeFrame("First reply.", "", true, testModel.ModelID),
	})

	// Turn 1 populates session continuation cache
	msgsTurn1 := []any{
		map[string]any{"role": "user", "content": longTurn1},
	}
	rec1 := runChat(t, srv, acct, acct.Client, map[string]any{"messages": msgsTurn1})
	if rec1.Code != 200 {
		t.Fatalf("turn 1 failed: %d %s", rec1.Code, rec1.Body.String())
	}

	// Turn 2 hits continuation cache
	msgsTurn2 := []any{
		map[string]any{"role": "user", "content": longTurn1},
		map[string]any{"role": "assistant", "content": "First reply."},
		map[string]any{"role": "user", "content": "Short follow-up?"},
	}
	acct.Client = clientFromFrames([]gemini.Frame{
		makeFrame("Second reply.", "", true, testModel.ModelID),
	})
	rec2 := runChat(t, srv, acct, acct.Client, map[string]any{"messages": msgsTurn2})
	if rec2.Code != 200 {
		t.Fatalf("turn 2 failed: %d %s", rec2.Code, rec2.Body.String())
	}
	var chatResp map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &chatResp)
	usage, _ := chatResp["usage"].(map[string]any)
	pDetails, _ := usage["prompt_tokens_details"].(map[string]any)
	cachedTok, _ := pDetails["cached_tokens"].(float64)
	if cachedTok <= 0 {
		t.Fatalf("expected Chat usage.prompt_tokens_details.cached_tokens > 0 on continuation hit, got %+v", usage)
	}

	// Also verify /v1/responses input_tokens_details.cached_tokens
	respUsage := buildResponsesUsage(map[string]any{
		"prompt_tokens":     400,
		"completion_tokens": 50,
		"total_tokens":      450,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 300,
		},
	})
	inDetails, _ := respUsage["input_tokens_details"].(map[string]any)
	if inDetails["cached_tokens"] != 300 {
		t.Fatalf("expected Responses input_tokens_details.cached_tokens=300, got %+v", respUsage)
	}
}

func TestEarlySSEFirstByteInRelaxedMode(t *testing.T) {
	srv, acct := newTestHarness(true, false)
	srv.Cfg.EarlySSE = true

	firstPingAt := make(chan time.Duration, 1)
	startAt := time.Now()
	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			time.Sleep(250 * time.Millisecond)
			_ = onFrame(&gemini.LineParseResult{
				CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{makeFrame("done", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	acct.Client = client

	ts := httptest.NewServer(srv)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gemini-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer audit")
	startAt = time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	if n > 0 {
		firstPingAt <- time.Since(startAt)
	}
	elapsed := <-firstPingAt
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("expected relaxed EarlySSE first byte in < 200ms, took %v", elapsed)
	}
	if !strings.Contains(string(buf[:n]), ": ping") {
		t.Fatalf("expected immediate ': ping' in first SSE chunk, got %q", string(buf[:n]))
	}
}

func TestSessionBusyRetryAndFallback(t *testing.T) {
	// 1. Account-scoped token gets 429 session_busy on attempt 1 -> sleeps ~1s and succeeds on attempt 2 on SAME account
	srv, acct := newTestHarness(false, false)
	srv.Cfg.MaxAttempts = 3
	calls := 0
	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			calls++
			if calls == 1 {
				return nil, transport.NewUpstreamError("session busy", 429, "session_busy", map[string]string{"retry-after": "1"}, nil)
			}
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c1",
				RID:             "r1",
				AnsweredModelID: testModel.ModelID,
				Frames:          []gemini.Frame{makeFrame("recovered", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	rec := runChat(t, srv, acct, client, nil)
	if rec.Code != 200 || calls != 2 {
		t.Fatalf("expected account-scoped session_busy to retry and succeed on attempt 2, got code=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
	if rec.Header().Get("X-GSB-Account") != "acct" || rec.Header().Get("X-GSB-Attempt") != "2" || rec.Header().Get("X-GSB-Retry-Reason") != "session_busy" {
		t.Fatalf("unexpected X-GSB-* headers: %+v", rec.Header())
	}

	// 2. Admin token with 3 accounts gets 429 on acct1 and acct2 -> succeeds on acct3
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.MaxAttempts = 3
	cfg.MaxUpstreamAttempts = 3
	srv3 := New(cfg)

	fail429 := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			return &gemini.StreamResponse{Status: 429}, nil
		},
	}
	ok3 := clientFromFrames([]gemini.Frame{
		makeFrame("from acct3", "", true, testModel.ModelID),
	})
	a1 := &AccountState{ID: "acct1", Email: "1@e.com", Client: fail429, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	a2 := &AccountState{ID: "acct2", Email: "2@e.com", Client: fail429, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	a3 := &AccountState{ID: "acct3", Email: "3@e.com", Client: ok3, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	srv3.SetAccounts([]*AccountState{a1, a2, a3})

	rec3 := httptest.NewRecorder()
	srv3.HandleChat(context.Background(), rec3, map[string]any{
		"model":    "gemini-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, AuthScope{OK: true, Admin: true})
	if rec3.Code != 200 {
		t.Fatalf("expected 3-account fallback to succeed on acct3, got %d: %s", rec3.Code, rec3.Body.String())
	}
	if rec3.Header().Get("X-GSB-Account") != "acct3" || rec3.Header().Get("X-GSB-Attempt") != "3" {
		t.Fatalf("expected X-GSB-Account=acct3 and X-GSB-Attempt=3, got %+v", rec3.Header())
	}
}

func TestSyntheticQuotaBackoffAndClear(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	baseNow := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	srv.clock = func() time.Time { return baseNow }

	// Strike 1 -> 1m
	srv.recordModelExhausted(acct, "gemini-pro")
	acct.mu.RLock()
	q1 := acct.Quota[0]
	acct.mu.RUnlock()
	t1, _ := time.Parse(time.RFC3339, q1.ResetAt)
	if !q1.Synthetic || t1.Sub(baseNow) != 1*time.Minute {
		t.Fatalf("expected strike 1 backoff=1m and Synthetic=true, got %+v (diff=%v)", q1, t1.Sub(baseNow))
	}

	// Strike 2 -> 5m
	srv.recordModelExhausted(acct, "gemini-pro")
	acct.mu.RLock()
	q2 := acct.Quota[0]
	acct.mu.RUnlock()
	t2, _ := time.Parse(time.RFC3339, q2.ResetAt)
	if t2.Sub(baseNow) != 5*time.Minute {
		t.Fatalf("expected strike 2 backoff=5m, got %v", t2.Sub(baseNow))
	}

	// Strike 3 -> 15m
	srv.recordModelExhausted(acct, "gemini-pro")
	acct.mu.RLock()
	q3 := acct.Quota[0]
	acct.mu.RUnlock()
	t3, _ := time.Parse(time.RFC3339, q3.ResetAt)
	if t3.Sub(baseNow) != 15*time.Minute {
		t.Fatalf("expected strike 3 backoff=15m, got %v", t3.Sub(baseNow))
	}

	// lookupQuotaResetHeaders must NOT return retry-after for synthetic-only bucket
	hdrs := srv.lookupQuotaResetHeaders(context.Background(), acct, "gemini-pro")
	if hdrs["retry-after"] != "" || hdrs["x-gsb-retry-reason"] != "model_substitution" {
		t.Fatalf("expected only x-gsb-retry-reason=model_substitution for synthetic bucket, got %+v", hdrs)
	}

	// Successful request in advisory mode clears the synthetic bucket
	srv.Cfg.QuotaPreflight = "advisory"
	rec := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame("ok", "", true, testModel.ModelID),
	}), nil)
	if rec.Code != 200 {
		t.Fatalf("expected advisory mode to allow request, got %d: %s", rec.Code, rec.Body.String())
	}
	acct.mu.RLock()
	remQuota := len(acct.Quota)
	acct.mu.RUnlock()
	if remQuota != 0 {
		t.Fatalf("expected synthetic quota bucket cleared after successful generation, got %d buckets", remQuota)
	}
}

func TestReadyzAndSessionStats(t *testing.T) {
	srv, acct := newTestHarness(false, true)

	// Healthy state
	req1 := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec1 := httptest.NewRecorder()
	srv.ServeHTTP(rec1, req1)
	if rec1.Code != 200 || !strings.Contains(rec1.Body.String(), `"status":"ok"`) {
		t.Fatalf("expected /readyz status:ok, got %d %s", rec1.Code, rec1.Body.String())
	}

	// Session cache stats in /v1/sessions
	_ = srv.getSessionCache("nonexistent")
	srv.putSessionCache("hit_key", []any{"c", "r", "rc", nil, nil, nil, nil, nil, nil, "ctx"})
	_ = srv.getSessionCache("hit_key")

	reqSess := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	reqSess.Header.Set("Authorization", "Bearer audit")
	recSess := httptest.NewRecorder()
	srv.ServeHTTP(recSess, reqSess)
	var sessPayload map[string]any
	_ = json.Unmarshal(recSess.Body.Bytes(), &sessPayload)
	if sessPayload["cache_entries"] != float64(1) || sessPayload["cache_hits"] != float64(1) || sessPayload["cache_misses"] != float64(1) {
		t.Fatalf("unexpected /v1/sessions cache stats: %+v", sessPayload)
	}

	// Quarantined account -> degraded (/readyz?strict=1 returns 503)
	srv.markAccountQuarantined(acct, "TOS_VIOLATION")
	req2 := httptest.NewRequest(http.MethodGet, "/readyz?strict=1", nil)
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != 503 || !strings.Contains(rec2.Body.String(), `"status":"degraded"`) {
		t.Fatalf("expected 503 degraded on /readyz?strict=1 when quarantined, got %d %s", rec2.Code, rec2.Body.String())
	}
}

func TestPerAccountRotationJitterAndNextInterval(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.RotateCookiesEvery = 9 * time.Minute
	srv := New(cfg)

	baseNow := time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC)
	srv.clock = func() time.Time { return baseNow }

	var accts []*AccountState
	for i := range 5 {
		accts = append(accts, &AccountState{
			ID:       fmt.Sprintf("acct_%d", i),
			Email:    fmt.Sprintf("u%d@example.com", i),
			Models:   []gemini.ModelInfo{testModel},
			ModelsAt: baseNow,
			Client: &mockUpstream{
				streamFn: nil,
			},
		})
	}
	srv.SetAccounts(accts)
	srv.ScheduleAccountTimers(baseNow)

	// Verify no two accounts are scheduled within 1 second of each other
	for i := range accts {
		for j := i + 1; j < len(accts); j++ {
			diff := accts[i].NextRotateAt.Sub(accts[j].NextRotateAt)
			if diff < 0 {
				diff = -diff
			}
			if diff < time.Second {
				t.Fatalf("accounts %d and %d rotation scheduled within 1s: %v vs %v", i, j, accts[i].NextRotateAt, accts[j].NextRotateAt)
			}
		}
	}
}

func TestSoftBlockNoAmplification(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.MaxAttempts = 3
	srv := New(cfg)

	var quotaCalls atomic.Int32
	var acct2Calls atomic.Int32
	var acct1Calls atomic.Int32

	c1 := &mockUpstream{
		quotaFn: func(ctx context.Context) []gemini.QuotaBucket {
			quotaCalls.Add(1)
			return nil
		},
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			acct1Calls.Add(1)
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c1",
				RID:             "r1",
				AnsweredModelID: testModel.ModelID,
				Frames:          []gemini.Frame{makeFrame("Sorry, something went wrong. Please try your request again.", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	c2 := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			acct2Calls.Add(1)
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}

	a1 := &AccountState{ID: "acct1", Email: "1@e.com", Client: c1, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	a2 := &AccountState{ID: "acct2", Email: "2@e.com", Client: c2, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	srv.SetAccounts([]*AccountState{a1, a2})

	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, map[string]any{
		"model":    "gemini-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, AuthScope{OK: true, Admin: true})

	if quotaCalls.Load() != 0 {
		t.Fatalf("expected 0 GetQuota calls on upstream_error_reply soft block, got %d", quotaCalls.Load())
	}
	if acct2Calls.Load() != 0 {
		t.Fatalf("expected 0 failover calls to acct2 on upstream_error_reply, got %d", acct2Calls.Load())
	}
	if acct1Calls.Load() != 2 {
		t.Fatalf("expected at most 1 retry (2 total calls) on acct1 for upstream_error_reply, got %d", acct1Calls.Load())
	}
}

func TestUpstreamDegradedFailsOverToAnotherAccount(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.MaxAttempts = 3
	srv := New(cfg)

	var acct1Calls atomic.Int32
	var acct2Calls atomic.Int32

	// 1185 means Gemini Web failed the turn upstream while the account still had
	// quota, so the pool must fail over instead of surfacing a bare error code.
	c1 := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			acct1Calls.Add(1)
			return nil, transport.NewUpstreamError("upstream error code 1185", 502, "upstream_degraded", nil, nil)
		},
	}
	c2 := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			acct2Calls.Add(1)
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c1",
				RID:             "r1",
				AnsweredModelID: testModel.ModelID,
				Frames:          []gemini.Frame{makeFrame("recovered", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}

	a1 := &AccountState{ID: "acct1", Email: "1@e.com", Client: c1, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	a2 := &AccountState{ID: "acct2", Email: "2@e.com", Client: c2, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	srv.SetAccounts([]*AccountState{a1, a2})

	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, map[string]any{
		"model":    "gemini-pro",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, AuthScope{OK: true, Admin: true})

	if acct1Calls.Load() != 1 {
		t.Fatalf("expected exactly 1 call on the degraded account, got %d", acct1Calls.Load())
	}
	if acct2Calls.Load() != 1 {
		t.Fatalf("expected failover to the healthy account, got %d calls", acct2Calls.Load())
	}
	if !strings.Contains(rec.Body.String(), "recovered") {
		t.Fatalf("expected the healthy account's answer, got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "upstream_degraded") {
		t.Fatalf("expected the failure to be retried, not surfaced: %s", rec.Body.String())
	}
}

func TestQuarantineOn403TOSViolation(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.MaxAttempts = 3
	srv := New(cfg)

	var banCalls atomic.Int32
	c1 := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			banCalls.Add(1)
			return nil, transport.NewUpstreamError("account banned (TOS_VIOLATION)", 403, "account_banned", nil, nil)
		},
	}
	c2 := clientFromFrames([]gemini.Frame{
		makeFrame("ok", "", true, testModel.ModelID),
	})
	a1 := &AccountState{ID: "acct1", Email: "1@e.com", Client: c1, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	a2 := &AccountState{ID: "acct2", Email: "2@e.com", Client: c2, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	srv.SetAccounts([]*AccountState{a1, a2})

	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, map[string]any{
		"model":    "gemini-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, AuthScope{OK: true, Admin: true})

	if rec.Code != 403 {
		t.Fatalf("expected 403 on TOS_VIOLATION without retry, got %d: %s", rec.Code, rec.Body.String())
	}
	if banCalls.Load() != 1 {
		t.Fatalf("expected no retry on account_banned, got %d calls", banCalls.Load())
	}
	a1.mu.RLock()
	q := a1.Quarantined
	a1.mu.RUnlock()
	if !q {
		t.Fatal("expected acct1 to be marked Quarantined=true")
	}

	// pickFallbackAccount must skip quarantined acct1
	cand, _ := srv.pickFallbackAccount(srv.Accounts(), map[string]bool{}, "", "gemini-pro", nil)
	if cand == nil || cand.ID != "acct2" {
		t.Fatalf("expected pickFallbackAccount to skip quarantined acct1 and pick acct2, got %+v", cand)
	}
}

func TestAccountsEndpointPostureAndIdleRotationBackoff(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	baseNow := time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC)
	srv.clock = func() time.Time { return baseNow }
	srv.Cfg.RotateCookiesEvery = 9 * time.Minute
	acct.ImportedFrom = "chrome"
	acct.ProfileID = "Default"
	acct.ProxyURL = "socks5://127.0.0.1:1080"

	client := clientFromFrames([]gemini.Frame{
		makeFrame("ok", "", true, testModel.ModelID),
	})
	rec := runChat(t, srv, acct, client, nil)
	if rec.Code != 200 {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/accounts", nil)
	req.Header.Set("Authorization", "Bearer audit")
	aRec := httptest.NewRecorder()
	srv.ServeHTTP(aRec, req)
	if aRec.Code != 200 {
		t.Fatalf("expected 200 on /v1/accounts, got %d: %s", aRec.Code, aRec.Body.String())
	}
	var body map[string][]map[string]any
	if err := json.Unmarshal(aRec.Body.Bytes(), &body); err != nil || len(body["accounts"]) != 1 {
		t.Fatalf("unexpected /v1/accounts body: %s (err=%v)", aRec.Body.String(), err)
	}
	entry := body["accounts"][0]
	if entry["importedFrom"] != "chrome" || entry["profileId"] != "Default" || entry["proxyConfigured"] != true {
		t.Fatalf("expected provenance & proxy fields in /v1/accounts, got: %+v", entry)
	}
	if entry["totalRequests"] != float64(1) || entry["recentRpm"] != float64(1) || entry["peakRpm"] != float64(1) {
		t.Fatalf("expected RPM and request counters in /v1/accounts, got: %+v", entry)
	}

	// Verify idle account rotation stretches interval by 2x when LastUsed is old
	acct.mu.Lock()
	acct.LastUsed = baseNow.Add(-2 * time.Hour)
	acct.NextRotateAt = baseNow.Add(-time.Second)
	acct.NextModelRefAt = baseNow.Add(time.Hour)
	acct.mu.Unlock()
	srv.RunSchedulerStep(context.Background(), baseNow)
	acct.mu.RLock()
	nextRot := acct.NextRotateAt
	acct.mu.RUnlock()
	if nextRot.Sub(baseNow) < 18*time.Minute {
		t.Fatalf("expected idle account rotation interval stretched to >= 18m, got %v", nextRot.Sub(baseNow))
	}
}

func TestAccountStrategyFillFirstHonorsPriority(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.AccountTokens = nil
	cfg.SessionCache = false
	cfg.AccountStrategy = "fill-first"
	cfg.AccountPriority = []string{"acctC", "acctA"}
	srv := New(cfg)

	mk := func(id string) *AccountState {
		return &AccountState{
			ID:       id,
			Email:    id + "@example.com",
			Models:   []gemini.ModelInfo{testModel},
			ModelsAt: time.Now(),
		}
	}
	acctA, acctB, acctC := mk("acctA"), mk("acctB"), mk("acctC")
	srv.SetAccounts([]*AccountState{acctA, acctB, acctC})

	// Fill-first must stick to the highest-priority healthy account regardless of LastUsed.
	for i := 0; i < 4; i++ {
		got := srv.resolveChatAccount(srv.Accounts(), AuthScope{OK: true, Admin: true}, testModel.ModelName, nil)
		if got == nil || got.ID != "acctC" {
			t.Fatalf("iteration %d: expected acctC, got %+v", i, got)
		}
		got.mu.Lock()
		got.LastUsed = time.Now().Add(time.Duration(i+1) * time.Hour)
		got.mu.Unlock()
	}

	// When the preferred account is busy, selection must move to the next rank.
	acctC.mu.Lock()
	acctC.Quarantined = true
	acctC.mu.Unlock()
	got := srv.resolveChatAccount(srv.Accounts(), AuthScope{OK: true, Admin: true}, testModel.ModelName, nil)
	if got == nil || got.ID != "acctA" {
		t.Fatalf("expected acctA after acctC quarantined, got %+v", got)
	}

	// Fallback order must follow the same ranking.
	cand, _ := srv.pickFallbackAccount(srv.Accounts(), map[string]bool{"acctC": true}, "", testModel.ModelName, nil)
	if cand == nil || cand.ID != "acctA" {
		t.Fatalf("expected fallback to prefer acctA, got %+v", cand)
	}
}

func TestAccountStrategyLRUIsDefault(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.AccountTokens = nil
	cfg.SessionCache = false
	cfg.AccountStrategy = "lru"
	srv := New(cfg)
	if srv.fillFirst() || srv.rotate() {
		t.Fatal("expected an explicit lru strategy to disable fill-first and rotate")
	}
	mk := func(id string, used time.Time) *AccountState {
		return &AccountState{
			ID:       id,
			Email:    id + "@example.com",
			Models:   []gemini.ModelInfo{testModel},
			ModelsAt: time.Now(),
			LastUsed: used,
		}
	}
	now := time.Now()
	acctA := mk("acctA", now)
	acctB := mk("acctB", now.Add(-time.Hour))
	srv.SetAccounts([]*AccountState{acctA, acctB})
	got := srv.resolveChatAccount(srv.Accounts(), AuthScope{OK: true, Admin: true}, testModel.ModelName, nil)
	if got == nil || got.ID != "acctB" {
		t.Fatalf("expected LRU to pick the least recently used account acctB, got %+v", got)
	}
}

func fillFirstServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.AccountTokens = nil
	cfg.SessionCache = false
	cfg.AccountStrategy = "fill-first"
	cfg.AccountPriority = []string{"acctA", "acctB", "acctC"}
	srv := New(cfg)
	mk := func(id string) *AccountState {
		return &AccountState{
			ID:       id,
			Email:    id + "@example.com",
			Models:   []gemini.ModelInfo{testModel},
			ModelsAt: time.Now(),
		}
	}
	srv.SetAccounts([]*AccountState{mk("acctA"), mk("acctB"), mk("acctC")})
	return srv
}

func TestFillFirstReturnsToRankOneAfterQuotaReset(t *testing.T) {
	srv := fillFirstServer(t)
	now := time.Now()
	acctA := srv.Accounts()[0]

	acctA.mu.Lock()
	acctA.Quota = []gemini.QuotaBucket{{
		ID: "4", Label: "Gemini Pro", Total: 2400, Remaining: 0,
		ResetAt: now.Add(time.Hour).UTC().Format(time.RFC3339),
	}}
	acctA.mu.Unlock()
	if got := srv.resolveChatAccount(srv.Accounts(), AuthScope{OK: true, Admin: true}, testModel.ModelName, nil); got.ID != "acctB" {
		t.Fatalf("expected failover to acctB while acctA is exhausted, got %s", got.ID)
	}

	acctA.mu.Lock()
	acctA.Quota = []gemini.QuotaBucket{{
		ID: "4", Label: "Gemini Pro", Total: 2400, Remaining: 2400,
		ResetAt: now.Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}}
	acctA.mu.Unlock()
	if got := srv.resolveChatAccount(srv.Accounts(), AuthScope{OK: true, Admin: true}, testModel.ModelName, nil); got.ID != "acctA" {
		t.Fatalf("expected fill-first to return to acctA after quota reset, got %s", got.ID)
	}
}

func TestFillFirstReturnsToRankOneAfterFailureCooldown(t *testing.T) {
	srv := fillFirstServer(t)
	acctA := srv.Accounts()[0]

	srv.markAccountFailure(acctA, "synthetic failure")
	if got := srv.resolveChatAccount(srv.Accounts(), AuthScope{OK: true, Admin: true}, testModel.ModelName, nil); got.ID != "acctB" {
		t.Fatalf("expected acctB right after acctA failed, got %s", got.ID)
	}

	acctA.mu.Lock()
	acctA.LastFail = time.Now().Add(-2 * fillFirstFailCooldown)
	acctA.mu.Unlock()
	if got := srv.resolveChatAccount(srv.Accounts(), AuthScope{OK: true, Admin: true}, testModel.ModelName, nil); got.ID != "acctA" {
		t.Fatalf("expected acctA to reclaim rank 1 after cooldown, got %s", got.ID)
	}

	// Fallback selection must follow the same recovery rule.
	acctA.mu.Lock()
	acctA.LastFail = time.Now()
	acctA.mu.Unlock()
	cand, _ := srv.pickFallbackAccount(srv.Accounts(), map[string]bool{"acctA": true}, "", testModel.ModelName, nil)
	if cand == nil || cand.ID != "acctB" {
		t.Fatalf("expected fallback to pick acctB while acctA cools down, got %+v", cand)
	}
}

func rotateServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.AccountTokens = nil
	cfg.SessionCache = false
	cfg.AccountStrategy = "rotate"
	srv := New(cfg)
	mk := func(id string) *AccountState {
		return &AccountState{
			ID:       id,
			Email:    id + "@example.com",
			Models:   []gemini.ModelInfo{testModel},
			ModelsAt: time.Now(),
		}
	}
	srv.SetAccounts([]*AccountState{mk("acctA"), mk("acctB"), mk("acctC")})
	return srv
}

func TestRotateSticksUntilAccountExhausted(t *testing.T) {
	srv := rotateServer(t)
	pick := func() string {
		got := srv.resolveChatAccount(srv.Accounts(), AuthScope{OK: true, Admin: true}, testModel.ModelName, nil)
		if got == nil {
			t.Fatal("expected an account, got nil")
		}
		return got.ID
	}

	// Sticky: repeated calls stay on the first account.
	for i := 0; i < 5; i++ {
		if id := pick(); id != "acctA" {
			t.Fatalf("call %d: expected acctA to stay sticky, got %s", i, id)
		}
	}

	// Exhaust acctA -> advance to acctB and stay there.
	acctA := srv.Accounts()[0]
	acctA.mu.Lock()
	acctA.Quota = []gemini.QuotaBucket{{
		ID: "4", Label: "Gemini Pro", Total: 2400, Remaining: 0,
		ResetAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}}
	acctA.mu.Unlock()
	for i := 0; i < 3; i++ {
		if id := pick(); id != "acctB" {
			t.Fatalf("call %d: expected acctB after acctA exhausted, got %s", i, id)
		}
	}

	// Exhaust acctB -> advance to acctC.
	acctB := srv.Accounts()[1]
	acctB.mu.Lock()
	acctB.Quota = []gemini.QuotaBucket{{
		ID: "4", Label: "Gemini Pro", Total: 2400, Remaining: 0,
		ResetAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}}
	acctB.mu.Unlock()
	if id := pick(); id != "acctC" {
		t.Fatalf("expected acctC after acctB exhausted, got %s", id)
	}

	// acctA recovers -> rotation wraps back to acctA only after acctC is exhausted.
	acctA.mu.Lock()
	acctA.Quota = nil
	acctA.mu.Unlock()
	if id := pick(); id != "acctC" {
		t.Fatalf("expected to keep acctC while it is healthy, got %s", id)
	}
	acctC := srv.Accounts()[2]
	acctC.mu.Lock()
	acctC.Quota = []gemini.QuotaBucket{{
		ID: "4", Label: "Gemini Pro", Total: 2400, Remaining: 0,
		ResetAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}}
	acctC.mu.Unlock()
	if id := pick(); id != "acctA" {
		t.Fatalf("expected rotation to wrap back to acctA, got %s", id)
	}
}

func TestRotateSkipsCoolingAccount(t *testing.T) {
	srv := rotateServer(t)
	acctA := srv.Accounts()[0]
	srv.markAccountFailure(acctA, "synthetic failure")

	got := srv.resolveChatAccount(srv.Accounts(), AuthScope{OK: true, Admin: true}, testModel.ModelName, nil)
	if got == nil || got.ID != "acctB" {
		t.Fatalf("expected rotation to skip the cooling account and pick acctB, got %+v", got)
	}

	acctA.mu.Lock()
	acctA.LastFail = time.Now().Add(-2 * fillFirstFailCooldown)
	acctA.mu.Unlock()
	// The cursor moved to acctB, so acctB keeps serving; acctA is reused only
	// after the cursor account stops being available.
	if got := srv.resolveChatAccount(srv.Accounts(), AuthScope{OK: true, Admin: true}, testModel.ModelName, nil); got.ID != "acctB" {
		t.Fatalf("expected acctB to stay sticky after acctA cooldown, got %s", got.ID)
	}
}

func TestRotateFallbackAdvancesAndWraps(t *testing.T) {
	srv := rotateServer(t)
	if got := srv.resolveChatAccount(srv.Accounts(), AuthScope{OK: true, Admin: true}, testModel.ModelName, nil); got.ID != "acctA" {
		t.Fatalf("expected acctA first, got %s", got.ID)
	}
	cand, _ := srv.pickFallbackAccount(srv.Accounts(), map[string]bool{"acctA": true}, "", testModel.ModelName, nil)
	if cand == nil || cand.ID != "acctB" {
		t.Fatalf("expected fallback to advance to acctB, got %+v", cand)
	}
	cand, _ = srv.pickFallbackAccount(srv.Accounts(), map[string]bool{"acctA": true, "acctB": true, "acctC": true}, "", testModel.ModelName, nil)
	if cand != nil {
		t.Fatalf("expected no fallback when every account was tried, got %s", cand.ID)
	}
}

// The grace timer exists so a proxy with a response-header timeout does not
// abandon a request the upstream is still thinking about. It used to be gated
// on `!RejectDowngrade`, which is on by default, so the timer never ran in a
// default deployment and a slow model still lost its client at the proxy's
// timeout. Headers must now flush once the grace elapses in strict mode too; the
// downgrade guard keeps working on the stream body.
func TestGraceFlushesStrictHeadersBeforeIdentity(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.EarlySSEGrace = 20 * time.Millisecond

	frameSent := make(chan time.Time, 1)
	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			time.Sleep(200 * time.Millisecond)
			frameSent <- time.Now()
			_ = onFrame(&gemini.LineParseResult{
				CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{makeFrame("done", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	acct.Client = client

	ts := httptest.NewServer(srv)
	defer ts.Close()
	start := time.Now()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer audit")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer resp.Body.Close()
	headerAt := time.Since(start)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if headerAt > 150*time.Millisecond {
		t.Fatalf("expected headers after the 20ms grace, not after the first frame, got %v", headerAt)
	}
	select {
	case at := <-frameSent:
		if at.Sub(start) <= headerAt {
			t.Fatalf("frame landed at %v, before headers at %v: the stream did not start early", at.Sub(start), headerAt)
		}
	default:
	}
	// The headers carry a keep-alive ping, so the client has bytes before the
	// upstream produces anything.
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), ": ping") {
		t.Fatalf("expected keep-alive ping after commit, got %q", string(buf[:n]))
	}
}

// A downgrade detected after the early flush must still stop the stream: the
// body check is what replaces the status-line check once headers are gone.
func TestGraceFlushStillStopsOnDowngrade(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.EarlySSEGrace = 20 * time.Millisecond

	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			time.Sleep(80 * time.Millisecond)
			err := onFrame(&gemini.LineParseResult{
				CID: "c1", RID: "r1", AnsweredModelID: "some-other-model",
				AnsweredModelLabel: "9.9 Mystery",
				Frames:             []gemini.Frame{makeFrame("wrong", "", true, "some-other-model")},
			})
			if err == nil {
				t.Errorf("expected the downgrade guard to stop the stream, got nil")
			}
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	acct.Client = client

	ts := httptest.NewServer(srv)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer audit")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "wrong") {
		t.Fatalf("downgraded content reached the client: %q", string(body))
	}
}

func TestGraceDisabledKeepsHeadersBuffered(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.EarlySSEGrace = 0

	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c1",
				RID:             "r1",
				AnsweredModelID: testModel.ModelID,
				Frames:          []gemini.Frame{makeFrame("done", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	acct.Client = client

	ts := httptest.NewServer(srv)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer audit")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestHeadersFlushOnVerifiedIdentity(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.EarlySSEGrace = 5 * time.Second

	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			time.Sleep(200 * time.Millisecond)
			// Identity frame without text: the model is proven, the answer is not ready.
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c1",
				RID:             "r1",
				AnsweredModelID: testModel.ModelID,
			})
			time.Sleep(300 * time.Millisecond)
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c1",
				RID:             "r1",
				AnsweredModelID: testModel.ModelID,
				Frames:          []gemini.Frame{makeFrame("hello", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	acct.Client = client

	ts := httptest.NewServer(srv)
	defer ts.Close()

	start := time.Now()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer audit")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected headers on identity verification (~200ms) instead of the 5s grace, got %v", elapsed)
	}
}

func TestDowngradeStillReturnsHTTPError(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.EarlySSEGrace = 5 * time.Second

	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			_ = onFrame(&gemini.LineParseResult{
				CID:                "c1",
				RID:                "r1",
				AnsweredModelID:    "zzzzzzzzzzzzzzzz",
				AnsweredModelLabel: "3.5 Flash-Lite",
				Frames:             []gemini.Frame{makeFrame("wrong model", "", true, "zzzzzzzzzzzzzzzz")},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	acct.Client = client

	ts := httptest.NewServer(srv)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer audit")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("expected a non-200 status for a downgraded model, got 200")
	}
}

func TestSubstitutionDoesNotFailoverPinnedScope(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.MaxAttempts = 3
	cfg.MaxUpstreamAttempts = 3
	cfg.RejectDowngrade = true
	srv := New(cfg)

	var attempts atomic.Int32
	c1 := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			attempts.Add(1)
			_ = onFrame(&gemini.LineParseResult{
				CID: "c1", RID: "r1", AnsweredModelID: lowerModelID, AnsweredModelLabel: "3.5 Flash-Lite",
				Frames: []gemini.Frame{makeFrame("downgraded", "", true, lowerModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	c2 := clientFromFrames([]gemini.Frame{makeFrame("must not run", "", true, testModel.ModelID)})
	a1 := &AccountState{ID: "acct1", Email: "1@e.com", Client: c1, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	a2 := &AccountState{ID: "acct2", Email: "2@e.com", Client: c2, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	srv.SetAccounts([]*AccountState{a1, a2})

	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, map[string]any{
		"model":    testModel.ModelName,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, AuthScope{OK: true, AccountID: "acct1"})
	if rec.Code != 429 {
		t.Fatalf("expected 429 on pinned-scope substitution, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-GSB-Account"); got != "acct1" {
		t.Fatalf("expected pinned account acct1, got %q", got)
	}
	if attempts.Load() != 1 {
		t.Fatalf("expected acct1 upstream called once, got %d", attempts.Load())
	}
}

func TestRetrySelectiveBootstrapAndFreshSocket(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.MaxAttempts = 3
	cfg.MaxUpstreamAttempts = 3
	cfg.RejectDowngrade = true
	srv := New(cfg)

	var boot1, boot2 atomic.Int32
	var fresh2 atomic.Bool
	c1 := &mockUpstream{
		bootFn: func(ctx context.Context) error {
			boot1.Add(1)
			return nil
		},
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			_ = onFrame(&gemini.LineParseResult{
				CID:                "c1",
				RID:                "r1",
				AnsweredModelID:    lowerModelID,
				AnsweredModelLabel: "3.5 Flash-Lite",
				Frames:             []gemini.Frame{makeFrame("downgraded", "", true, lowerModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	c2 := &mockUpstream{
		bootFn: func(ctx context.Context) error {
			boot2.Add(1)
			return nil
		},
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			if opts.FreshSocket {
				fresh2.Store(true)
			}
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c2",
				RID:             "r2",
				AnsweredModelID: testModel.ModelID,
				Frames:          []gemini.Frame{makeFrame("ok from acct2", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	a1 := &AccountState{ID: "acct1", Email: "1@e.com", Client: c1, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	a2 := &AccountState{ID: "acct2", Email: "2@e.com", Client: c2, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	srv.SetAccounts([]*AccountState{a1, a2})

	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, map[string]any{
		"model":    "gemini-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, AuthScope{OK: true, Admin: true})
	if rec.Code != 200 {
		t.Fatalf("expected 200 on failover to acct2, got %d: %s", rec.Code, rec.Body.String())
	}
	if boot1.Load() != 0 || boot2.Load() != 0 {
		t.Fatalf("expected 0 forced Bootstrap calls on cross-account failover, got boot1=%d boot2=%d", boot1.Load(), boot2.Load())
	}
	if fresh2.Load() {
		t.Fatal("expected FreshSocket=false when failing over to healthy acct2")
	}

	// Same-account transport error retry MUST set FreshSocket=true and call Bootstrap
	srvSame, acctSame := newTestHarness(false, false)
	srvSame.Cfg.MaxAttempts = 2
	var sameBoot atomic.Int32
	var sameFreshOnRetry atomic.Bool
	sameCalls := 0
	cSame := &mockUpstream{
		bootFn: func(ctx context.Context) error {
			sameBoot.Add(1)
			return nil
		},
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			sameCalls++
			if sameCalls == 1 {
				return nil, transport.NewUpstreamError("socket reset", 502, "upstream_transport_error", nil, nil)
			}
			if opts.FreshSocket {
				sameFreshOnRetry.Store(true)
			}
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c1",
				RID:             "r1",
				AnsweredModelID: testModel.ModelID,
				Frames:          []gemini.Frame{makeFrame("recovered", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	recSame := runChat(t, srvSame, acctSame, cSame, nil)
	if recSame.Code != 200 || sameBoot.Load() != 1 || !sameFreshOnRetry.Load() {
		t.Fatalf("expected same-account transport retry to call Bootstrap once and set FreshSocket=true, got code=%d boot=%d fresh=%v",
			recSame.Code, sameBoot.Load(), sameFreshOnRetry.Load())
	}
}

func TestStrictIdentityUnknownDoesNotEmitReasoning(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	client := clientFromFrames([]gemini.Frame{
		makeFrame("answer", "secret reasoning", true, ""),
	})
	acct.Client = client
	rec := runChat(t, srv, acct, client, map[string]any{"stream": true})
	if rec.Code != 502 {
		t.Fatalf("expected identity error, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_identity_unknown") {
		t.Fatalf("expected model_identity_unknown, got %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret reasoning") || strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("strict identity gate leaked model output: %s", rec.Body.String())
	}
}

func TestStreamedReasoningPreventsRetryConcatenation(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.MaxAttempts = 3
	srv.Cfg.MaxUpstreamAttempts = 3
	var calls atomic.Int32
	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			calls.Add(1)
			_ = onFrame(&gemini.LineParseResult{
				CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{makeFrame("", "reasoning once", false, testModel.ModelID)},
			})
			return nil, transport.NewUpstreamError("socket reset", 502, "upstream_transport_error", nil, nil)
		},
	}
	rec := runChat(t, srv, acct, client, map[string]any{"stream": true})
	if calls.Load() != 1 {
		t.Fatalf("expected no retry after reasoning output, got %d upstream calls", calls.Load())
	}
	if strings.Count(rec.Body.String(), "reasoning once") != 1 {
		t.Fatalf("expected one reasoning emission, got %s", rec.Body.String())
	}
}

func TestResponsesCanonicalizesAssistantFunctionCallTurn(t *testing.T) {
	messages, _, _, err := NormalizeResponsesInput(map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "checking"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": `{"path":"/tmp/a"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeResponsesInput failed: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected one assistant and one tool message, got %#v", messages)
	}
	if messages[0]["role"] != "assistant" || messages[1]["role"] != "tool" {
		t.Fatalf("unexpected canonical roles: %#v", messages)
	}
	content, _ := messages[0]["content"].(string)
	if content != "checking" {
		t.Fatalf("assistant content was not preserved: %#v", messages[0])
	}
	calls, ok := messages[0]["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("expected one combined tool call: %#v", messages[0])
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_1" {
		t.Fatalf("call id changed: %#v", call)
	}
}

func TestStreamingToolCallErrorRetriesInStreamAndAppendsCorrection(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.MaxAttempts = 3
	srv.Cfg.MaxUpstreamAttempts = 3

	var calls atomic.Int32
	var secondPrompt string
	client := &mockUpstream{
		streamFn: func(ctx context.Context, promptStr string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			n := calls.Add(1)
			if n == 1 {
				// Frame 1: verified model identity commits SSE 200 OK headers, but no text/tool delta is emitted yet.
				_ = onFrame(&gemini.LineParseResult{
					CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
					Frames: []gemini.Frame{makeFrame("```json\n{\"tool_calls\":[{\"name\":\"read_file\"", "thinking attempt 1", false, testModel.ModelID)},
				})
				// Frame 2 (done): missing required "path" property -> triggers tool_call_error
				_ = onFrame(&gemini.LineParseResult{
					CID: "c1", RID: "r1", ContextToken: "ctx1", AnsweredModelID: testModel.ModelID,
					Frames: []gemini.Frame{makeFrame("```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{}}]}\n```", "thinking attempt 1", true, testModel.ModelID)},
				})
				return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
			}
			secondPrompt = promptStr
			_ = onFrame(&gemini.LineParseResult{
				CID: "c2", RID: "r2", ContextToken: "ctx2", AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{makeFrame("```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/fixed.txt\"}}]}\n```", "thinking attempt 2", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200, CID: "c2", RID: "r2"}, nil
		},
	}

	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
					"required": []any{"path"},
				},
			},
		},
	}
	rec := runChat(t, srv, acct, client, map[string]any{
		"stream": true,
		"tools":  tools,
	})
	if calls.Load() != 2 {
		t.Fatalf("expected 2 upstream attempts for in-stream tool_call_error recovery, got %d (body=%s)", calls.Load(), rec.Body.String())
	}
	if !strings.Contains(secondPrompt, "[System correction:") || !strings.Contains(secondPrompt, "missing required property") {
		t.Fatalf("expected retry prompt to include system correction, got:\n%s", secondPrompt)
	}
	if !strings.Contains(rec.Body.String(), `/tmp/fixed.txt`) || !strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected recovered tool_calls in SSE stream, got:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "thinking attempt 2") {
		t.Fatalf("expected retry attempt not to concatenate duplicate reasoning onto already streamed reasoning, got:\n%s", rec.Body.String())
	}
}

func TestStreamedPreambleWithLeadingWhitespaceDoesNotTriggerFalseUpstreamRevision(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.KeepToolPreamble = true
	longPreamble := " " + strings.Repeat("Analyzing transit headway and corridor speed bottlenecks. ", 8)
	toolBlock := "\n\n```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/data.db\"}}]}\n```"

	client := &mockUpstream{
		streamFn: func(ctx context.Context, promptStr string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			_ = onFrame(&gemini.LineParseResult{
				CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{makeFrame(longPreamble, "", false, testModel.ModelID)},
			})
			_ = onFrame(&gemini.LineParseResult{
				CID: "c1", RID: "r1", ContextToken: "ctx1", AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{makeFrame(longPreamble+toolBlock, "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
		},
	}
	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
					"required": []any{"path"},
				},
			},
		},
	}
	rec := runChat(t, srv, acct, client, map[string]any{
		"stream": true,
		"tools":  tools,
	})
	if strings.Contains(rec.Body.String(), "upstream_revision") {
		t.Fatalf("unexpected false upstream_revision on leading-space preamble:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `/tmp/data.db`) || !strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected tool_calls completion, got:\n%s", rec.Body.String())
	}
}
