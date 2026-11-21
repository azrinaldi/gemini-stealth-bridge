package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

func TestWebUIDisabledByDefaultReturns404(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = t.TempDir()
	cfg.AccountsDir = filepath.Join(cfg.HomeDir, "accounts")
	cfg.WebUI = false

	srv := New(cfg)

	for _, path := range []string{"/ui", "/ui/", "/v1/admin/config"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:43210"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("path %s with WebUI=false: expected 404, got %d (%s)", path, rec.Code, rec.Body.String())
		}
	}
}

func TestWebUIRemoteAccessGateAndRateLimit(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = t.TempDir()
	cfg.AccountsDir = filepath.Join(cfg.HomeDir, "accounts")
	cfg.EnvFile = filepath.Join(cfg.HomeDir, "config.env")
	cfg.WebUI = true
	cfg.APIKey = "public"
	cfg.AdminKey = ""

	srv := New(cfg)

	// 1. Direct loopback with public key -> allowed
	reqLoop := httptest.NewRequest(http.MethodGet, "/ui", nil)
	reqLoop.RemoteAddr = "127.0.0.1:50001"
	reqLoop.Host = "127.0.0.1:20133"
	recLoop := httptest.NewRecorder()
	srv.ServeHTTP(recLoop, reqLoop)
	if recLoop.Code != http.StatusOK || !strings.Contains(recLoop.Body.String(), "Gemini Stealth Bridge Control Plane") {
		t.Fatalf("loopback /ui expected 200 HTML, got %d", recLoop.Code)
	}

	// 2. Remote peer with public key and no AdminKey -> 403 remote_web_ui_locked
	reqRemote := httptest.NewRequest(http.MethodGet, "/ui", nil)
	reqRemote.RemoteAddr = "198.51.100.42:50002"
	recRemote := httptest.NewRecorder()
	srv.ServeHTTP(recRemote, reqRemote)
	if recRemote.Code != http.StatusForbidden || !strings.Contains(recRemote.Body.String(), "remote_web_ui_locked") {
		t.Fatalf("remote /ui with public key expected 403 remote_web_ui_locked, got %d: %s", recRemote.Code, recRemote.Body.String())
	}

	// 3. Loopback peer carrying X-Forwarded-For from a public IP -> treated as remote, 403
	reqProxy := httptest.NewRequest(http.MethodGet, "/v1/admin/config", nil)
	reqProxy.RemoteAddr = "127.0.0.1:50003"
	reqProxy.Host = "127.0.0.1:20133"
	reqProxy.Header.Set("X-Forwarded-For", "203.0.113.9")
	recProxy := httptest.NewRecorder()
	srv.ServeHTTP(recProxy, reqProxy)
	if recProxy.Code != http.StatusForbidden {
		t.Fatalf("proxied /v1/admin/config with public key expected 403, got %d", recProxy.Code)
	}

	// 4. Configure AdminKey -> remote /ui serves HTML shell, /v1/admin/config requires token and rate-limits brute force
	srv.UpdateCfg(func(c *config.Config) { c.AdminKey = "top-secret-admin-key" })

	for i := range 5 {
		reqBad := httptest.NewRequest(http.MethodGet, "/v1/admin/config", nil)
		reqBad.RemoteAddr = "198.51.100.77:51000"
		reqBad.Header.Set("Authorization", "Bearer wrong-key")
		recBad := httptest.NewRecorder()
		srv.ServeHTTP(recBad, reqBad)
		if recBad.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d expected 401, got %d", i+1, recBad.Code)
		}
	}

	// 6th attempt from same IP -> 429 rate limited even with valid key
	reqRL := httptest.NewRequest(http.MethodGet, "/v1/admin/config", nil)
	reqRL.RemoteAddr = "198.51.100.77:51000"
	reqRL.Header.Set("Authorization", "Bearer top-secret-admin-key")
	recRL := httptest.NewRecorder()
	srv.ServeHTTP(recRL, reqRL)
	if recRL.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt expected 429, got %d: %s", recRL.Code, recRL.Body.String())
	}

	// Valid key from clean IP -> 200 OK
	reqOK := httptest.NewRequest(http.MethodGet, "/v1/admin/config", nil)
	reqOK.RemoteAddr = "198.51.100.88:51000"
	reqOK.Header.Set("X-GSB-Admin-Key", "top-secret-admin-key")
	recOK := httptest.NewRecorder()
	srv.ServeHTTP(recOK, reqOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("valid admin key expected 200, got %d: %s", recOK.Code, recOK.Body.String())
	}
}

