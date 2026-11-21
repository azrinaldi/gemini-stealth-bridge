package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

// Item 2.1: Recursive RLock deadlock prevention in accountStatusMap / AccountStatus.
func TestAccountStatusMapNoRecursiveRLockDeadlock(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	acct.mu.Lock()
	acct.Quota = []gemini.QuotaBucket{
		{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 1000, ResetAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)},
		{ID: "sub:gemini-pro", Label: "gemini-pro", Total: 1, Remaining: 0, ResetAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339), Synthetic: true, Strikes: 1},
	}
	acct.QuotaAt = time.Now()
	acct.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := make(chan struct{})
	var wg sync.WaitGroup

	// 4 concurrent readers calling accountStatusMap and AccountStatus.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 150; i++ {
				if ctx.Err() != nil {
					return
				}
				now := time.Now()
				m := srv.accountStatusMap(acct, now)
				if _, ok := m["status"].(string); !ok {
					t.Errorf("missing status in accountStatusMap: %+v", m)
					return
				}
				st, _ := AccountStatus(acct, now)
				if st == "" {
					t.Errorf("empty AccountStatus")
					return
				}
			}
		}()
	}

	// 2 concurrent writers acquiring acct.mu.Lock().
	for w := range 2 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			for i := 0; i < 100; i++ {
				if ctx.Err() != nil {
					return
				}
				if (id+i)%2 == 0 {
					srv.recordModelExhausted(acct, "gemini-pro")
				} else {
					srv.clearModelExhausted(acct, "gemini-pro")
				}
				acct.mu.Lock()
				acct.Fails = i % 2
				acct.mu.Unlock()
			}
		}(w)
	}

	close(start)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("deadlock detected between accountStatusMap/AccountStatus and acct.mu.Lock()")
	}
}

func TestAccountStatusLockedMatchesAccountStatus(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		setup  func(a *AccountState)
		wantSt string
	}{
		{
			name:   "disabled",
			setup:  func(a *AccountState) { a.Disabled = true },
			wantSt: StatusDisabled,
		},
		{
			name:   "quarantined",
			setup:  func(a *AccountState) { a.Quarantined = true; a.QuarantineReason = "TOS_VIOLATION" },
			wantSt: StatusQuarantined,
		},
		{
			name:   "expired",
			setup:  func(a *AccountState) { a.LastError = "upstream_auth_error: code 1016" },
			wantSt: StatusExpired,
		},
		{
			name: "exhausted_real",
			setup: func(a *AccountState) {
				a.Quota = []gemini.QuotaBucket{{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 0, ResetAt: now.Add(time.Hour).Format(time.RFC3339)}}
			},
			wantSt: StatusExhausted,
		},
		{
			name: "exhausted_synthetic_3_strikes",
			setup: func(a *AccountState) {
				a.Quota = []gemini.QuotaBucket{{ID: "sub:gemini-pro", Label: "gemini-pro", Total: 1, Remaining: 0, ResetAt: now.Add(10 * time.Minute).Format(time.RFC3339), Synthetic: true, Strikes: 3}}
				a.subStrikes = map[string]int{"sub:gemini-pro": 3}
			},
			wantSt: StatusExhausted,
		},
		{
			name: "cooling",
			setup: func(a *AccountState) {
				a.Fails = 1
				a.LastFail = now.Add(-10 * time.Second)
				a.LastError = "upstream_transient"
			},
			wantSt: StatusCooling,
		},
		{
			name: "degraded",
			setup: func(a *AccountState) {
				a.Fails = 2
				a.LastFail = now.Add(-5 * time.Minute)
				a.LastError = "upstream_stalled"
			},
			wantSt: StatusDegraded,
		},
		{
			name:   "unknown",
			setup:  func(a *AccountState) {},
			wantSt: StatusUnknown,
		},
		{
			name: "ok",
			setup: func(a *AccountState) {
				a.Quota = []gemini.QuotaBucket{{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 1200, ResetAt: now.Add(time.Hour).Format(time.RFC3339)}}
			},
			wantSt: StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &AccountState{ID: "acct"}
			tc.setup(a)
			a.mu.RLock()
			gotSt, gotReason := accountStatusLocked(a, now)
			a.mu.RUnlock()
			extSt, extReason := AccountStatus(a, now)
			if gotSt != tc.wantSt || extSt != tc.wantSt {
				t.Fatalf("status mismatch: locked=%q external=%q want=%q", gotSt, extSt, tc.wantSt)
			}
			if gotReason != extReason {
				t.Fatalf("reason mismatch: locked=%q external=%q", gotReason, extReason)
			}
		})
	}
}

