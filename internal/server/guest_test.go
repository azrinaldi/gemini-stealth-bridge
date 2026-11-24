package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
	fhttp "github.com/bogdanfinn/fhttp"
)

type guestHTTPMock struct {
	mu      sync.Mutex
	prompts []string
	respond func(string, int) (string, int)
}

func (m *guestHTTPMock) Do(req *fhttp.Request) (*fhttp.Response, error) {
	const endpoint = "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"
	if req.URL.String() != endpoint || req.Method != "POST" {
		return nil, errors.New("guest touched a page, bootstrap, registry or quota endpoint")
	}
	for key := range req.Header {
		key = strings.ToLower(key)
		if key == "cookie" || key == "authorization" || strings.HasPrefix(key, "x-goog-ext-") {
			return nil, fmt.Errorf("guest sent forbidden header %s", key)
		}
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil || len(form) != 1 || len(form["f.req"]) != 1 {
		return nil, errors.New("guest did not use the minimal f.req-only form")
	}
	var outer []any
	if json.Unmarshal([]byte(form.Get("f.req")), &outer) != nil || len(outer) != 2 || outer[0] != nil {
		return nil, errors.New("invalid guest f.req")
	}
	encoded, _ := outer[1].(string)
	var inner [][]string
	if json.Unmarshal([]byte(encoded), &inner) != nil || len(inner) != 1 || len(inner[0]) != 1 {
		return nil, errors.New("guest must send only [[prompt]]")
	}
	m.mu.Lock()
	m.prompts = append(m.prompts, inner[0][0])
	index := len(m.prompts) - 1
	m.mu.Unlock()
	body, status := serverGuestFrame("OK", "Anonymous model", true), 200
	if m.respond != nil {
		body, status = m.respond(inner[0][0], index)
	}
	return &fhttp.Response{StatusCode: status, Header: fhttp.Header{"Set-Cookie": {"SAPISID=ignore-guest-response-cookie; Path=/; Secure"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (*guestHTTPMock) CloseIdleConnections() {}
func (m *guestHTTPMock) captured() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.prompts...)
}

func serverGuestFrame(text, label string, done bool) string {
	candidate := []any{"rc_guest", []string{text}, nil, nil, nil, nil, nil, nil, nil}
	if done {
		candidate[8] = []int{2}
	}
	inner, _ := json.Marshal([]any{nil, []string{"c_not_reusable", "r_not_reusable"}, nil, nil, []any{candidate}, map[string]any{"43": label, "26": "ctx_not_reusable"}})
	outer, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(inner)}})
	return ")]}'\n\n" + string(outer) + "\n"
}

func newGuestTestServer(t *testing.T, mock *guestHTTPMock) *Server {
	t.Helper()
	t.Setenv("BRIDGE_HOME", t.TempDir())
	t.Setenv("BRIDGE_GUEST_ACCOUNT", "")
	cfg := config.Load()
	cfg.APIKey, cfg.AdminKey = "public", "admin-test"
	cfg.CookieDB = ""
	cfg.AccountTokens, cfg.GroupTokens = nil, nil
	cfg.RejectDowngrade, cfg.RejectLabelMismatch = true, true
	cfg.EarlySSE, cfg.EarlySSEGrace = false, 0
	cfg.MaxAttempts, cfg.MaxUpstreamAttempts = 3, 3
	srv := New(cfg)
	client := gemini.NewGuestClient(transport.ClientConfig{CustomDoer: mock}, prompt.EstimateTokens)
	srv.guest.Client = client
	t.Cleanup(func() { client.Close(); srv.Pool.Reset() })
	return srv
}

func guestRequest(t *testing.T, srv *Server, method, path, key string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(method, path, input)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func guestBody(endpoint string, text string, stream bool) map[string]any {
	body := map[string]any{"model": gemini.GuestModelID, "stream": stream}
	if endpoint == "/v1/responses" {
		body["input"] = text
	} else {
		body["messages"] = []any{map[string]any{"role": "user", "content": text}}
	}
	return body
}

func guestJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON response: %v (%s)", err, rec.Body.String())
	}
	return payload
}