func TestAdminConfigPatchAndEnvFilePersistence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BRIDGE_HOME", home)
	t.Setenv("BRIDGE_DEFAULT_THINKING", "")
	t.Setenv("BRIDGE_ACCOUNT_STRATEGY", "")
	t.Setenv("BRIDGE_MAX_RPM_PER_ACCOUNT", "")

	cfg := config.Load()
	cfg.WebUI = true
	srv := New(cfg)

	patchBody := map[string]any{
		"defaultThinking":  "medium",
		"accountStrategy":  "lru",
		"maxRpmPerAccount": 36,
		"liveToolCalls":    true,
		"persist":          true,
	}
	raw, _ := json.Marshal(patchBody)
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/config", bytes.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:41000"
	req.Host = "127.0.0.1:20133"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /v1/admin/config failed: %d %s", rec.Code, rec.Body.String())
	}

	gotCfg := srv.Config()
	if gotCfg.DefaultThinking != 4 || gotCfg.AccountStrategy != "lru" || gotCfg.MaxRPMPerAccount != 36 || !gotCfg.LiveToolCalls {
		t.Fatalf("in-memory config not updated: %+v", gotCfg)
	}

	// Reload from disk via config.Load() and verify config.env was persisted
	reloaded := config.Load()
	if reloaded.DefaultThinking != 4 || reloaded.AccountStrategy != "lru" || reloaded.MaxRPMPerAccount != 36 || !reloaded.LiveToolCalls {
		t.Fatalf("reloaded config from config.env did not match persisted values: %+v", reloaded)
	}
	if reloaded.FieldSources["BRIDGE_DEFAULT_THINKING"] != "file" {
		t.Fatalf("expected source 'file' for BRIDGE_DEFAULT_THINKING, got %q", reloaded.FieldSources["BRIDGE_DEFAULT_THINKING"])
	}
}

func TestAdminAccountLifecycleAndWriteOnlyCookies(t *testing.T) {
	home := t.TempDir()
	cfg := config.Load()
	cfg.HomeDir = home
	cfg.AccountsDir = filepath.Join(home, "accounts")
	cfg.WebUI = true

	srv := New(cfg)
	srv.clientFactory = func(a *AccountState) UpstreamClient {
		return &mockUpstream{}
	}

	secretCookie := "__Secure-1PSID=super-secret-psid-value; SAPISID=fake-sapisid-12345"
	importPayload, _ := json.Marshal(map[string]any{
		"id":           "ops-acct-1",
		"accountGroup": "tier1",
		"cookie":       secretCookie,
	})
	reqImp := httptest.NewRequest(http.MethodPost, "/v1/admin/accounts/import", bytes.NewReader(importPayload))
	reqImp.RemoteAddr = "127.0.0.1:42000"
	reqImp.Host = "127.0.0.1:20133"
	recImp := httptest.NewRecorder()
	srv.ServeHTTP(recImp, reqImp)
	if recImp.Code != http.StatusOK {
		t.Fatalf("import failed: %d %s", recImp.Code, recImp.Body.String())
	}
	if strings.Contains(recImp.Body.String(), "super-secret-psid-value") {
		t.Fatalf("import response leaked raw cookie value: %s", recImp.Body.String())
	}

	// Quarantine the account, then clear quarantine via POST /v1/admin/accounts/ops-acct-1/unquarantine
	acct := srv.findAccountByID("ops-acct-1")
	if acct == nil {
		t.Fatalf("expected ops-acct-1 in server pool")
	}
	acct.mu.Lock()
	acct.Quarantined = true
	acct.QuarantineReason = "TOS_VIOLATION"
	acct.mu.Unlock()

	reqUnq := httptest.NewRequest(http.MethodPost, "/v1/admin/accounts/ops-acct-1/unquarantine", nil)
	reqUnq.RemoteAddr = "127.0.0.1:42000"
	reqUnq.Host = "127.0.0.1:20133"
	recUnq := httptest.NewRecorder()
	srv.ServeHTTP(recUnq, reqUnq)
	if recUnq.Code != http.StatusOK {
		t.Fatalf("unquarantine failed: %d %s", recUnq.Code, recUnq.Body.String())
	}
	if acct.Quarantined {
		t.Fatalf("expected account quarantine cleared")
	}

	// Delete the account via DELETE /v1/admin/accounts/ops-acct-1
	reqDel := httptest.NewRequest(http.MethodDelete, "/v1/admin/accounts/ops-acct-1", nil)
	reqDel.RemoteAddr = "127.0.0.1:42000"
	reqDel.Host = "127.0.0.1:20133"
	recDel := httptest.NewRecorder()
	srv.ServeHTTP(recDel, reqDel)
	if recDel.Code != http.StatusOK {
		t.Fatalf("delete failed: %d %s", recDel.Code, recDel.Body.String())
	}
	if srv.findAccountByID("ops-acct-1") != nil {
		t.Fatalf("expected ops-acct-1 evicted from pool")
	}
	if _, err := os.Stat(filepath.Join(cfg.AccountsDir, "ops-acct-1.json")); !os.IsNotExist(err) {
		t.Fatalf("expected ops-acct-1.json removed from disk")
	}
}