// lateFlushGuardWriter wraps httptest.ResponseRecorder and detects any Write/Flush
// after the HTTP handler has returned (which would panic on nil *bufio.Writer in net/http).
type lateFlushGuardWriter struct {
	rec       *httptest.ResponseRecorder
	closed    atomic.Bool
	lateWrite atomic.Int32
	lateFlush atomic.Int32
}

func (w *lateFlushGuardWriter) Header() http.Header { return w.rec.Header() }
func (w *lateFlushGuardWriter) Write(p []byte) (int, error) {
	if w.closed.Load() {
		w.lateWrite.Add(1)
	}
	return w.rec.Write(p)
}
func (w *lateFlushGuardWriter) WriteHeader(code int) { w.rec.WriteHeader(code) }
func (w *lateFlushGuardWriter) Flush() {
	if w.closed.Load() {
		w.lateFlush.Add(1)
	}
	w.rec.Flush()
}

// Item 2.2: Early error before EarlySSEGrace fires must join grace goroutine and never write/flush after return.
func TestGraceEarlyErrorBeforeGraceFiresNoPanicOrLeak(t *testing.T) {
	for _, tc := range []struct {
		name    string
		relaxed bool
		path    string
		payload map[string]any
	}{
		{
			name:    "chat",
			relaxed: false,
			path:    "/v1/chat/completions",
			payload: map[string]any{"model": "gemini-pro", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		},
		{
			name:    "responses",
			relaxed: true, // responses.go:716 gates grace on eagerStream || !RejectDowngrade in M1
			path:    "/v1/responses",
			payload: map[string]any{"model": "gemini-pro", "stream": true, "input": "hi"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, acct := newTestHarness(tc.relaxed, false)
			srv.UpdateCfg(func(c *config.Config) { c.EarlySSEGrace = 15 * time.Millisecond })
			acct.Client = &mockUpstream{
				streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
					time.Sleep(time.Millisecond)
					return nil, transport.NewUpstreamError("account banned", 403, "account_banned", nil, nil)
				},
			}

			for i := range 10 {
				acct.mu.Lock()
				acct.Quarantined = false
				acct.QuarantineReason = ""
				acct.mu.Unlock()

				gw := &lateFlushGuardWriter{rec: httptest.NewRecorder()}
				if tc.path == "/v1/chat/completions" {
					srv.HandleChat(context.Background(), gw, tc.payload, AuthScope{OK: true, AccountID: "acct"})
				} else {
					srv.HandleResponses(context.Background(), gw, tc.payload, AuthScope{OK: true, AccountID: "acct"})
				}
				gw.closed.Store(true)
				time.Sleep(25 * time.Millisecond)

				if gw.rec.Code != 403 {
					t.Fatalf("iter %d: expected 403, got %d (%s)", i, gw.rec.Code, gw.rec.Body.String())
				}
				if gw.lateWrite.Load() > 0 || gw.lateFlush.Load() > 0 {
					t.Fatalf("iter %d: detected write/flush after handler return: writes=%d flushes=%d", i, gw.lateWrite.Load(), gw.lateFlush.Load())
				}
			}

			// Also verify via real httptest.Server under -race.
			ts := httptest.NewServer(srv)
			defer ts.Close()
			rawBody, _ := json.Marshal(tc.payload)
			for i := range 10 {
				acct.mu.Lock()
				acct.Quarantined = false
				acct.QuarantineReason = ""
				acct.mu.Unlock()
				req, _ := http.NewRequest(http.MethodPost, ts.URL+tc.path, bytes.NewReader(rawBody))
				req.Header.Set("Authorization", "Bearer audit")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("http iter %d failed: %v", i, err)
				}
				_, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 403 {
					t.Fatalf("http iter %d expected 403, got %d", i, resp.StatusCode)
				}
			}
		})
	}
}