func TestGuestModelsAccountsQuotaAndReadinessWithZeroRealAccounts(t *testing.T) {
	mock := &guestHTTPMock{}
	srv := newGuestTestServer(t, mock)
	if !srv.Config().GuestAccount || len(srv.Accounts()) != 0 {
		t.Fatal("guest default or real account isolation was lost")
	}
	for _, path := range []string{"/v1/models", "/v1/models?fresh=1"} {
		rec := guestRequest(t, srv, "GET", path, "public", nil)
		if rec.Code != 200 {
			t.Fatalf("models: %d %s", rec.Code, rec.Body.String())
		}
		data := guestJSON(t, rec)["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("models = %v, want only guest-free", data)
		}
		model := data[0].(map[string]any)
		if model["id"] != "guest-free" || model["display_name"] != "Guest (no account)" || model["context_length"] != float64(1048576) || model["max_output_tokens"] != float64(65536) || model["usage_estimated"] != true || model["served_model_mutable"] != true {
			t.Fatalf("guest metadata = %v", model)
		}
		caps := model["capabilities"].(map[string]any)
		if caps["vision"] != false || caps["search"] != false || caps["tools"] != true || caps["limitsSource"] != "documented" {
			t.Fatalf("guest capabilities = %v", caps)
		}
		limits := model["prompt_limits"].(map[string]any)
		if limits["hard_chars"] != float64(500000) || limits["hard_tokens"] != float64(128000) {
			t.Fatalf("anonymous prompt ceiling is missing: %v", limits)
		}
	}
	accounts := guestRequest(t, srv, "GET", "/v1/accounts?live=1", "public", nil)
	list := guestJSON(t, accounts)["accounts"].([]any)
	if len(list) != 1 {
		t.Fatalf("accounts = %v", list)
	}
	acct := list[0].(map[string]any)
	if acct["id"] != "guest" || acct["guest"] != true || acct["status"] != "ok" || acct["capturedAt"] != "" || acct["rotatedAt"] != "" {
		t.Fatalf("synthetic status = %v", acct)
	}
	for _, path := range []string{"/v1/quota", "/v1/quota?fresh=1"} {
		quota := guestRequest(t, srv, "GET", path, "public", nil)
		if quota.Code != 200 || len(guestJSON(t, quota)["accounts"].([]any)) != 0 {
			t.Fatalf("guest must be absent from quota: %s", quota.Body.String())
		}
	}
	ready := guestRequest(t, srv, "GET", "/readyz?strict=1", "", nil)
	if ready.Code != 200 || guestJSON(t, ready)["healthy"] != float64(1) {
		t.Fatalf("synthetic readiness = %s", ready.Body.String())
	}
	if len(mock.captured()) != 0 {
		t.Fatal("metadata triggered an anonymous model request")
	}
	if status, _ := AccountStatus(srv.guest, time.Now()); status != StatusOK {
		t.Fatalf("guest status = %s", status)
	}
}

func TestGuestChatAndResponsesStreamAndNonstreamUseEstimatedUsage(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", endpoint, stream), func(t *testing.T) {
				mock := &guestHTTPMock{respond: func(string, int) (string, int) {
					return serverGuestFrame("O", "Next anonymous model", false) + serverGuestFrame("OK", "Next anonymous model", true), 200
				}}
				srv := newGuestTestServer(t, mock)
				rec := guestRequest(t, srv, "POST", endpoint, "public", guestBody(endpoint, "Reply with exactly: OK", stream))
				if rec.Code != 200 {
					t.Fatalf("guest request: %d %s", rec.Code, rec.Body.String())
				}
				var payload, usage map[string]any
				var text strings.Builder
				if stream {
					if rec.Header().Get("Content-Type") != "text/event-stream" {
						t.Fatal("guest stream is not SSE")
					}
					events := parseSSEEvents(rec.Body.String())
					if len(events) == 0 {
						t.Fatal("no guest SSE events")
					}
					if endpoint == "/v1/chat/completions" {
						if !strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n") {
							t.Fatal("Chat guest stream lacks DONE")
						}
						for _, event := range events {
							choices, _ := event["choices"].([]any)
							if len(choices) == 1 {
								delta := choices[0].(map[string]any)["delta"].(map[string]any)
								if s, _ := delta["content"].(string); s != "" {
									text.WriteString(s)
								}
								if served, ok := delta["model_served"].(map[string]any); ok {
									payload = map[string]any{"model_served": served}
								}
							}
							if u, ok := event["usage"].(map[string]any); ok {
								usage = u
							}
						}
					} else {
						if strings.Contains(rec.Body.String(), "[DONE]") {
							t.Fatal("Responses must not emit Chat DONE")
						}
						for _, event := range events {
							if event["type"] == "response.output_text.delta" {
								text.WriteString(event["delta"].(string))
							}
						}
						last := events[len(events)-1]
						if last["type"] != "response.completed" {
							t.Fatalf("guest Responses ended as %v", last["type"])
						}
						payload = last["response"].(map[string]any)
						usage = payload["usage"].(map[string]any)
					}
				} else {
					payload = guestJSON(t, rec)
					usage = payload["usage"].(map[string]any)
					if endpoint == "/v1/chat/completions" {
						choice := payload["choices"].([]any)[0].(map[string]any)
						text.WriteString(choice["message"].(map[string]any)["content"].(string))
					} else {
						item := payload["output"].([]any)[0].(map[string]any)
						text.WriteString(item["content"].([]any)[0].(map[string]any)["text"].(string))
					}
				}
				if text.String() != "OK" || usage["estimated"] != true || usage["total_tokens"] != float64(prompt.EstimateTokens("Reply with exactly: OK")+prompt.EstimateTokens("OK")) {
					t.Fatalf("guest text/usage = %q %v", text.String(), usage)
				}
				if payload != nil {
					served := payload["model_served"].(map[string]any)
					if served["label"] != "Next anonymous model" || served["id"] != "" {
						t.Fatalf("guest invented or pinned identity: %v", served)
					}
				}
				if len(mock.captured()) != 1 || len(srv.Pool.Status()) != 0 || srv.sessionCacheLen() != 0 || srv.cacheHits.Load() != 0 || srv.cacheMisses.Load() != 0 {
					t.Fatal("guest touched real transport or continuation caching")
				}
			})
		}
	}
}

