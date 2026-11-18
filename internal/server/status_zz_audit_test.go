package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

const zzReal1016Msg = "session is unauthenticated or the saved cookies have expired (code 1016) — run `gsb --login <name>` to sign in again and re-import the account"

// 2: a recorded auth failure is sticky — a successful request clears only Fails.
func TestZZAuditAuthFailureStickyAfterSuccess(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	acct.Quota = []gemini.QuotaBucket{{ID: "5h", Label: "5-hour", Total: 100, Remaining: 99}}
	srv.SetAccounts([]*AccountState{acct})

	srv.markAccountFailure(acct, zzReal1016Msg)
	st, reason := AccountStatus(acct, srv.now())
	fmt.Printf("AUDIT0 after auth failure: status=%q reason=%q\n", st, reason)

	rec := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame("hello", "", true, testModel.ModelID),
	}), nil)
	acct.mu.RLock()
	fails, lastErr := acct.Fails, acct.LastError
	acct.mu.RUnlock()
	fmt.Printf("AUDIT1 chat http=%d fails=%d lastError=%q\n", rec.Code, fails, lastErr)

	// Well past cooling: only a recapture or probe can clear it.
	later := srv.now().Add(2 * time.Hour)
	st, reason = AccountStatus(acct, later)
	fmt.Printf("AUDIT2 +2h after successful traffic: status=%q reason=%q\n", st, reason)

	srv.RefreshAccount(context.Background(), acct, true)
	st, reason = AccountStatus(acct, later)
	fmt.Printf("AUDIT3 after scheduler refresh (healthy registry read): status=%q reason=%q\n", st, reason)
	if st == StatusExpired {
		fmt.Println("AUDIT CONFIRMED-2: serving account still reports expired")
	}
}

// 3: probeSingleAccount clears Fails/LastError but never clears quarantine.
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
	q, qr, fails, lastErr, surfaces := acct.Quarantined, acct.QuarantineReason, acct.Fails, acct.LastError, fmt.Sprint(acct.Surfaces)
	acct.mu.RUnlock()
	fmt.Printf("AUDIT4 after successful probe: quarantined=%v reason=%q fails=%d lastError=%q surfaces=%s\n",
		q, qr, fails, lastErr, surfaces)
	st, reason := AccountStatus(acct, srv.now())
	fmt.Printf("AUDIT5 status after successful probe: status=%q reason=%q\n", st, reason)
	if q {
		fmt.Println("AUDIT CONFIRMED-3: healthy probe still reports quarantined")
	}
}
