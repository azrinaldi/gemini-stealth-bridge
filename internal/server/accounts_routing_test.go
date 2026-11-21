package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

func TestRoutingReadinessBeforeRankOrCache(t *testing.T) {
	for _, strategy := range []string{"lru", "fill-first", "rotate"} {
		t.Run(strategy, func(t *testing.T) {
			now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
			for _, blocked := range []struct {
				name string
				set  func(*AccountState)
			}{
				{"missing-model", func(a *AccountState) { a.Models = []gemini.ModelInfo{{ModelName: "gemini-flash-lite"}} }},
				{"rpm", func(a *AccountState) { a.reqTimestamps = []time.Time{now} }},
				{"concurrency", func(a *AccountState) { a.activeStreams = 1 }},
				{"cooldown", func(a *AccountState) { a.LastFail = now }},
				{"quota", func(a *AccountState) { a.Quota = []gemini.QuotaBucket{routingSpentQuota("weekly", now.Add(time.Hour))} }},
				{"disabled", func(a *AccountState) { a.Disabled = true }},
				{"quarantined", func(a *AccountState) { a.Quarantined = true }},
			} {
				t.Run(blocked.name, func(t *testing.T) {
					cfg := routingTestConfig(t)
					cfg.AccountStrategy = strategy
					cfg.SessionCache = true
					srv := New(cfg)
					t.Cleanup(srv.FlushSessionCacheNow)
					srv.clock = func() time.Time { return now }
					preferred := &AccountState{ID: "preferred", Models: []gemini.ModelInfo{testModel}, LastUsed: now.Add(-time.Hour)}
					ready := &AccountState{ID: "ready", Models: []gemini.ModelInfo{testModel}, LastUsed: now}
					srv.SetAccounts([]*AccountState{preferred, ready})
					history := []map[string]any{
						{"role": "user", "content": "Question"},
						{"role": "assistant", "content": "Answer"},
						{"role": "user", "content": "Follow-up"},
					}
					hasher := prompt.NewHistoryHasher(history, nil, nil)
					srv.putSessionCache(hasher.HashPrefix(preferred.ID, testModel.ModelName, 2), []any{"cid", "rid", "rcid"})
					blocked.set(preferred)

					if got := srv.resolveChatAccount(srv.Accounts(), AuthScope{OK: true, Admin: true}, testModel.ModelName, hasher); got != ready {
						t.Fatalf("primary account = %v, want %s", got, ready.ID)
					}
					// Start rotation before preferred so it is considered first for failover.
					srv.setRotateCursor(ready.ID)
					got, model := srv.pickFallbackAccount(srv.Accounts(), nil, "", testModel.ModelName, hasher)
					if got != ready || model == nil || model.ModelName != testModel.ModelName {
						t.Fatalf("fallback account/model = %v/%v, want %s/%s", got, model, ready.ID, testModel.ModelName)
					}
					// Selection charges the destination's only RPM slot.
					if got, _ := srv.pickFallbackAccount(srv.Accounts(), nil, "", testModel.ModelName, hasher); got != nil {
						t.Fatalf("second fallback = %v, want none after RPM reservation", got)
					}
				})
			}
		})
	}
}

func TestRoutingBlockedPinnedAndSingleAccount(t *testing.T) {
	for _, blocked := range []struct {
		code string
		set  func(*AccountState)
	}{
		{"account_disabled", func(a *AccountState) { a.Disabled = true }},
		{"account_banned", func(a *AccountState) { a.Quarantined = true }},
	} {
		t.Run(blocked.code, func(t *testing.T) {
			cfg := routingTestConfig(t)
			cfg.APIKey = "pool-test-key"
			cfg.QuotaPreflight = "off"
			srv := New(cfg)
			account := &AccountState{ID: "paused", AccountGroup: "team", Client: &mockUpstream{
				modelsFn: func(context.Context, bool) (*gemini.AvailableModelsResult, error) {
					t.Fatal("blocked account triggered model discovery")
					return nil, nil
				},
			}}
			blocked.set(account)
			srv.SetAccounts([]*AccountState{account})
			poolReq := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			poolReq.Header.Set("Authorization", "Bearer pool-test-key")
			pool := srv.ResolveAuthScope(poolReq)
			if !pool.OK {
				t.Fatal("pool key was not authorized")
			}
			for name, scope := range map[string]AuthScope{
				"single-pool": pool,
				"pinned":      {OK: true, AccountID: account.ID},
				"group":       {OK: true, Group: account.AccountGroup},
			} {
				t.Run(name, func(t *testing.T) {
					_, err := srv.prepareGeneration(context.Background(), map[string]any{"model": "gemini-flash-lite"}, scope, nil, nil, nil)
					routingRequireError(t, err, 403, blocked.code)
				})
			}
		})
	}
}