func TestGuestOptOutAndNoDefaultOrFallbackWithZeroRealAccounts(t *testing.T) {
	mock := &guestHTTPMock{}
	srv := newGuestTestServer(t, mock)
	for _, model := range []string{"", "gemini-pro", "Guest (no account)"} {
		body := guestBody("/v1/chat/completions", "question", false)
		body["model"] = model
		rec := guestRequest(t, srv, "POST", "/v1/chat/completions", "public", body)
		if rec.Code != 503 || guestJSON(t, rec)["error"].(map[string]any)["code"] != "auth_unavailable" {
			t.Fatalf("unspecified/real model fell back to guest: %d %s", rec.Code, rec.Body.String())
		}
	}
	srv.UpdateCfg(func(cfg *config.Config) { cfg.GuestAccount = false })
	models := guestRequest(t, srv, "GET", "/v1/models", "public", nil)
	if models.Code != 503 || strings.Contains(models.Body.String(), "guest-free") {
		t.Fatalf("opt-out changed old empty-account models behavior: %s", models.Body.String())
	}
	rec := guestRequest(t, srv, "POST", "/v1/chat/completions", "public", guestBody("/v1/chat/completions", "question", false))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "model_not_found") {
		t.Fatalf("opt-out guest request was not unknown: %s", rec.Body.String())
	}
	if len(mock.captured()) != 0 {
		t.Fatal("unsupported/default model reached anonymous upstream")
	}
}

func TestGuestAvailabilityDoesNotMaskUnhealthyRealReadiness(t *testing.T) {
	srv := newGuestTestServer(t, &guestHTTPMock{})
	srv.SetAccounts([]*AccountState{{ID: "restricted", Quarantined: true, Models: []gemini.ModelInfo{testModel}}})
	rec := guestRequest(t, srv, "GET", "/readyz?strict=1", "", nil)
	if rec.Code != 503 || guestJSON(t, rec)["status"] != "degraded" {
		t.Fatalf("synthetic guest masked unavailable real credentials: %d %s", rec.Code, rec.Body.String())
	}
	if status, _ := AccountStatus(srv.guest, time.Now()); status != StatusOK {
		t.Fatal("real account failure changed synthetic guest status")
	}
}

func TestGuestDoesNotChangeEmptyRealModelCatalogForRestrictedKeys(t *testing.T) {
	mock := &guestHTTPMock{}
	srv := newGuestTestServer(t, mock)
	srv.UpdateCfg(func(cfg *config.Config) {
		cfg.AccountTokens = map[string]string{"account-test": "not-loaded"}
		cfg.GroupTokens = map[string]string{"group-test": "empty-team"}
	})
	for _, key := range []string{"account-test", "group-test"} {
		rec := guestRequest(t, srv, "GET", "/v1/models", key, nil)
		if rec.Code != 503 || strings.Contains(rec.Body.String(), "guest-free") {
			t.Fatalf("guest changed an empty real-key catalog: %d %s", rec.Code, rec.Body.String())
		}
	}
	if len(mock.captured()) != 0 {
		t.Fatal("restricted metadata reached anonymous upstream")
	}
}

func TestGuestNoFrameCapIgnoresUnsupportedEffortButHonorsExplicitTimeout(t *testing.T) {
	cfg := config.Config{NoFrameTimeout: 75 * time.Second}
	for _, effort := range []string{"low", "medium", "high"} {
		if got := noFrameTimeoutForAttempt(cfg, gemini.GuestModel(), effort); got != 30*time.Second {
			t.Fatalf("guest deadline with %s = %s", effort, got)
		}
	}
	cfg.NoFrameTimeoutExplicit = true
	if got := noFrameTimeoutForAttempt(cfg, gemini.GuestModel(), "low"); got != 75*time.Second {
		t.Fatal("guest ignored an explicit no-frame deadline")
	}
}