// Item 2.2: Concurrent streaming requests straddling EarlySSEGrace, keepalive ticks, success, error, and client cancel.
func TestGraceDeadlineStraddleAndKeepaliveStopNoRaceOrDeadlock(t *testing.T) {
	origInterval := defaultKeepaliveInterval
	defaultKeepaliveInterval = 2 * time.Millisecond
	t.Cleanup(func() { defaultKeepaliveInterval = origInterval })

	for _, tc := range []struct {
		name    string
		relaxed bool
		path    string
		payload map[string]any
	}{
		{
			name:    "chat",
			relaxed: false,
			path:    "/v1/chat/completions",
			payload: map[string]any{"model": "gemini-pro", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		},
		{
			name:    "responses",
			relaxed: true,
			path:    "/v1/responses",
			payload: map[string]any{"model": "gemini-pro", "stream": true, "input": "hi"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, acct := newTestHarness(tc.relaxed, false)
			srv.UpdateCfg(func(c *config.Config) {
				c.EarlySSEGrace = 5 * time.Millisecond
				c.MaxAttempts = 1
				c.MaxConcurrentPerAccount = 0
				c.MaxRPMPerAccount = 0
			})

			var seq atomic.Int32
			acct.Client = &mockUpstream{
				streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
					n := seq.Add(1)
					delay := time.Duration(2+(n%9)) * time.Millisecond
					select {
					case <-time.After(delay):
					case <-ctx.Done():
						return nil, transport.NewUpstreamError("cancelled", 499, "client_cancelled", nil, ctx.Err())
					}
					if n%5 == 0 {
						return nil, transport.NewUpstreamError("transient", 502, "upstream_transport_error", nil, nil)
					}
					_ = onFrame(&gemini.LineParseResult{
						CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
						Frames: []gemini.Frame{makeFrame("ok", "think", true, testModel.ModelID)},
					})
					return &gemini.StreamResponse{Status: 200}, nil
				},
			}

			ts := httptest.NewServer(srv)
			defer ts.Close()

			rawBody, _ := json.Marshal(tc.payload)
			var wg sync.WaitGroup
			for g := range 6 {
				wg.Add(1)
				go func(gid int) {
					defer wg.Done()
					for i := range 6 {
						reqCtx := context.Background()
						var cancel context.CancelFunc
						if (gid+i)%4 == 0 {
							reqCtx, cancel = context.WithTimeout(reqCtx, 6*time.Millisecond)
						}
						req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, ts.URL+tc.path, bytes.NewReader(rawBody))
						req.Header.Set("Authorization", "Bearer audit")
						resp, err := http.DefaultClient.Do(req)
						if err == nil {
							_, _ = io.ReadAll(resp.Body)
							resp.Body.Close()
						}
						if cancel != nil {
							cancel()
						}
					}
				}(g)
			}
			wg.Wait()
		})
	}
}

// Item 2.2: Failover reassigning account concurrently with EarlySSEGrace startStreamWith must not race on account.ID.
func TestGraceFailoverUpdatesActiveAccountIDWithoutRace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		relaxed bool
		path    string
		payload string
	}{
		{
			name:    "chat",
			relaxed: false,
			path:    "/v1/chat/completions",
			payload: `{"model":"gemini-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:    "responses",
			relaxed: true,
			path:    "/v1/responses",
			payload: `{"model":"gemini-pro","stream":true,"input":"hi"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Load()
			cfg.HomeDir = "/synthetic"
			cfg.RejectDowngrade = !tc.relaxed
			cfg.EarlySSEGrace = 8 * time.Millisecond
			cfg.MaxAttempts = 2
			cfg.MaxUpstreamAttempts = 2
			cfg.AccountStrategy = "fill-first"
			cfg.AccountPriority = []string{"acctA", "acctB"}
			srv := New(cfg)

			acctA := &AccountState{
				ID:       "acctA",
				Email:    "a@example.com",
				Models:   []gemini.ModelInfo{testModel},
				Capacity: 2,
				ModelsAt: time.Now(),
				Client: &mockUpstream{
					streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
						time.Sleep(7 * time.Millisecond)
						return nil, transport.NewUpstreamError("rate limited", 429, "rate_limit_error", nil, nil)
					},
				},
			}
			acctB := &AccountState{
				ID:       "acctB",
				Email:    "b@example.com",
				Models:   []gemini.ModelInfo{testModel},
				Capacity: 2,
				ModelsAt: time.Now(),
				Client: &mockUpstream{
					streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
						time.Sleep(20 * time.Millisecond)
						_ = onFrame(&gemini.LineParseResult{
							CID: "c2", RID: "r2", AnsweredModelID: testModel.ModelID,
							Frames: []gemini.Frame{makeFrame("from acctB", "", true, testModel.ModelID)},
						})
						return &gemini.StreamResponse{Status: 200}, nil
					},
				},
			}
			srv.SetAccounts([]*AccountState{acctA, acctB})

			ts := httptest.NewServer(srv)
			defer ts.Close()

			for i := range 8 {
				acctA.mu.Lock()
				acctA.Fails = 0
				acctA.LastFail = time.Time{}
				acctA.mu.Unlock()

				req, _ := http.NewRequest(http.MethodPost, ts.URL+tc.path, strings.NewReader(tc.payload))
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("iter %d Do failed: %v", i, err)
				}
				bodyBytes, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 || !strings.Contains(string(bodyBytes), "from acctB") {
					t.Fatalf("iter %d expected 200 with acctB output, got %d: %s", i, resp.StatusCode, string(bodyBytes))
				}
			}
		})
	}
}