func TestServerSideMediaGeneratorToolAndImageEndpoint(t *testing.T) {
	srv, acct := newTestHarness(false, true)

	callNum := 0
	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			callNum++
			switch callNum {
			case 1:
				// Turn 1: model decides to call built-in generate_media tool
				_ = onFrame(&gemini.LineParseResult{
					CID: "c_1",
					RID: "r_1",
					Frames: []gemini.Frame{
						makeFrame("```json\n{\"tool_calls\":[{\"name\":\"generate_media\",\"arguments\":{\"media_type\":\"image\",\"prompt\":\"neon cyberpunk cat\"}}]}\n```", "", true, testModel.ModelID),
					},
				})
			case 2:
				// Turn 2: server-side media generation call (DisableTools=false)
				if opts.DisableTools {
					t.Fatalf("expected DisableTools=false during media generation, got true")
				}
				_ = onFrame(&gemini.LineParseResult{
					CID:             "c_media",
					RID:             "r_media",
					MediaToolActive: true,
					Frames: []gemini.Frame{
						{
							RCID: "rc_media",
							Text: "![Generated Image](https://lh3.googleusercontent.com/gg-dl/test-cat=s2048)",
							Done: true,
							GeneratedImages: []gemini.GeneratedImage{
								{URL: "https://lh3.googleusercontent.com/gg-dl/test-cat=s2048", Title: "Cyberpunk Cat"},
							},
						},
					},
				})
			case 3:
				// Turn 3: model receives the proxied /v1/media/<id>.jpg URL and calls client's bash tool to save it
				if !strings.Contains(prompt, "/v1/media/") {
					t.Fatalf("expected proxied /v1/media/ URL injected into continuation prompt, got: %s", prompt)
				}
				_ = onFrame(&gemini.LineParseResult{
					CID: "c_2",
					RID: "r_2",
					Frames: []gemini.Frame{
						makeFrame("```json\n{\"tool_calls\":[{\"name\":\"bash\",\"arguments\":{\"command\":\"curl -fsSL http://127.0.0.1:20133/v1/media/cat.jpg -o /tmp/cat.jpg\"}}]}\n```", "", true, testModel.ModelID),
					},
				})
			}
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}

	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "bash",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"command": map[string]any{"type": "string"}},
					"required":   []any{"command"},
				},
			},
		},
	}
	rec := runChat(t, srv, acct, client, map[string]any{
		"tools":       tools,
		"tool_choice": "auto",
		"messages": []any{
			map[string]any{"role": "user", "content": "generate an image of a neon cyberpunk cat"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from agent generate_media + bash workflow, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"bash"`) {
		t.Fatalf("expected final client tool call to be bash, got: %s", rec.Body.String())
	}
}

func TestStreamingPoolRotatesOnSubstitutionAfterHeadersAndMediaRound(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = t.TempDir()
	cfg.AccountTokens = nil
	cfg.AccountStrategy = "rotate"
	cfg.RejectDowngrade = true
	cfg.MaxAttempts = 3
	cfg.MaxUpstreamAttempts = 3
	srv := New(cfg)
	t.Cleanup(srv.FlushSessionCacheNow)

	acct1Calls := 0
	c1 := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			acct1Calls++
			switch acct1Calls {
			case 1:
				// Frame 1 (done=false): verifies identity and flushes SSE 200 OK headers!
				_ = onFrame(&gemini.LineParseResult{
					CID:             "c1",
					RID:             "r1",
					AnsweredModelID: testModel.ModelID,
					Frames: []gemini.Frame{
						makeFrame("```json\n{\"tool_calls\":[{\"name\":\"generate_media\"", "", false, testModel.ModelID),
					},
				})
				// Frame 2 (done=true): calls generate_media
				_ = onFrame(&gemini.LineParseResult{
					CID:             "c1",
					RID:             "r1",
					AnsweredModelID: testModel.ModelID,
					Frames: []gemini.Frame{
						makeFrame("```json\n{\"tool_calls\":[{\"name\":\"generate_media\",\"arguments\":{\"media_type\":\"video\",\"prompt\":\"explainer video\"}}]}\n```", "", true, testModel.ModelID),
					},
				})
			case 2:
				// Media generation succeeds on acct1
				_ = onFrame(&gemini.LineParseResult{
					CID:             "c_vid",
					RID:             "r_vid",
					MediaToolActive: true,
					Frames: []gemini.Frame{
						{
							RCID: "rc_vid",
							Text: "Video ready",
							Done: true,
							GeneratedVideos: []gemini.GeneratedVideo{
								{URL: "https://contribution.usercontent.google.com/download?c=123", Title: "Explainer"},
							},
						},
					},
				})
			case 3:
				// Post-media turn on acct1 gets downgraded to 3.5 Flash-Lite!
				_ = onFrame(&gemini.LineParseResult{
					CID:                "c1_sub",
					RID:                "r1_sub",
					AnsweredModelID:    lowerModelID,
					AnsweredModelLabel: "3.5 Flash-Lite",
					Frames: []gemini.Frame{
						makeFrame("downgraded", "", true, lowerModelID),
					},
				})
			}
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}

	acct2Called := false
	c2 := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			acct2Called = true
			if !strings.Contains(prompt, "/v1/media/") {
				t.Fatalf("expected failover account acct2 to receive prompt with generated /v1/media/ URL, got: %s", prompt)
			}
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c2",
				RID:             "r2",
				AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{
					makeFrame("```json\n{\"tool_calls\":[{\"name\":\"bash\",\"arguments\":{\"command\":\"curl -fsSL http://127.0.0.1:20133/v1/media/vid.mp4 -o ./explainer.mp4\"}}]}\n```", "", true, testModel.ModelID),
				},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}

	a1 := &AccountState{ID: "acct1", Email: "1@e.com", Client: c1, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	a2 := &AccountState{ID: "acct2", Email: "2@e.com", Client: c2, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	srv.SetAccounts([]*AccountState{a1, a2})

	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "bash",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"command": map[string]any{"type": "string"}},
					"required":   []any{"command"},
				},
			},
		},
	}
	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, map[string]any{
		"model":       testModel.ModelName,
		"stream":      true,
		"tools":       tools,
		"tool_choice": "auto",
		"messages":    []any{map[string]any{"role": "user", "content": "make an explainer video"}},
	}, AuthScope{OK: true, Admin: true})

	if !acct2Called {
		t.Fatalf("expected pool to rotate from acct1 to acct2 on in-stream model_substitution, body=%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) || !strings.Contains(rec.Body.String(), "./explainer.mp4") {
		t.Fatalf("expected completed tool_calls stream from acct2, got:\n%s", rec.Body.String())
	}
}