func TestGuestRoutingIsolatesPoolAdminAccountAndGroupKeys(t *testing.T) {
	for _, strategy := range []string{"rotate", "lru", "fill-first"} {
		t.Run(strategy, func(t *testing.T) {
			mock := &guestHTTPMock{}
			srv := newGuestTestServer(t, mock)
			srv.UpdateCfg(func(cfg *config.Config) {
				cfg.APIKey, cfg.AdminKey, cfg.AccountStrategy = "pool-test", "admin-test", strategy
				cfg.AccountTokens = map[string]string{"account-test": "real"}
				cfg.GroupTokens = map[string]string{"group-test": "team"}
			})
			var realCalls atomic.Int32
			realClient := &mockUpstream{streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, cb func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				realCalls.Add(1)
				if err := cb(&gemini.LineParseResult{AnsweredModelID: testModel.ModelID, Frames: []gemini.Frame{makeFrame("REAL", "", true, testModel.ModelID)}}); err != nil {
					return nil, err
				}
				return &gemini.StreamResponse{Status: 200}, nil
			}}
			real := &AccountState{ID: "real", AccountGroup: "team", Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now(), Client: realClient}
			srv.SetAccounts([]*AccountState{real})
			poolReq := httptest.NewRequest("GET", "/v1/models", nil)
			poolReq.Header.Set("Authorization", "Bearer pool-test")
			if scope := srv.ResolveAuthScope(poolReq); scope.AccountID != "real" || !scope.Pool {
				t.Fatalf("guest changed single-real pool pinning: %+v", scope)
			}
			for _, key := range []string{"pool-test", "admin-test"} {
				rec := guestRequest(t, srv, "POST", "/v1/chat/completions", key, guestBody("/v1/chat/completions", "question", false))
				if rec.Code != 200 || rec.Header().Get("X-GSB-Account") != "guest" {
					t.Fatalf("pool/admin guest routing: %d %s", rec.Code, rec.Body.String())
				}
			}
			for _, key := range []string{"account-test", "group-test"} {
				rec := guestRequest(t, srv, "POST", "/v1/chat/completions", key, guestBody("/v1/chat/completions", "question", false))
				if rec.Code != 404 {
					t.Fatalf("restricted key crossed into guest scope: %d %s", rec.Code, rec.Body.String())
				}
				models := guestRequest(t, srv, "GET", "/v1/models", key, nil)
				if strings.Contains(models.Body.String(), "guest-free") {
					t.Fatal("account/group models leaked guest scope")
				}
			}
			if realCalls.Load() != 0 || len(real.reqTimestamps) != 0 || real.activeStreams != 0 || real.Fails != 0 {
				t.Fatal("guest spent real account rate or failure state")
			}
			body := guestBody("/v1/chat/completions", "question", false)
			delete(body, "model")
			rec := guestRequest(t, srv, "POST", "/v1/chat/completions", "pool-test", body)
			if rec.Code != 200 || rec.Header().Get("X-GSB-Account") != "real" || realCalls.Load() != 1 || len(mock.captured()) != 2 {
				t.Fatalf("default real routing changed: %d %s", rec.Code, rec.Body.String())
			}
			// Failover selection must remain real-only even when given both partitions.
			if fallback, _ := srv.pickFallbackAccount(srv.scopedAccounts(AuthScope{Admin: true}), map[string]bool{"real": true}, "", "", nil); fallback != nil {
				t.Fatal("real request used guest as fallback")
			}
		})
	}
}

func TestGuestPublicAliasesPreserveRealAccountScopeAndExplicitModel(t *testing.T) {
	mock := &guestHTTPMock{}
	srv := newGuestTestServer(t, mock)
	srv.SetAccounts([]*AccountState{{ID: "real", AccountGroup: "team", Models: []gemini.ModelInfo{testModel}}})
	for _, key := range []string{"", "public", poolToken(srv.Config().HomeDir), "admin-test"} {
		body := guestBody("/v1/chat/completions", "fixture", false)
		body["model"] = "gsb/guest-free@think=high"
		rec := guestRequest(t, srv, "POST", "/v1/chat/completions", key, body)
		if rec.Code != 200 || rec.Header().Get("X-GSB-Account") != "guest" {
			t.Fatalf("public/admin guest scope: %d %s", rec.Code, rec.Body.String())
		}
	}
	for _, key := range []string{"real", "gsb-real"} {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		if scope := srv.ResolveAuthScope(req); scope.AccountID != "real" || scope.Pool {
			t.Fatalf("guest changed a public real alias: %+v", scope)
		}
		rec := guestRequest(t, srv, "POST", "/v1/chat/completions", key, guestBody("/v1/chat/completions", "fixture", false))
		if rec.Code != 404 || len(mock.captured()) != 4 {
			t.Fatal("real public alias was promoted into guest scope")
		}
	}
}