// Item 2.3: Concurrent PATCH /v1/admin/config, POST /v1/admin/accounts/<id>/move, and concurrent readers.
func TestConfigPatchMoveAndConcurrentReadersNoRace(t *testing.T) {
	home := t.TempDir()
	cfg := config.Load()
	cfg.HomeDir = home
	cfg.EnvFile = filepath.Join(home, "config.env")
	cfg.WebUI = true
	cfg.MaxConcurrentPerAccount = 0
	cfg.MaxRPMPerAccount = 0
	srv := New(cfg)

	okClient := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			_ = onFrame(&gemini.LineParseResult{
				CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{makeFrame("ok", "thought", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	a1 := &AccountState{ID: "a1", Email: "1@e.com", Client: okClient, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	a2 := &AccountState{ID: "a2", Email: "2@e.com", Client: okClient, Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()}
	srv.SetAccounts([]*AccountState{a1, a2})

	start := make(chan struct{})
	var wg sync.WaitGroup

	// Writer 1: PATCH /v1/admin/config with persist: false.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		strategies := []string{"rotate", "lru", "fill-first"}
		preflights := []string{"strict", "advisory", "off"}
		for i := range 30 {
			patch := map[string]any{
				"accountStrategy":  strategies[i%len(strategies)],
				"accountPriority":  []string{"a2", "a1"},
				"maxRpmPerAccount": 10 + (i % 50),
				"liveThoughts":     i%2 == 0,
				"rejectDowngrade":  i%3 != 0,
				"quotaPreflight":   preflights[i%len(preflights)],
				"persist":          false,
			}
			raw, _ := json.Marshal(patch)
			req := httptest.NewRequest(http.MethodPatch, "/v1/admin/config", bytes.NewReader(raw))
			req.RemoteAddr = "127.0.0.1:41001"
			req.Host = "127.0.0.1:20133"
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Errorf("PATCH config failed: %d %s", rec.Code, rec.Body.String())
				return
			}
		}
	}()

	// Writer 2: Single goroutine calling POST /v1/admin/accounts/a1/move (avoids M5's SaveEnvFile race).
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := range 20 {
			dir := "down"
			if i%2 == 1 {
				dir = "up"
			}
			raw, _ := json.Marshal(map[string]any{"direction": dir})
			req := httptest.NewRequest(http.MethodPost, "/v1/admin/accounts/a1/move", bytes.NewReader(raw))
			req.RemoteAddr = "127.0.0.1:41002"
			req.Host = "127.0.0.1:20133"
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Errorf("move account failed: %d %s", rec.Code, rec.Body.String())
				return
			}
		}
	}()

	// Reader 1: GET /v1/admin/config, GET /v1/accounts, GET /v1/models.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := range 30 {
			for _, ep := range []string{"/v1/admin/config", "/v1/accounts", "/v1/models"} {
				req := httptest.NewRequest(http.MethodGet, ep, nil)
				req.RemoteAddr = "127.0.0.1:41003"
				req.Host = "127.0.0.1:20133"
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Errorf("GET %s iter %d failed: %d %s", ep, i, rec.Code, rec.Body.String())
					return
				}
			}
			srv.RunSchedulerStep(context.Background(), time.Now())
		}
	}()

	// Readers 2 & 3: POST /v1/chat/completions & POST /v1/responses.
	for _, ep := range []string{"/v1/chat/completions", "/v1/responses"} {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			<-start
			body := `{"model":"gemini-pro","messages":[{"role":"user","content":"hi"}]}`
			if path == "/v1/responses" {
				body = `{"model":"gemini-pro","input":"hi"}`
			}
			for i := range 20 {
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				req.RemoteAddr = "127.0.0.1:41004"
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Errorf("POST %s iter %d failed: %d %s", path, i, rec.Code, rec.Body.String())
					return
				}
			}
		}(ep)
	}

	close(start)
	wg.Wait()

	finalCfg := srv.Config()
	if finalCfg.AccountStrategy == "" {
		t.Fatalf("unexpected empty final AccountStrategy: %+v", finalCfg)
	}
}