func TestMediaToolZeroOverheadOnCodingTurnsAndSessionCacheContinuity(t *testing.T) {
	srv, acct := newTestHarness(false, true)

	var capturedPrompts []string
	var capturedMeta [][]any
	turn := 0
	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			turn++
			capturedPrompts = append(capturedPrompts, prompt)
			capturedMeta = append(capturedMeta, opts.Metadata)
			_ = onFrame(&gemini.LineParseResult{
				CID: fmt.Sprintf("cid_%d", turn),
				RID: fmt.Sprintf("rid_%d", turn),
				Frames: []gemini.Frame{
					makeFrame("```json\n{\"tool_calls\":[{\"name\":\"bash\",\"arguments\":{\"command\":\"ls -la\"}}]}\n```", "", true, testModel.ModelID),
				},
			})
			return &gemini.StreamResponse{Status: 200, CID: fmt.Sprintf("cid_%d", turn), RID: fmt.Sprintf("rid_%d", turn)}, nil
		},
	}

	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "bash",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"command": map[string]any{"type": "string"}},
					"required":   []any{"command"},
				},
			},
		},
	}

	// Turn 1: Pure coding/audit request -> generate_media must NOT be injected into prompt!
	msgsTurn1 := []any{
		map[string]any{"role": "user", "content": "lakukan audit penuh terhadap database sqlite transpadang"},
	}
	rec1 := runChat(t, srv, acct, client, map[string]any{
		"tools":       tools,
		"tool_choice": "auto",
		"messages":    msgsTurn1,
	})
	if rec1.Code != http.StatusOK {
		t.Fatalf("turn 1 failed: %d %s", rec1.Code, rec1.Body.String())
	}
	if strings.Contains(capturedPrompts[0], "generate_media") {
		t.Fatalf("expected pure coding turn NOT to contain generate_media in prompt, got:\n%s", capturedPrompts[0])
	}

	var resp1 map[string]any
	_ = json.Unmarshal(rec1.Body.Bytes(), &resp1)
	choice1 := resp1["choices"].([]any)[0].(map[string]any)
	msg1 := choice1["message"].(map[string]any)
	tcs1 := msg1["tool_calls"].([]any)
	tcID1 := tcs1[0].(map[string]any)["id"].(string)

	// Turn 2: Tool result continuation -> must HIT SessionCache (opts.Metadata non-empty)!
	msgsTurn2 := []any{
		map[string]any{"role": "user", "content": "lakukan audit penuh terhadap database sqlite transpadang"},
		map[string]any{"role": "assistant", "tool_calls": tcs1},
		map[string]any{"role": "tool", "tool_call_id": tcID1, "name": "bash", "content": "tracker_202610.db"},
	}
	rec2 := runChat(t, srv, acct, client, map[string]any{
		"tools":       tools,
		"tool_choice": "auto",
		"messages":    msgsTurn2,
	})
	if rec2.Code != http.StatusOK {
		t.Fatalf("turn 2 failed: %d %s", rec2.Code, rec2.Body.String())
	}
	if len(capturedMeta[1]) == 0 {
		t.Fatalf("expected Turn 2 with tools to HIT SessionCache (opts.Metadata non-empty), got nil")
	}

	var resp2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	tcs2 := resp2["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
	tcID2 := tcs2[0].(map[string]any)["id"].(string)

	// Turn 3: User now asks to generate an explainer video -> generate_media activates WITHOUT invalidating SessionCache!
	msgsTurn3 := append(append([]any{}, msgsTurn2...),
		map[string]any{"role": "assistant", "tool_calls": tcs2},
		map[string]any{"role": "tool", "tool_call_id": tcID2, "name": "bash", "content": "done"},
		map[string]any{"role": "user", "content": "sekarang buatkan video explainer dari hasil audit ini"},
	)
	rec3 := runChat(t, srv, acct, client, map[string]any{
		"tools":       tools,
		"tool_choice": "auto",
		"messages":    msgsTurn3,
	})
	if rec3.Code != http.StatusOK {
		t.Fatalf("turn 3 failed: %d %s", rec3.Code, rec3.Body.String())
	}
	if len(capturedMeta[2]) == 0 {
		t.Fatalf("expected Turn 3 (media tool activated mid-session) to STILL HIT SessionCache")
	}
	if !strings.Contains(capturedPrompts[2], "generate_media") {
		t.Fatalf("expected Turn 3 with video intent to include generate_media in continuation reminder, got:\n%s", capturedPrompts[2])
	}
}