func TestGuestDoesNotMoveRealRotationCursor(t *testing.T) {
	mock := &guestHTTPMock{}
	srv := newGuestTestServer(t, mock)
	srv.UpdateCfg(func(cfg *config.Config) { cfg.AccountStrategy = "rotate" })
	var calls [2]atomic.Int32
	var real []*AccountState
	for i, id := range []string{"first", "sticky"} {
		real = append(real, &AccountState{ID: id, Models: []gemini.ModelInfo{testModel}, Client: &mockUpstream{streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, cb func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			calls[i].Add(1)
			if err := cb(&gemini.LineParseResult{AnsweredModelID: testModel.ModelID, Frames: []gemini.Frame{makeFrame("REAL", "", true, testModel.ModelID)}}); err != nil {
				return nil, err
			}
			return &gemini.StreamResponse{Status: 200}, nil
		}}})
	}
	srv.SetAccounts(real)
	srv.setRotateCursor("sticky")
	guest := guestRequest(t, srv, "POST", "/v1/chat/completions", "public", guestBody("/v1/chat/completions", "fixture", false))
	if guest.Code != 200 {
		t.Fatalf("guest request failed: %d %s", guest.Code, guest.Body.String())
	}
	body := guestBody("/v1/chat/completions", "fixture", false)
	delete(body, "model")
	rec := guestRequest(t, srv, "POST", "/v1/chat/completions", "public", body)
	if rec.Code != 200 || rec.Header().Get("X-GSB-Account") != "sticky" || calls[0].Load() != 0 || calls[1].Load() != 1 || len(mock.captured()) != 1 {
		t.Fatal("guest traffic moved the real pool's sticky rotation cursor")
	}
}

func TestGuestUpstreamFailuresNeverFailOverToRealAccounts(t *testing.T) {
	for _, status := range []int{401, 403, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			mock := &guestHTTPMock{respond: func(string, int) (string, int) { return "anonymous endpoint rejected request", status }}
			srv := newGuestTestServer(t, mock)
			var realCalls atomic.Int32
			real := &AccountState{ID: "real", Models: []gemini.ModelInfo{testModel}, Client: &mockUpstream{streamFn: func(context.Context, string, gemini.StreamOptions, func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				realCalls.Add(1)
				return nil, errors.New("guest crossed into a real upstream")
			}}}
			srv.SetAccounts([]*AccountState{real})
			rec := guestRequest(t, srv, "POST", "/v1/chat/completions", "public", guestBody("/v1/chat/completions", "fixture", false))
			if rec.Code == 200 || realCalls.Load() != 0 || len(real.reqTimestamps) != 0 || real.Fails != 0 || srv.guest.Quarantined || len(srv.guest.Quota) != 0 || srv.guest.Fails != 0 {
				t.Fatalf("guest failure crossed account state: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestGuestCitationSanitizationAndStopAcrossBothAPIFormats(t *testing.T) {
	prefix := strings.Repeat("word ", 65) + "Fact"
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", endpoint, stream), func(t *testing.T) {
				mock := &guestHTTPMock{respond: func(string, int) (string, int) {
					return serverGuestFrame(prefix+" [ci", "", false) + serverGuestFrame(prefix+" [cite: 1]", "", false) + serverGuestFrame(prefix+" [cite: 1].STOPignored", "", true), 200
				}}
				srv := newGuestTestServer(t, mock)
				body := guestBody(endpoint, "fixture", stream)
				body["stop"] = "STOP"
				rec := guestRequest(t, srv, "POST", endpoint, "public", body)
				if rec.Code != 200 {
					t.Fatalf("guest citation/stop: %d %s", rec.Code, rec.Body.String())
				}
				var text strings.Builder
				if stream {
					for _, event := range parseSSEEvents(rec.Body.String()) {
						if endpoint == "/v1/responses" {
							if event["type"] == "response.output_text.delta" {
								text.WriteString(event["delta"].(string))
							}
						} else if choices, _ := event["choices"].([]any); len(choices) > 0 {
							delta := choices[0].(map[string]any)["delta"].(map[string]any)
							if s, _ := delta["content"].(string); s != "" {
								text.WriteString(s)
							}
						}
					}
				} else {
					payload := guestJSON(t, rec)
					if endpoint == "/v1/responses" {
						text.WriteString(payload["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string))
					} else {
						text.WriteString(payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string))
					}
				}
				if text.String() != prefix+"." || strings.Contains(rec.Body.String(), "[ci") || strings.Contains(rec.Body.String(), "STOP") || strings.Contains(rec.Body.String(), "ignored") {
					t.Fatalf("guest citation scaffolding/stop bytes leaked: %q", text.String())
				}
			})
		}
	}
}

func TestGuestAllowsTwoIsolatedStreamsAndRejectsThird(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		mock := &guestHTTPMock{respond: func(string, int) (string, int) { <-release; return serverGuestFrame("OK", "", true), 200 }}
		srv := newGuestTestServer(t, mock)
		responses := make(chan *httptest.ResponseRecorder, 2)
		for range 2 {
			go func() {
				responses <- guestRequest(t, srv, "POST", "/v1/chat/completions", "public", guestBody("/v1/chat/completions", "fixture", false))
			}()
		}
		synctest.Wait()
		if len(mock.captured()) != 2 {
			t.Error("guest inherited an unnecessary account-serialized transport queue")
		}
		third := guestRequest(t, srv, "POST", "/v1/chat/completions", "public", guestBody("/v1/chat/completions", "fixture", false))
		if third.Code != 429 || len(mock.captured()) != 2 {
			t.Error("third guest stream exceeded concurrency=2")
		}
		close(release)
		for range 2 {
			if rec := <-responses; rec.Code != 200 {
				t.Errorf("one of two guest streams failed: %d %s", rec.Code, rec.Body.String())
			}
		}
	})
}

