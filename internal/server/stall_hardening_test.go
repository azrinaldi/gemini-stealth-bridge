package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

func newStallTestServer() *Server {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.AccountTokens = nil
	cfg.MaxConcurrentPerAccount = 0
	cfg.MaxRPMPerAccount = 0
	cfg.MaxAttempts = 4
	cfg.MaxUpstreamAttempts = 4
	cfg.SessionCache = true
	return New(cfg)
}

func TestStalledContinuationEvictsSessionCache_Chat(t *testing.T) {
	srv := newStallTestServer()

	var a1Calls, a2Calls atomic.Int32
	stalled := &mockUpstream{
		streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			a1Calls.Add(1)
			return nil, transport.NewUpstreamError("upstream sent no candidate content or new activity for 1m15s", 504, "upstream_stalled", nil, nil)
		},
	}
	healthy := &mockUpstream{
		streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			a2Calls.Add(1)
			if len(opts.Metadata) != 0 {
				t.Errorf("fallback account must not inherit stale continuation metadata, got %v", opts.Metadata)
			}
			_ = onFrame(&gemini.LineParseResult{
				CID:    "c_new",
				RID:    "r_new",
				Frames: []gemini.Frame{makeFrame("recovered", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200, CID: "c_new", RID: "r_new"}, nil
		},
	}

	a1 := &AccountState{ID: "acct1", Email: "a1@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), Client: stalled}
	a2 := &AccountState{ID: "acct2", Email: "a2@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), Client: healthy}
	srv.SetAccounts([]*AccountState{a1, a2})

	prefix := []map[string]any{
		{"role": "user", "content": "turn 1"},
		{"role": "assistant", "content": "answer 1"},
	}
	staleKey := prompt.HashHistoryWithTools(a1.ID, testModel.ModelName, prefix, nil, nil)
	srv.putSessionCache(staleKey, gemini.BuildContinuationMetadata("c_stale", "r_stale", "rc_stale", ""))

	body := map[string]any{
		"model": testModel.ModelName,
		"messages": []any{
			map[string]any{"role": "user", "content": "turn 1"},
			map[string]any{"role": "assistant", "content": "answer 1"},
			map[string]any{"role": "user", "content": "turn 2"},
		},
	}
	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, body, AuthScope{OK: true})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "recovered") {
		t.Fatalf("expected 200 recovered, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := srv.peekSessionCache(staleKey); got != nil {
		t.Fatalf("expected stalled continuation entry to be evicted, still present: %v", got)
	}
	if a1Calls.Load() != 1 || a2Calls.Load() != 1 {
		t.Fatalf("expected 1 call on acct1 and 1 on acct2, got a1=%d a2=%d", a1Calls.Load(), a2Calls.Load())
	}
	// Single-account stall followed by healthy failover must still record failure on acct1.
	a1.mu.RLock()
	a1Fails := a1.Fails
	a1.mu.RUnlock()
	if a1Fails != 1 {
		t.Fatalf("expected acct1.Fails == 1 on single-account stall, got %d", a1Fails)
	}
}

func TestStalledContinuationEvictsSessionCache_Responses(t *testing.T) {
	srv := newStallTestServer()

	stalled := &mockUpstream{
		streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			return nil, transport.NewUpstreamError("upstream sent no candidate content or new activity for 1m15s", 504, "upstream_stalled", nil, nil)
		},
	}
	healthy := clientFromFrames([]gemini.Frame{
		makeFrame("responses-ok", "", true, testModel.ModelID),
	})

	a1 := &AccountState{ID: "acct1", Email: "a1@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), Client: stalled}
	a2 := &AccountState{ID: "acct2", Email: "a2@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), Client: healthy}
	srv.SetAccounts([]*AccountState{a1, a2})

	prefix := []map[string]any{
		{"role": "user", "content": "turn 1"},
		{"role": "assistant", "content": "answer 1"},
	}
	staleKey := prompt.HashHistoryWithTools(a1.ID, testModel.ModelName, prefix, nil, nil)
	srv.putSessionCache(staleKey, gemini.BuildContinuationMetadata("c_stale", "r_stale", "rc_stale", ""))

	body := map[string]any{
		"model": testModel.ModelName,
		"input": []any{
			map[string]any{"role": "user", "content": "turn 1"},
			map[string]any{"role": "assistant", "content": "answer 1"},
			map[string]any{"role": "user", "content": "turn 2"},
		},
	}
	rec := httptest.NewRecorder()
	srv.HandleResponses(context.Background(), rec, body, AuthScope{OK: true})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "responses-ok") {
		t.Fatalf("expected 200 responses-ok, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := srv.peekSessionCache(staleKey); got != nil {
		t.Fatalf("expected stalled continuation entry evicted on Responses API, still present: %v", got)
	}
}

func TestStalledContinuationRetriesFreshOnPinnedAccount(t *testing.T) {
	srv := newStallTestServer()

	var calls atomic.Int32
	client := &mockUpstream{
		streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			n := calls.Add(1)
			if n == 1 {
				if len(opts.Metadata) == 0 {
					t.Errorf("attempt 1 should use cached continuation metadata")
				}
				return nil, transport.NewUpstreamError("upstream sent no candidate content or new activity for 1m15s", 504, "upstream_stalled", nil, nil)
			}
			if len(opts.Metadata) != 0 {
				t.Errorf("attempt 2 must retry as fresh conversation without metadata, got %v", opts.Metadata)
			}
			_ = onFrame(&gemini.LineParseResult{
				CID:    "c_fresh",
				RID:    "r_fresh",
				Frames: []gemini.Frame{makeFrame("fresh-ok", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200, CID: "c_fresh", RID: "r_fresh"}, nil
		},
	}

	a1 := &AccountState{ID: "pinned1", Email: "p1@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), Client: client}
	srv.SetAccounts([]*AccountState{a1})

	prefix := []map[string]any{
		{"role": "user", "content": "turn 1"},
		{"role": "assistant", "content": "answer 1"},
	}
	staleKey := prompt.HashHistoryWithTools(a1.ID, testModel.ModelName, prefix, nil, nil)
	srv.putSessionCache(staleKey, gemini.BuildContinuationMetadata("c_stale", "r_stale", "rc_stale", ""))

	body := map[string]any{
		"model": testModel.ModelName,
		"messages": []any{
			map[string]any{"role": "user", "content": "turn 1"},
			map[string]any{"role": "assistant", "content": "answer 1"},
			map[string]any{"role": "user", "content": "turn 2"},
		},
	}
	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, body, AuthScope{OK: true, AccountID: a1.ID})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "fresh-ok") {
		t.Fatalf("expected 200 fresh-ok on pinned account continuation retry, got %d: %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 calls (stalled continuation + fresh retry), got %d", calls.Load())
	}
	if got := srv.peekSessionCache(staleKey); got != nil {
		t.Fatalf("expected stale cache evicted, still present: %v", got)
	}
}

func TestCrossAccountStallRollsBackFailureAndStopsPoisoning(t *testing.T) {
	srv := newStallTestServer()

	var c1, c2, c3 atomic.Int32
	mkStall := func(counter *atomic.Int32) *mockUpstream {
		return &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				counter.Add(1)
				return nil, transport.NewUpstreamError("upstream sent no candidate content or new activity for 1m15s", 504, "upstream_stalled", nil, nil)
			},
		}
	}

	a1 := &AccountState{ID: "acct1", Email: "a1@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), Client: mkStall(&c1)}
	a2 := &AccountState{ID: "acct2", Email: "a2@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), Client: mkStall(&c2)}
	a3 := &AccountState{ID: "acct3", Email: "a3@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), Client: mkStall(&c3)}
	srv.SetAccounts([]*AccountState{a1, a2, a3})

	body := map[string]any{
		"model":    testModel.ModelName,
		"messages": []any{map[string]any{"role": "user", "content": "poison payload"}},
	}
	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, body, AuthScope{OK: true})
	if rec.Code != 504 {
		t.Fatalf("expected 504 upstream_stalled, got %d: %s", rec.Code, rec.Body.String())
	}
	if c1.Load() != 1 || c2.Load() != 1 || c3.Load() != 0 {
		t.Fatalf("expected poison prompt to stop after 2 distinct stalled accounts, got calls a1=%d a2=%d a3=%d", c1.Load(), c2.Load(), c3.Load())
	}
	for _, a := range []*AccountState{a1, a2, a3} {
		a.mu.RLock()
		fails, lastErr := a.Fails, a.LastError
		a.mu.RUnlock()
		if fails != 0 || lastErr != "" {
			t.Fatalf("account %s should not be penalized by cross-account poison stall, got fails=%d lastErr=%q", a.ID, fails, lastErr)
		}
	}
}

func TestStallFollowedByUpstreamErrorReplyRollsBackFirstAccount(t *testing.T) {
	srv := newStallTestServer()

	stalled := &mockUpstream{
		streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			return nil, transport.NewUpstreamError("upstream sent no candidate content or new activity for 1m15s", 504, "upstream_stalled", nil, nil)
		},
	}
	errReply := clientFromFrames([]gemini.Frame{
		makeFrame("Sorry, something went wrong. Please try your request again.", "", true, testModel.ModelID),
	})

	a1 := &AccountState{ID: "acct1", Email: "a1@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), Client: stalled}
	a2 := &AccountState{ID: "acct2", Email: "a2@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), Client: errReply}
	srv.SetAccounts([]*AccountState{a1, a2})

	body := map[string]any{
		"model":    testModel.ModelName,
		"messages": []any{map[string]any{"role": "user", "content": "poison payload"}},
	}
	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, body, AuthScope{OK: true})
	if rec.Code != 502 {
		t.Fatalf("expected 502 upstream_error_reply, got %d: %s", rec.Code, rec.Body.String())
	}
	for _, a := range []*AccountState{a1, a2} {
		a.mu.RLock()
		fails := a.Fails
		a.mu.RUnlock()
		if fails != 0 {
			t.Fatalf("account %s should have Fails=0 after stall->upstream_error_reply, got %d", a.ID, fails)
		}
	}
}

func TestRollbackPreservesConcurrentFailure(t *testing.T) {
	now := time.Now()
	srv := &Server{clock: func() time.Time { return now }}
	a := &AccountState{ID: "acct1"}
	snap := srv.markAccountFailureSnapshot(a, "upstream_stalled")

	// Simulate a different concurrent auth failure landing afterward.
	now = now.Add(time.Second)
	srv.markAccountFailure(a, "upstream_auth_error")

	srv.rollbackAccountFailure(snap)
	a.mu.RLock()
	fails, lastErr := a.Fails, a.LastError
	a.mu.RUnlock()
	if fails != 2 || lastErr != "upstream_auth_error" {
		t.Fatalf("rollback must not clobber a newer failure, got fails=%d lastErr=%q", fails, lastErr)
	}
}

func TestNoFrameTimeoutForAttemptTiering(t *testing.T) {
	defaultCfg := config.Config{NoFrameTimeout: 180 * time.Second, NoFrameTimeoutExplicit: false}
	explicitCfg := config.Config{NoFrameTimeout: 180 * time.Second, NoFrameTimeoutExplicit: true}
	smallCfg := config.Config{NoFrameTimeout: 50 * time.Millisecond, NoFrameTimeoutExplicit: false}

	hydrogen := gemini.ModelInfo{ModelID: "56fdd199312815e2", ModelName: "gemini-4-hydrogen"}
	neon := gemini.ModelInfo{ModelID: "e6fa609c3fa255c0", ModelName: "gemini-4-neon"}

	if got := noFrameTimeoutForAttempt(defaultCfg, hydrogen, "low"); got != 30*time.Second {
		t.Fatalf("hydrogen low expected 30s, got %v", got)
	}
	if got := noFrameTimeoutForAttempt(defaultCfg, hydrogen, "high"); got != 180*time.Second {
		t.Fatalf("hydrogen high expected 180s, got %v", got)
	}
	if got := noFrameTimeoutForAttempt(defaultCfg, neon, "low"); got != 45*time.Second {
		t.Fatalf("neon low expected 45s, got %v", got)
	}
	if got := noFrameTimeoutForAttempt(defaultCfg, neon, "medium"); got != 180*time.Second {
		t.Fatalf("neon medium expected 180s, got %v", got)
	}
	if got := noFrameTimeoutForAttempt(defaultCfg, neon, "high"); got != 180*time.Second {
		t.Fatalf("neon high expected 180s, got %v", got)
	}
	if got := noFrameTimeoutForAttempt(explicitCfg, hydrogen, "low"); got != 180*time.Second {
		t.Fatalf("explicit BRIDGE_NO_FRAME_TIMEOUT_MS must be respected verbatim, got %v", got)
	}
	if got := noFrameTimeoutForAttempt(smallCfg, hydrogen, "low"); got != 50*time.Millisecond {
		t.Fatalf("small timeout must not be inflated, got %v", got)
	}
}

func TestDynamicAccountGetsScheduledTimersAndDecollideSkipsZero(t *testing.T) {
	now := time.Now()
	srv := &Server{clock: func() time.Time { return now }}
	srv.SetCfg(config.Config{
		RotateCookiesEvery: 9 * time.Minute,
		ModelRefreshEvery:  30 * time.Minute,
	})
	a1 := &AccountState{ID: "dyn1"}
	a2 := &AccountState{ID: "dyn2"}
	srv.SetAccounts([]*AccountState{a1, a2})

	// Decollide on unscheduled (zero) timers must leave them zero rather than
	// setting year-0001 timestamps.
	srv.decollideRotationTimers()
	if !a1.NextRotateAt.IsZero() || !a2.NextRotateAt.IsZero() {
		t.Fatalf("decollideRotationTimers modified zero timers: a1=%v a2=%v", a1.NextRotateAt, a2.NextRotateAt)
	}

	// RunSchedulerStep initializes both NextRotateAt and NextModelRefAt for
	// accounts added after startup.
	srv.RunSchedulerStep(context.Background(), now)
	a1.mu.RLock()
	rot1, ref1 := a1.NextRotateAt, a1.NextModelRefAt
	a1.mu.RUnlock()
	if rot1.IsZero() || ref1.IsZero() {
		t.Fatalf("expected RunSchedulerStep to initialize dynamic account timers, got rot=%v ref=%v", rot1, ref1)
	}
}

func TestAccountDisabledAndHTTP5xxFailOverToHealthyPoolAccount(t *testing.T) {
	srv := newStallTestServer()

	disabledMidFlight := &mockUpstream{
		streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			return nil, transport.NewUpstreamError("account credentials were replaced; retry the request", 503, "account_disabled", nil, nil)
		},
	}
	http500 := &mockUpstream{
		streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			return &gemini.StreamResponse{Status: 503}, nil
		},
	}
	healthy := clientFromFrames([]gemini.Frame{
		makeFrame("pool-survived", "", true, testModel.ModelID),
	})

	a1 := &AccountState{ID: "acct1", Email: "a1@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), Client: disabledMidFlight}
	a2 := &AccountState{ID: "acct2", Email: "a2@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), Client: http500}
	a3 := &AccountState{ID: "acct3", Email: "a3@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), Client: healthy}
	srv.SetAccounts([]*AccountState{a1, a2, a3})

	body := map[string]any{
		"model":    testModel.ModelName,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, body, AuthScope{OK: true})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "pool-survived") {
		t.Fatalf("expected pool failover across account_disabled + 503 accounts to succeed, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestExpiredAccountExcludedFromAccountReadyAndStatusClassification(t *testing.T) {
	now := time.Now()
	srv := newStallTestServer()
	srv.clock = func() time.Time { return now }

	expired := &AccountState{
		ID:        "exp1",
		Models:    []gemini.ModelInfo{testModel},
		ModelsAt:  now,
		LastError: "upstream HTTP 401",
		LastFail:  now.Add(-5 * time.Minute), // past 60s cooling window
	}
	if _, ok := srv.accountReady(expired, testModel.ModelName); ok {
		t.Fatalf("expired account must not be ready after cooling window elapses")
	}
	if st, _ := accountStatusLocked(expired, now); st != StatusExpired {
		t.Fatalf("expected StatusExpired for upstream HTTP 401, got %q", st)
	}

	// Bare "1016" in an ordinary message must not false-positive as expired.
	transient := &AccountState{
		ID:        "tr1",
		Models:    []gemini.ModelInfo{testModel},
		ModelsAt:  now,
		Fails:     1,
		LastError: "read 1016 bytes before unexpected EOF",
		LastFail:  now.Add(-5 * time.Minute),
	}
	if st, _ := accountStatusLocked(transient, now); st == StatusExpired {
		t.Fatalf("bare 1016 byte count must not be classified as StatusExpired")
	}

	bannedByCode := &AccountState{
		ID:          "ban1",
		ModelsError: transport.NewUpstreamError("upstream rejected this account (code 1021): access denied", 403, "account_banned", nil, nil),
	}
	if st, _ := accountStatusLocked(bannedByCode, now); st != StatusQuarantined {
		t.Fatalf("expected StatusQuarantined for account_banned ModelsError, got %q", st)
	}
}

func TestStopAtIndexZeroAndMaxTokensWithToolsInStreamingChat(t *testing.T) {
	srv := newStallTestServer()

	// 1. Stop sequence at byte 0 in streaming mode must return 200 with finish_reason="stop", not 502 empty_upstream_response.
	stopClient := clientFromFrames([]gemini.Frame{
		makeFrame("STOP rest of answer", "", true, testModel.ModelID),
	})
	a1 := &AccountState{ID: "acct1", Email: "a1@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), Client: stopClient}
	srv.SetAccounts([]*AccountState{a1})

	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, map[string]any{
		"model":    testModel.ModelName,
		"stream":   true,
		"stop":     []any{"STOP"},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, AuthScope{OK: true})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Fatalf("expected 200 with finish_reason=stop on stop-at-index-0 stream, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Small max_tokens on a streaming tool-call request must not truncate the raw tool JSON envelope into a 502 tool_call_error.
	toolClient := clientFromFrames([]gemini.Frame{
		makeFrame(`{"tool_calls":[{"name":"read_file","arguments":{"path":"/very/long/path/that/exceeds/five/tokens/easily.txt"}}]}`, "", true, testModel.ModelID),
	})
	a1.Client = toolClient
	rec2 := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec2, map[string]any{
		"model":      testModel.ModelName,
		"stream":     true,
		"max_tokens": float64(4),
		"messages":   []any{map[string]any{"role": "user", "content": "read file"}},
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
					},
				},
			},
		},
	}, AuthScope{OK: true})
	if rec2.Code != 200 || !strings.Contains(rec2.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected 200 tool_calls even with small max_tokens, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestPrepareGenerationFallsBackOnRateLimitedFirstCandidate(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.AccountTokens = nil
	cfg.AccountStrategy = "fill-first"
	cfg.MaxConcurrentPerAccount = 1
	srv := New(cfg)

	healthy := clientFromFrames([]gemini.Frame{
		makeFrame("from-acct2", "", true, testModel.ModelID),
	})
	a1 := &AccountState{ID: "acct1", Email: "a1@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), activeStreams: 1, Client: healthy}
	a2 := &AccountState{ID: "acct2", Email: "a2@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(), activeStreams: 0, Client: healthy}
	srv.SetAccounts([]*AccountState{a1, a2})

	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, map[string]any{
		"model":    testModel.ModelName,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, AuthScope{OK: true})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "from-acct2") {
		t.Fatalf("expected prepareGeneration to fall back to acct2 when acct1 is at concurrency limit, got %d: %s", rec.Code, rec.Body.String())
	}
}