func TestResponsesCustomToolAndNamespaceNormalization(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	acct.Client = clientFromFrames([]gemini.Frame{
		makeFrame("```json\n{\"tool_calls\":[{\"name\":\"apply_patch\",\"arguments\":{\"input\":\"*** Begin Patch\\n*** End Patch\"}},{\"name\":\"query_db\",\"arguments\":{\"sql\":\"SELECT 1\"}}]}\n```", "", true, testModel.ModelID),
	})

	body := map[string]any{
		"model": testModel.ModelName,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "patch and query"},
			map[string]any{"type": "custom_tool_call", "call_id": "call_prev", "name": "apply_patch", "input": "*** Begin Patch\n*** End Patch"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_prev", "output": "Applied"},
		},
		"tools": []any{
			map[string]any{
				"type":        "custom",
				"name":        "apply_patch",
				"description": "Apply a patch",
			},
			map[string]any{
				"type": "namespace",
				"name": "mcp__sqlite",
				"tools": []any{
					map[string]any{
						"type": "function",
						"name": "query_db",
						"parameters": map[string]any{
							"type":       "object",
							"properties": map[string]any{"sql": map[string]any{"type": "string"}},
							"required":   []any{"sql"},
						},
					},
				},
			},
		},
	}
	rec := httptest.NewRecorder()
	srv.HandleResponses(context.Background(), rec, body, AuthScope{OK: true, AccountID: "acct"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from HandleResponses, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	output, _ := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected 2 output items (custom_tool_call + namespaced function_call), got %+v", output)
	}
	item0 := output[0].(map[string]any)
	if item0["type"] != "custom_tool_call" || item0["input"] != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("expected custom_tool_call with raw input, got %+v", item0)
	}
	item1 := output[1].(map[string]any)
	if item1["type"] != "function_call" || item1["namespace"] != "mcp__sqlite" {
		t.Fatalf("expected function_call with namespace=mcp__sqlite, got %+v", item1)
	}
}