// Item 2.4: Concurrent AccountState.Quota mutations (recordModelExhausted, clearModelExhausted, mergeAccountQuota)
// and concurrent unlocked readers (GET /v1/quota, preflight, classifyEmpty, lookupQuotaResetHeaders, AccountStatus).
func TestAccountQuotaConcurrentMutateAndReadNoRace(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	pct := 42.5
	acct.mu.Lock()
	acct.Quota = []gemini.QuotaBucket{
		{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 1000, UsagePercentage: &pct, ResetAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
	}
	acct.QuotaAt = time.Now()
	acct.Client = &mockUpstream{
		quotaFn: func(ctx context.Context) []gemini.QuotaBucket {
			p := 55.0
			return []gemini.QuotaBucket{
				{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 900, UsagePercentage: &p, ResetAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
			}
		},
	}
	acct.mu.Unlock()

	start := make(chan struct{})
	var wg sync.WaitGroup

	// Writer 1: recordModelExhausted / clearModelExhausted.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := range 60 {
			if i%2 == 0 {
				srv.recordModelExhausted(acct, "gemini-pro")
			} else {
				srv.clearModelExhausted(acct, "gemini-pro")
			}
		}
	}()

	// Writer 2: mergeAccountQuota (allocates spare slice capacity).
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := range 40 {
			p := float64(i)
			fresh := []gemini.QuotaBucket{
				{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 2400 - i, UsagePercentage: &p, ResetAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
			}
			out := srv.mergeAccountQuota(acct, fresh)
			if len(out) == 0 {
				t.Errorf("mergeAccountQuota returned empty slice")
				return
			}
			// Mutating caller's returned slice must not race with AccountState.Quota.
			out[0].Remaining = -999
		}
	}()

	// Reader 1: GET /v1/quota (json.Marshal reads after RUnlock).
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := range 40 {
			req := httptest.NewRequest(http.MethodGet, "/v1/quota", nil)
			req.Header.Set("Authorization", "Bearer audit")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Errorf("GET /v1/quota iter %d failed: %d %s", i, rec.Code, rec.Body.String())
				return
			}
		}
	}()

	// Reader 2: checkPreFlightQuotaInternal & lookupQuotaResetHeaders.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for range 60 {
			_ = srv.checkPreFlightQuotaInternal(acct, testModel, testModel.ModelName, false)
			_ = srv.lookupQuotaResetHeaders(context.Background(), acct, testModel.ModelName)
		}
	}()

	// Reader 3: classifyEmpty.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for range 40 {
			err := srv.classifyEmpty(context.Background(), acct, testModel.ModelName)
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("unexpected classifyEmpty result: %v", err)
				return
			}
		}
	}()

	// Reader 4: AccountStatus, accountStatusMap, AccountSnapshot.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for range 60 {
			_, _ = AccountStatus(acct, time.Now())
			_ = srv.accountStatusMap(acct, time.Now())
			_, snapQ := AccountSnapshot(acct)
			if len(snapQ) > 0 && snapQ[0].UsagePercentage != nil {
				*snapQ[0].UsagePercentage = -1 // mutating snapshot copy must not race
			}
		}
	}()

	close(start)
	wg.Wait()
}