func TestGuestPromptLimitsApplyBeforeCompactionAndRateAdmission(t *testing.T) {
	mock := &guestHTTPMock{}
	srv := newGuestTestServer(t, mock)
	srv.UpdateCfg(func(cfg *config.Config) {
		cfg.MaxPromptChars, cfg.MaxOldToolChars, cfg.HistoryOffload = 1024, 128, true
	})
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, text := range []string{"", " \t", strings.Repeat("x", 600000), strings.Repeat("\u4e00", 90000)} {
			rec := guestRequest(t, srv, "POST", endpoint, "public", guestBody(endpoint, text, false))
			if rec.Code != 400 {
				t.Fatalf("local guest prompt validation: %d %s", rec.Code, rec.Body.String())
			}
		}
	}
	if len(mock.captured()) != 0 || srv.guest.totalRequests != 0 {
		t.Fatal("local prompt rejection spent anonymous rate or upstream requests")
	}
	for _, tools := range []any{nil, guestWeatherTool()} {
		body := guestBody("/v1/chat/completions", "ignored", false)
		body["messages"] = []any{map[string]any{"role": "user", "content": " "}, map[string]any{"role": "assistant", "content": ""}, map[string]any{"role": "user", "content": "\n"}}
		body["tools"] = tools
		rec := guestRequest(t, srv, "POST", "/v1/chat/completions", "public", body)
		if rec.Code != 400 || len(mock.captured()) != 0 {
			t.Fatal("empty transcript was made non-empty by role labels or tool schemas")
		}
	}
	valid := strings.Repeat("x", 4000) + "PRESERVED-TAIL"
	rec := guestRequest(t, srv, "POST", "/v1/chat/completions", "public", guestBody("/v1/chat/completions", valid, false))
	if rec.Code != 200 || mock.captured()[0] != valid {
		t.Fatal("normal inline budget silently truncated the guest prompt")
	}
	largeTool := []any{map[string]any{"type": "function", "function": map[string]any{"name": "choose", "parameters": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string", "enum": []any{strings.Repeat("x", 500001)}}}}}}}
	body := guestBody("/v1/chat/completions", "choose a value", false)
	body["tools"] = largeTool
	rec = guestRequest(t, srv, "POST", "/v1/chat/completions", "public", body)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "context_length_exceeded") || len(mock.captured()) != 1 {
		t.Fatalf("guest tool schema was not included in local ceilings: %d %s", rec.Code, rec.Body.String())
	}
}

func TestGuestFlattensHistoryToolsAndResultsWithoutAttachmentsOrCache(t *testing.T) {
	mock := &guestHTTPMock{}
	srv := newGuestTestServer(t, mock)
	srv.UpdateCfg(func(cfg *config.Config) {
		cfg.MaxPromptChars, cfg.MaxOldToolChars, cfg.HistoryOffload, cfg.SessionCache = 1024, 128, true, true
	})
	oldResult := "OLD-TOOL-START" + strings.Repeat("x", 40000) + "OLD-TOOL-END"
	body := guestBody("/v1/chat/completions", "ignored", false)
	body["messages"] = []any{
		map[string]any{"role": "user", "content": "My name is Budi"},
		map[string]any{"role": "assistant", "content": "Noted"},
		map[string]any{"role": "system", "content": "SYSTEM-FIRST"},
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call-history", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"Bandung"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "call-history", "content": oldResult},
		map[string]any{"role": "user", "content": "Continue"},
		map[string]any{"role": "assistant", "content": "Continuing"},
		map[string]any{"role": "user", "content": "Who am I?"},
	}
	rec := guestRequest(t, srv, "POST", "/v1/chat/completions", "public", body)
	if rec.Code != 200 {
		t.Fatalf("flattened guest history: %d %s", rec.Code, rec.Body.String())
	}
	got := mock.captured()[0]
	if !strings.HasPrefix(got, "SYSTEM-FIRST") {
		t.Fatal("guest system instructions were not first")
	}
	for _, part := range []string{"My name is Budi", "Noted", "get_weather", `"city":"Bandung"`, "call-history", oldResult, "Who am I?"} {
		if !strings.Contains(got, part) {
			t.Fatalf("flattened history omitted %q", part[:min(len(part), 50)])
		}
	}
	if strings.Contains(got, "attached in") || strings.Contains(got, "truncated") || srv.sessionCacheLen() != 0 || srv.guest.offloadRequests != 0 {
		t.Fatal("guest offloaded or cached history")
	}
}

func guestWeatherTool() []any {
	return []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}, "days": map[string]any{"type": "integer"}}, "required": []any{"city", "days"}, "additionalProperties": false}}}}
}

