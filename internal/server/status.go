package server

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

// Account status values reported by /v1/accounts and `gsb --accounts`.
//
// The point of a single computed status is that a caller should not have to
// cross-reference quarantined, fails, quota, and surfaces to work out whether an
// account can serve traffic. Each value answers "why not, or is it fine".
const (
	// StatusOK means the account is loaded, authenticated, and not known to be limited.
	StatusOK = "ok"
	// StatusQuarantined means upstream restricted the account (ban, TOS violation).
	StatusQuarantined = "quarantined"
	// StatusExpired means the saved session is no longer valid and needs a fresh login.
	StatusExpired = "expired"
	// StatusExhausted means a real quota window is fully spent.
	StatusExhausted = "exhausted"
	// StatusCooling means a recent failure put the account in a short cooldown.
	StatusCooling = "cooling"
	// StatusDegraded means the account is usable but has recorded recent failures.
	StatusDegraded = "degraded"
	// StatusUnknown means nothing is known yet about this account's quota because
	// no upstream check has run in this process. Reporting "ok" here would be a
	// guess: a fresh process reading account files has no quota data, and claiming
	// health it has not verified is exactly the confusion this status is meant to
	// remove.
	StatusUnknown = "unknown"
	// StatusDisabled means the operator paused this account in the control plane.
	StatusDisabled = "disabled"
)

// AccountStatus derives the status and a short human reason for one account.
// The reason is written for an operator reading a table, not for a parser.
// It is exported so the CLI can report the same status the HTTP API does, instead
// of reimplementing the rules and drifting from them.
func AccountStatus(a *AccountState, now time.Time) (status string, reason string) {
	if a == nil {
		return StatusUnknown, "account unavailable"
	}
	// Best-effort cleanup: status evaluation is a natural point to drop expired
	// synthetic cooldowns and their strike counters. If TryLock succeeds, prune
	// and evaluate under that single write lock; otherwise fall back to a single
	// RLock (the read-only evaluator ignores expired entries).
	if a.mu.TryLock() {
		pruneExpiredSyntheticLocked(a, now)
		status, reason = accountStatusLocked(a, now)
		a.mu.Unlock()
		return status, reason
	}
	a.mu.RLock()
	status, reason = accountStatusLocked(a, now)
	a.mu.RUnlock()
	return status, reason
}

// accountStatusLocked evaluates status and reason for a with a.mu already held
// (read or write). It must never acquire or release any lock and never mutate a.
func accountStatusLocked(a *AccountState, now time.Time) (status string, reason string) {
	if a == nil {
		return StatusUnknown, "account unavailable"
	}
	if a.Guest {
		return StatusOK, "anonymous; upstream model and per-IP availability may change"
	}
	if a.Disabled {
		return StatusDisabled, "paused by operator"
	}
	if a.Quarantined {
		if strings.TrimSpace(a.QuarantineReason) == "" {
			return StatusQuarantined, "restricted upstream"
		}
		return StatusQuarantined, a.QuarantineReason
	}
	if looksLikeAccountBan(a.LastError) || isModelsBanError(a.ModelsError) {
		reason := strings.TrimSpace(a.QuarantineReason)
		if reason == "" && a.ModelsError != nil {
			reason = a.ModelsError.Error()
		}
		if reason == "" {
			reason = truncateReason(a.LastError)
		}
		if reason == "" {
			reason = "restricted upstream"
		}
		return StatusQuarantined, reason
	}

	// An auth failure means the saved session cannot be used at all until a new
	// login, which is more severe than a quota problem and should not be hidden
	// behind one.
	if looksLikeAuthFailure(a.LastError) || isModelsAuthError(a.ModelsError) {
		return StatusExpired, "session expired; run `gsb --login <name>`"
	}

	if label, resetAt, ok := exhaustedBucket(a.Quota, now); ok {
		if resetAt.IsZero() {
			return StatusExhausted, label + " quota spent"
		}
		return StatusExhausted, label + " quota spent until " + resetAt.UTC().Format(time.RFC3339)
	}

	// A long cooldown means we already retried and the limit is still there, which
	// is the signal that this is the weekly window rather than the 5-hour one.
	// Only an active synthetic bucket justifies reporting exhausted: once its
	// ResetAt has passed, the strike counter is stale state, not a live limit.
	for subID, strikes := range a.subStrikes {
		if strikes < 3 {
			continue
		}
		active := false
		for _, q := range a.Quota {
			if q.ID == subID && syntheticCooldownActive(q, now) {
				active = true
				break
			}
		}
		if !active {
			continue
		}
		name := strings.TrimPrefix(subID, "sub:")
		return StatusExhausted, name + " still exhausted after " + strconv.Itoa(strikes) + " retries"
	}

	if !a.LastFail.IsZero() && now.Sub(a.LastFail) < fillFirstFailCooldown {
		return StatusCooling, "recent failure: " + truncateReason(a.LastError)
	}

	if a.Fails > 0 {
		return StatusDegraded, truncateReason(a.LastError)
	}

	if len(a.Quota) == 0 {
		return StatusUnknown, "no quota check yet; run with --accounts-live or query /v1/accounts on the running daemon"
	}

	return StatusOK, ""
}

