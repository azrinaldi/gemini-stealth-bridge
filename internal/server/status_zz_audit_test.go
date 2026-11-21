package server

import (
	"context"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

const zzReal1016Msg = "session is unauthenticated or the saved cookies have expired (code 1016) — run `gsb --login <name>` to sign in again and re-import the account"

// A recorded auth failure clears cleanly once a real request succeeds.
func TestZZAuditAuthFailureStickyAfterSuccess(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	acct.Quota = []gemini.QuotaBucket{{ID: "5h", Label: "5-hour", Total: 100, Remaining: 99}}
	srv.SetAccounts([]*AccountState{acct})

	srv.markAccountFailure(acct, zzReal1016Msg)
	st, _ := AccountStatus(acct, srv.now())
	if st != StatusExpired {
		t.Fatalf("after auth failure: got status=%q, want %q", st, StatusExpired)
	}

	rec := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame("hello", "", true, testModel.ModelID),
	}), nil)
	if rec.Code != 200 {
		t.Fatalf("expected 200 OK chat, got %d: %s", rec.Code, rec.Body.String())
	}
	acct.mu.RLock()
	fails, lastErr, lastFail, modelsErr := acct.Fails, acct.LastError, acct.LastFail, acct.ModelsError
	acct.mu.RUnlock()
	if fails != 0 || lastErr != "" || !lastFail.IsZero() || modelsErr != nil {
		t.Fatalf("expected auth error state cleared on success, got fails=%d lastErr=%q lastFail=%v modelsErr=%v",
			fails, lastErr, lastFail, modelsErr)
	}

	later := srv.now().Add(2 * time.Hour)
	if st, _ = AccountStatus(acct, later); st != StatusOK {
		t.Fatalf("+2h after successful traffic: got status=%q, want %q", st, StatusOK)
	}

	srv.RefreshAccount(context.Background(), acct, true)
	if st, _ = AccountStatus(acct, later); st != StatusOK {
		t.Fatalf("after scheduler refresh: got status=%q, want %q", st, StatusOK)
	}
}

// A healthy probe clears quarantine alongside failure state.
func TestZZAuditProbeDoesNotClearQuarantine(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	acct.Quota = []gemini.QuotaBucket{{ID: "5h", Label: "5-hour", Total: 100, Remaining: 99}}
	srv.SetAccounts([]*AccountState{acct})
	acct.mu.Lock()
	acct.Quarantined = true
	acct.QuarantineReason = "upstream rejected this account (code 1033)"
	acct.LastError = zzReal1016Msg
	acct.Fails = 3
	acct.mu.Unlock()

	acct.Client = &mockUpstream{} // everything healthy
	if err := srv.probeSingleAccount(context.Background(), acct); err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	acct.mu.RLock()
	q, qr, fails, lastErr, lastFail, modelsErr := acct.Quarantined, acct.QuarantineReason, acct.Fails, acct.LastError, acct.LastFail, acct.ModelsError
	gwSurface := acct.Surfaces["geminiWeb"]
	acct.mu.RUnlock()
	if q || qr != "" || fails != 0 || lastErr != "" || !lastFail.IsZero() || modelsErr != nil || gwSurface != "ok" {
		t.Fatalf("healthy probe did not clear quarantine/error state: q=%v qr=%q fails=%d lastErr=%q lastFail=%v modelsErr=%v surface=%q",
			q, qr, fails, lastErr, lastFail, modelsErr, gwSurface)
	}
	if st, reason := AccountStatus(acct, srv.now()); st != StatusOK {
		t.Fatalf("status after healthy probe = %q (%q), want %q", st, reason, StatusOK)
	}
}