func TestGuestToolAutoRequiredCoercionAndMediaSuppression(t *testing.T) {
	const envelope = `{"tool_calls":[{"name":"get_weather","arguments":{"city":"Bandung","days":"2"}}]}`
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, choice := range []string{"auto", "required"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", endpoint, choice, stream), func(t *testing.T) {
					mock := &guestHTTPMock{respond: func(p string, index int) (string, int) {
						if choice == "required" && index == 0 {
							return serverGuestFrame("There are 0 tool calls remaining for this turn.", "", true), 200
						}
						return serverGuestFrame(envelope, "", true), 200
					}}
					srv := newGuestTestServer(t, mock)
					body := guestBody(endpoint, "Generate an image, but first get the weather for Bandung", stream)
					body["tools"], body["tool_choice"] = guestWeatherTool(), choice
					rec := guestRequest(t, srv, "POST", endpoint, "public", body)
					if rec.Code != 200 || strings.Contains(rec.Body.String(), `"code":"tool_call_error"`) {
						t.Fatalf("guest tool protocol: %d %s", rec.Code, rec.Body.String())
					}
					var args string
					if stream {
						for _, event := range parseSSEEvents(rec.Body.String()) {
							if endpoint == "/v1/chat/completions" {
								choices, _ := event["choices"].([]any)
								if len(choices) == 0 {
									continue
								}
								delta := choices[0].(map[string]any)["delta"].(map[string]any)
								if calls, _ := delta["tool_calls"].([]any); len(calls) > 0 {
									if fn, ok := calls[0].(map[string]any)["function"].(map[string]any); ok {
										a, _ := fn["arguments"].(string)
										args += a
									}
								}
							} else if event["type"] == "response.function_call_arguments.done" {
								args = event["arguments"].(string)
							}
						}
					} else {
						payload := guestJSON(t, rec)
						if endpoint == "/v1/chat/completions" {
							choiceObj := payload["choices"].([]any)[0].(map[string]any)
							if choiceObj["finish_reason"] != "tool_calls" {
								t.Fatal("guest did not return a tool turn")
							}
							args = choiceObj["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
						} else {
							args = payload["output"].([]any)[0].(map[string]any)["arguments"].(string)
						}
					}
					var decoded map[string]any
					if json.Unmarshal([]byte(args), &decoded) != nil || decoded["city"] != "Bandung" || decoded["days"] != float64(2) {
						t.Fatalf("guest tool coercion failed: %q", args)
					}
					wantCalls := 1
					if choice == "required" {
						wantCalls = 2
					}
					captured := mock.captured()
					if len(captured) != wantCalls || srv.guest.Fails != 0 {
						t.Fatalf("guest required retry or failure isolation changed: calls=%d fails=%d", len(captured), srv.guest.Fails)
					}
					for _, p := range captured {
						if strings.Contains(p, "generate_media") {
							t.Fatal("guest offered the unsupported media generator")
						}
					}
				})
			}
		}
	}
}

func TestGuestRejectsMediaImagesAndMCPWithoutGeneration(t *testing.T) {
	mock := &guestHTTPMock{}
	srv := newGuestTestServer(t, mock)
	for _, endpoint := range []string{"/v1/images/generations", "/v1/videos/generations"} {
		rec := guestRequest(t, srv, "POST", endpoint, "public", map[string]any{"model": "gsb/guest-free", "prompt": "fixture"})
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "unsupported_guest_feature") {
			t.Fatalf("guest media accepted: %d %s", rec.Code, rec.Body.String())
		}
	}
	for _, kind := range []string{"image_url", "input_image", "image", "input_audio", "input_file"} {
		body := guestBody("/v1/chat/completions", "ignored", false)
		body["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "fixture"}, map[string]any{"type": kind, "image_url": "https://example.invalid/not-fetched.png"}}}}
		rec := guestRequest(t, srv, "POST", "/v1/chat/completions", "public", body)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "unsupported_guest_feature") {
			t.Fatalf("guest non-text part accepted: %d %s", rec.Code, rec.Body.String())
		}
	}
	responseImage := guestBody("/v1/responses", "ignored", false)
	responseImage["input"] = []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "fixture"}, map[string]any{"type": "input_image", "image_url": "https://example.invalid/not-fetched.png"}}}}
	if rec := guestRequest(t, srv, "POST", "/v1/responses", "public", responseImage); rec.Code != 400 || !strings.Contains(rec.Body.String(), "unsupported_guest_feature") {
		t.Fatalf("guest Responses image accepted: %d %s", rec.Code, rec.Body.String())
	}
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		body := guestBody(endpoint, "fixture", false)
		body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "generate_media", "parameters": map[string]any{"type": "object"}}}}
		if rec := guestRequest(t, srv, "POST", endpoint, "public", body); rec.Code != 400 || !strings.Contains(rec.Body.String(), "unsupported_guest_feature") {
			t.Fatalf("guest builtin media tool accepted: %d %s", rec.Code, rec.Body.String())
		}
	}
	for _, requested := range []string{"", "guest-free"} {
		rec := postMCP(t, srv, "/mcp", "public", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "web_search", "arguments": map[string]any{"query": "fixture", "model": requested}}})
		result := guestJSON(t, rec)["result"].(map[string]any)
		if result["isError"] != true {
			t.Fatalf("guest silently served MCP: %s", rec.Body.String())
		}
	}
	if len(mock.captured()) != 0 || srv.guest.totalRequests != 0 {
		t.Fatal("unsupported guest feature spent a request")
	}
}