// syntheticCooldownActive reports whether a synthetic substitution bucket is
// still within its cooldown window. A missing or malformed ResetAt is treated as
// expired so stale state cannot keep an account excluded.
func syntheticCooldownActive(q gemini.QuotaBucket, now time.Time) bool {
	if !q.Synthetic && !strings.HasPrefix(q.ID, "sub:") {
		return false
	}
	if q.ResetAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, q.ResetAt)
	return err == nil && t.After(now)
}

// exhaustedBucket reports the first real (non-synthetic) quota bucket that is
// fully spent and has not reset yet. Synthetic buckets are excluded because they
// describe a per-model retry cooldown, not an upstream quota window.
func exhaustedBucket(quota []gemini.QuotaBucket, now time.Time) (label string, resetAt time.Time, ok bool) {
	for _, q := range quota {
		if q.Synthetic || strings.HasPrefix(q.ID, "sub:") || q.Unlimited {
			continue
		}
		if q.Total <= 0 || q.Remaining > 0 {
			continue
		}
		var reset time.Time
		if q.ResetAt != "" {
			if t, err := time.Parse(time.RFC3339, q.ResetAt); err == nil {
				reset = t
			}
		}
		if !reset.IsZero() && !reset.After(now) {
			continue // already reset; the next refresh will clear it
		}
		label := strings.TrimSpace(q.Label)
		if label == "" {
			label = q.ID
		}
		return label, reset, true
	}
	return "", time.Time{}, false
}

// looksLikeAccountBan reports whether a recorded failure was upstream restricting
// the account itself. Such a verdict survives a healthy registry read: the account
// can list models and still be refused for every generation.
func isModelsBanError(err error) bool {
	if err == nil {
		return false
	}
	var ue *transport.UpstreamError
	if errors.As(err, &ue) && ue != nil && ue.Code == "account_banned" {
		return true
	}
	return looksLikeAccountBan(err.Error())
}

func isModelsAuthError(err error) bool {
	if err == nil {
		return false
	}
	var ue *transport.UpstreamError
	if errors.As(err, &ue) && ue != nil && (ue.Code == "upstream_auth_error" || ue.Code == "auth_redirect") {
		return true
	}
	return looksLikeAuthFailure(err.Error())
}

func looksLikeAccountBan(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	if m == "" {
		return false
	}
	for _, needle := range []string{
		"account_banned",
		"tos_violation",
		"permission_denied",
		"quarantin",
		"upstream rejected this account (code ",
	} {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}

func looksLikeAuthFailure(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	if m == "" {
		return false
	}
	for _, needle := range []string{
		"upstream_auth_error",
		"auth_redirect",
		"redirected to google sign-in",
		"code 1016",
		"session is unauthenticated",
		"session expired",
		"snlm0e",
		"sign in again",
		"invalid api key",
		"upstream http 401",
		"upstream http 403",
		"file upload failed with http 401",
		"file upload failed with http 403",
		"cookie rotation rejected (http 401)",
		"cookie rotation rejected (http 403)",
	} {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}

func truncateReason(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown error"
	}
	if len(s) > 160 {
		return s[:157] + "..."
	}
	return s
}

func cloneQuotaBuckets(src []gemini.QuotaBucket) []gemini.QuotaBucket {
	if src == nil {
		return nil
	}
	out := make([]gemini.QuotaBucket, len(src))
	copy(out, src)
	for i := range out {
		if out[i].UsagePercentage != nil {
			v := *out[i].UsagePercentage
			out[i].UsagePercentage = &v
		}
	}
	return out
}

// AccountSnapshot returns the fields the CLI needs, copied under the account lock.
// Exposing a snapshot instead of the lock keeps the mutex private while letting
// other packages read consistent values.
func AccountSnapshot(a *AccountState) (email string, quota []gemini.QuotaBucket) {
	if a == nil {
		return "", nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Email, cloneQuotaBuckets(a.Quota)
}