func TestRoutingPreservesModelAndRateErrors(t *testing.T) {
	for _, strategy := range []string{"lru", "fill-first", "rotate"} {
		t.Run(strategy, func(t *testing.T) {
			now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
			cfg := routingTestConfig(t)
			cfg.AccountStrategy = strategy
			srv := New(cfg)
			srv.clock = func() time.Time { return now }
			missing := &AccountState{ID: "missing", Models: []gemini.ModelInfo{{ModelName: "gemini-flash-lite"}}, reqTimestamps: []time.Time{now}}
			limited := &AccountState{ID: "limited", Models: []gemini.ModelInfo{testModel}, reqTimestamps: []time.Time{now}}
			srv.SetAccounts([]*AccountState{missing, limited})
			for _, scope := range []AuthScope{{OK: true, Admin: true}, {OK: true, AccountID: limited.ID}} {
				_, err := srv.prepareGeneration(context.Background(), map[string]any{"model": "unknown-model"}, scope, nil, nil, nil)
				routingRequireError(t, err, 404, "model_not_found")
				_, err = srv.prepareGeneration(context.Background(), map[string]any{"model": testModel.ModelName}, scope, nil, nil, nil)
				ue := routingRequireError(t, err, 429, "rate_limit_error")
				if ue.Headers["retry-after"] != "60" {
					t.Fatalf("retry-after = %q, want 60", ue.Headers["retry-after"])
				}
			}
		})
	}
}

func TestRoutingQuotaDiagnosticsMatchActiveWindows(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	srv := New(routingTestConfig(t))
	srv.clock = func() time.Time { return now }
	for _, tc := range []struct {
		name  string
		model string
		quota []gemini.QuotaBucket
		reset time.Time
	}{
		{"rolling", "gemini-pro", []gemini.QuotaBucket{routingSpentQuota("5h", now.Add(time.Hour))}, now.Add(time.Hour)},
		{"weekly", "gemini-carbon", []gemini.QuotaBucket{routingSpentQuota("weekly", now.Add(48*time.Hour))}, now.Add(48 * time.Hour)},
		{"weekly-with-family", "gemini-carbon", []gemini.QuotaBucket{
			routingSpentQuota("weekly", now.Add(time.Hour)),
			{ID: "4", Label: "Gemini Pro", Total: 100, ResetAt: now.Add(2 * time.Hour).Format(time.RFC3339)},
		}, now.Add(time.Hour)},
		{"expired", "gemini-pro", []gemini.QuotaBucket{routingSpentQuota("5h", now)}, time.Time{}},
		{"unrelated", "gemini-pro", []gemini.QuotaBucket{{ID: "x", Label: "Gemini Flash", Total: 100, ResetAt: now.Add(time.Hour).Format(time.RFC3339)}}, time.Time{}},
		{"lite", "gemini-flash-lite", []gemini.QuotaBucket{routingSpentQuota("weekly", now.Add(time.Hour))}, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acct := &AccountState{ID: tc.name, Quota: tc.quota, Client: &mockUpstream{}}
			err := srv.classifyEmpty(context.Background(), acct, tc.model)
			lookup := srv.lookupQuotaResetHeaders(context.Background(), acct, tc.model)
			if tc.reset.IsZero() {
				routingRequireError(t, err, 502, "empty_upstream_response")
				if lookup != nil {
					t.Fatalf("lookup headers = %v, want nil", lookup)
				}
				return
			}
			ue := routingRequireError(t, err, 429, "rate_limit_error")
			for name, headers := range map[string]map[string]string{"classify": ue.Headers, "lookup": lookup} {
				if headers["x-resets-at"] != tc.reset.Format(time.RFC3339) || headers["retry-after"] != strconv.Itoa(int(tc.reset.Sub(now).Seconds())) {
					t.Fatalf("%s headers = %v, want reset at %s", name, headers, tc.reset)
				}
			}
		})
	}
}

func TestRoutingPrefersEligibleContinuation(t *testing.T) {
	for _, strategy := range []string{"lru", "fill-first", "rotate"} {
		t.Run(strategy, func(t *testing.T) {
			cfg := routingTestConfig(t)
			cfg.AccountStrategy = strategy
			cfg.SessionCache = true
			srv := New(cfg)
			t.Cleanup(srv.FlushSessionCacheNow)
			first := &AccountState{ID: "first", Models: []gemini.ModelInfo{testModel}}
			cached := &AccountState{ID: "cached", Models: []gemini.ModelInfo{testModel}, LastUsed: time.Now()}
			srv.SetAccounts([]*AccountState{first, cached})
			hasher := prompt.NewHistoryHasher([]map[string]any{
				{"role": "user", "content": "Question"},
				{"role": "assistant", "content": "Answer"},
				{"role": "user", "content": "Follow-up"},
			}, nil, nil)
			srv.putSessionCache(hasher.HashPrefix(cached.ID, testModel.ModelName, 2), []any{"cid", "rid", "rcid"})
			if got := srv.resolveChatAccount(srv.Accounts(), AuthScope{OK: true, Admin: true}, testModel.ModelName, hasher); got != cached {
				t.Fatalf("primary account = %v, want cached eligible account", got)
			}
		})
	}
}

func routingTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Load()
	cfg.HomeDir = t.TempDir()
	cfg.AccountTokens = nil
	cfg.GroupTokens = nil
	cfg.AccountPriority = nil
	cfg.RejectDowngrade = true
	cfg.QuotaPreflight = "strict"
	cfg.MaxRPMPerAccount = 1
	cfg.MaxConcurrentPerAccount = 1
	cfg.SessionCache = false
	return cfg
}

func routingSpentQuota(window string, reset time.Time) gemini.QuotaBucket {
	return gemini.QuotaBucket{ID: window, Label: window, Total: 100, ResetAt: reset.Format(time.RFC3339)}
}

func routingRequireError(t *testing.T, err error, status int, code string) *transport.UpstreamError {
	t.Helper()
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != status || ue.Code != code {
		t.Fatalf("error = %#v, want %d %s", err, status, code)
	}
	return ue
}