func TestGuestConservativeLimitsAndExplicitControlOverrides(t *testing.T) {
	mock := &guestHTTPMock{}
	srv := newGuestTestServer(t, mock)
	srv.UpdateCfg(func(cfg *config.Config) {
		cfg.FieldSources["BRIDGE_MAX_RPM_PER_ACCOUNT"] = "default"
		cfg.FieldSources["BRIDGE_MAX_CONCURRENT_PER_ACCOUNT"] = "default"
	})
	for range 10 {
		rec := guestRequest(t, srv, "POST", "/v1/chat/completions", "public", guestBody("/v1/chat/completions", "fixture", false))
		if rec.Code != 200 {
			t.Fatalf("guest rejected one of the first 10 requests: %s", rec.Body.String())
		}
	}
	rec := guestRequest(t, srv, "POST", "/v1/chat/completions", "public", guestBody("/v1/chat/completions", "fixture", false))
	if rec.Code != 429 || len(mock.captured()) != 10 {
		t.Fatal("guest did not enforce conservative 10 RPM")
	}
	srv.guest.mu.Lock()
	srv.guest.reqTimestamps = nil
	srv.guest.mu.Unlock()
	finish1, err := srv.beginAuxStream(srv.guest)
	if err != nil {
		t.Fatal(err)
	}
	finish2, err := srv.beginAuxStream(srv.guest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.beginAuxStream(srv.guest); err == nil {
		t.Fatal("guest exceeded two active streams")
	}
	finish1()
	finish2()
	srv.UpdateCfg(func(cfg *config.Config) {
		cfg.MaxRPMPerAccount, cfg.MaxConcurrentPerAccount = 0, 1
		cfg.FieldSources["BRIDGE_MAX_RPM_PER_ACCOUNT"] = "env"
		cfg.FieldSources["BRIDGE_MAX_CONCURRENT_PER_ACCOUNT"] = "runtime"
	})
	if rpm, concurrency := accountRateLimits(srv.Config(), srv.guest); rpm != 0 || concurrency != 1 {
		t.Fatal("guest ignored explicit existing rate controls")
	}
	finish, err := srv.beginAuxStream(srv.guest)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if _, err := srv.beginAuxStream(srv.guest); err == nil {
		t.Fatal("guest ignored explicit concurrency=1")
	}
}

func TestGuestNeverEntersCredentialMaintenanceOrQuarantine(t *testing.T) {
	mock := &guestHTTPMock{}
	srv := newGuestTestServer(t, mock)
	var maintenanceCalls atomic.Int32
	srv.guest.Client = &mockUpstream{
		modelsFn: func(context.Context, bool) (*gemini.AvailableModelsResult, error) {
			maintenanceCalls.Add(1)
			return nil, errors.New("forbidden")
		},
		quotaFn: func(context.Context) []gemini.QuotaBucket { maintenanceCalls.Add(1); return nil },
		bootFn:  func(context.Context) error { maintenanceCalls.Add(1); return errors.New("forbidden") },
	}
	srv.clientFactory = func(*AccountState) UpstreamClient { maintenanceCalls.Add(1); return nil }
	if srv.clientFor(srv.guest) != srv.guest.Client {
		t.Fatal("guest entered the authenticated client factory")
	}
	srv.RefreshAccount(context.Background(), srv.guest, true)
	srv.EnsureModels(context.Background(), true)
	srv.ScheduleAccountTimers(time.Now())
	srv.RunSchedulerStep(context.Background(), time.Now().Add(24*time.Hour))
	srv.refreshQuotaAsync(srv.guest, srv.guest.Client)
	srv.classifyEmpty(context.Background(), srv.guest, "guest-free")
	srv.markAccountQuarantined(srv.guest, "fixture restriction")
	srv.markAccountFailure(srv.guest, "upstream_auth_error")
	srv.recordModelExhausted(srv.guest, "guest-free")
	if maintenanceCalls.Load() != 0 || srv.guest.Quarantined || srv.guest.Fails != 0 || len(srv.guest.Quota) != 0 || !srv.guest.NextRotateAt.IsZero() || !srv.guest.NextModelRefAt.IsZero() {
		t.Fatal("synthetic guest entered credential state machinery")
	}
	for _, action := range []string{"probe", "rotate", "unquarantine", "toggle", "move"} {
		if srv.findAccountByID("guest") != nil {
			t.Fatalf("guest was available to credential admin action %s", action)
		}
	}
}
