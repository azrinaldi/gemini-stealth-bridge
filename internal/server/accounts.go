package server

import (
	"context"
	"fmt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/toolbridge"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SetAccounts replaces the server's loaded account list (used by tests).
func (s *Server) SetAccounts(accts []*AccountState) {
	s.mu.Lock()
	s.accounts = accts
	s.tokenToAcct = make(map[string]string, 2*len(accts))
	for _, a := range accts {
		if a != nil && a.ID != "" {
			s.registerAccountTokensLocked(a.ID, a.ID)
		}
	}
	s.mu.Unlock()
}

// registerAccountTokensLocked records aliases only; the resolver decides when
// these aliases are accepted and handles explicit configured tokens.
func (s *Server) registerAccountTokensLocked(rawID, targetID string) {
	if rawID == "" || targetID == "" {
		return
	}
	s.tokenToAcct["gsb-"+rawID] = targetID
	s.tokenToAcct[rawID] = targetID
}

// Accounts returns a snapshot of the loaded accounts.
func (s *Server) Accounts() []*AccountState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*AccountState, len(s.accounts))
	copy(out, s.accounts)
	return out
}

// scopedAccounts returns the accounts the caller's scope may use. A group scope
// is limited to the accounts that carry the matching account group; an account
// scope is limited to that single account.
func (s *Server) scopedAccounts(scope AuthScope) []*AccountState {
	accts := s.Accounts()
	if scope.AccountID == "" && scope.Group == "" {
		return accts
	}
	var out []*AccountState
	for _, a := range accts {
		if a == nil {
			continue
		}
		if scope.AccountID != "" {
			if a.ID == scope.AccountID {
				out = append(out, a)
			}
			continue
		}
		a.mu.RLock()
		group := a.AccountGroup
		a.mu.RUnlock()
		if group == scope.Group {
			out = append(out, a)
		}
	}
	return out
}

// LoadCredentials loads and deduplicates account JSON files from AccountsDir and CredFile,
// falling back to live Chrome SQLite decryption if no saved files exist.
func (s *Server) LoadCredentials() int {
	s.adminImportMu.Lock()
	defer s.adminImportMu.Unlock()
	return s.loadCredentialsLocked()
}

func (s *Server) loadCredentialsLocked() int {
	cfg := s.cfgSnapshot()
	var rawList []*AccountState

	credFile := filepath.Join(cfg.HomeDir, "web-credentials.json")
	if entries, err := os.ReadDir(cfg.AccountsDir); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, fn := range names {
			id := strings.TrimSuffix(fn, ".json")
			if !reSafeID.MatchString(id) {
				continue
			}
			fullPath := filepath.Join(cfg.AccountsDir, fn)
			if st, err := s.loadAccountStateFromFile(id, fullPath); err == nil {
				rawList = append(rawList, st)
			}
		}
	}

	if st, err := s.loadAccountStateFromFile("default", credFile); err == nil {
		rawList = append(rawList, st)
	}

	if len(rawList) == 0 && cfg.CookieDB != "" {
		if dbPath := cookies.FindChromeCookieDB(cfg.CookieDB); dbPath != "" {
			if hdr, err := cookies.ReadChromeCookies(dbPath); err == nil && cookies.ExtractSAPISID(hdr) != "" {
				ac := gemini.NewAccountClient("chrome-live", hdr, "", s.Pool, s.Rotator)
				rawList = append(rawList, &AccountState{
					ID:     "chrome-live",
					Email:  "chrome-profile",
					Client: ac,
				})
			}
		}
	}

	deduped := dedupeAccounts(rawList, credFile)

	s.mu.Lock()
	existingByID := make(map[string]*AccountState, len(s.accounts))
	for _, a := range s.accounts {
		if a != nil && a.ID != "" {
			existingByID[a.ID] = a
		}
	}
	for i, st := range deduped {
		prev, ok := existingByID[st.ID]
		if !ok || prev == nil {
			continue
		}
		prev.mu.Lock()
		if prev.retired || !strings.EqualFold(prev.Email, st.Email) {
			prev.mu.Unlock()
			continue
		}
		recaptured := (!st.CapturedAt.IsZero() && st.CapturedAt.After(prev.CapturedAt)) ||
			(prev.Quarantined && !st.Quarantined)
		prev.FilePath = st.FilePath
		prev.CapturedAt = st.CapturedAt
		if !st.RotatedAt.IsZero() && st.RotatedAt.After(prev.RotatedAt) {
			prev.RotatedAt = st.RotatedAt
		}
		prev.ImportedFrom = st.ImportedFrom
		prev.ProfileID = st.ProfileID
		prev.AccountGroup = st.AccountGroup
		prev.ProxyURL = st.ProxyURL
		prev.Quarantined = st.Quarantined
		prev.QuarantineReason = st.QuarantineReason
		prev.Disabled = st.Disabled
		if len(st.Surfaces) > 0 {
			if prev.Surfaces == nil {
				prev.Surfaces = make(map[string]string, len(st.Surfaces))
			}
			for k, v := range st.Surfaces {
				prev.Surfaces[k] = v
			}
		}
		if recaptured {
			prev.Fails = 0
			prev.LastFail = time.Time{}
			prev.LastError = ""
			prev.ModelsError = nil
			clearSyntheticQuotaLocked(prev)
		}
		client := prev.Client
		prev.mu.Unlock()
		if ac, ok := client.(*gemini.AccountClient); ok {
			ac.ReloadFromDiskIfNewer()
			ac.SetRuntimeOptions(cfg.Locale, cfg.BLOverride, cfg.ProtocolMode, st.AccountGroup, st.ProxyURL)
		}
		deduped[i] = prev
	}
	if len(cfg.AccountPriority) > 0 {
		ranks := s.accountRanks(deduped)
		sort.SliceStable(deduped, func(i, j int) bool {
			return ranks[deduped[i].ID] < ranks[deduped[j].ID]
		})
	}
	s.accounts = deduped
	s.tokenToAcct = make(map[string]string, 2*len(rawList))
	byEmail := make(map[string]string, len(deduped))
	for _, a := range deduped {
		s.registerAccountTokensLocked(a.ID, a.ID)
		a.mu.RLock()
		email := strings.ToLower(strings.TrimSpace(a.Email))
		a.mu.RUnlock()
		if email != "" {
			byEmail[email] = a.ID
		}
	}
	for _, raw := range rawList {
		if targetID, ok := byEmail[strings.ToLower(strings.TrimSpace(raw.Email))]; ok {
			s.registerAccountTokensLocked(raw.ID, targetID)
		}
	}
	s.mu.Unlock()
	return len(deduped)
}

func (s *Server) loadAccountStateFromFile(id, path string) (*AccountState, error) {
	ac, err := gemini.LoadAccountFromFile(path, s.Pool, s.Rotator)
	if err != nil {
		return nil, err
	}
	ac.ID = id
	rec, _, _ := cookies.LoadAccountFile(path)
	var capAt, rotAt time.Time
	var importedFrom, profileID, acctGroup, qReason string
	var quarantined, disabled bool
	surfaces := make(map[string]string)
	if rec != nil {
		if rec.CapturedAt != "" {
			capAt, _ = time.Parse(time.RFC3339, rec.CapturedAt)
		}
		if rec.RotatedAt != "" {
			rotAt, _ = time.Parse(time.RFC3339, rec.RotatedAt)
		}
		importedFrom = rec.ImportedFrom
		profileID = rec.ProfileID
		acctGroup = rec.AccountGroup
		quarantined = rec.Quarantined
		qReason = rec.QuarantineReason
		disabled = rec.Disabled
		for k, v := range rec.Surfaces {
			surfaces[k] = v
		}
	}
	cfg := s.cfgSnapshot()
	if g, ok := cfg.AccountGroups[id]; ok && g != "" {
		acctGroup = g
	}
	proxyURL := ""
	if p, ok := cfg.EgressPerAccount[id]; ok && p != "" {
		proxyURL = p
	} else if rec != nil && rec.ProxyURL != "" {
		proxyURL = rec.ProxyURL
	}
	ac.SetRuntimeOptions(cfg.Locale, cfg.BLOverride, cfg.ProtocolMode, acctGroup, proxyURL)

	return &AccountState{
		ID:               id,
		Email:            ac.Email(),
		FilePath:         path,
		CapturedAt:       capAt,
		RotatedAt:        rotAt,
		ImportedFrom:     importedFrom,
		ProfileID:        profileID,
		AccountGroup:     acctGroup,
		ProxyURL:         proxyURL,
		Quarantined:      quarantined,
		QuarantineReason: qReason,
		Disabled:         disabled,
		Surfaces:         surfaces,
		subStrikes:       make(map[string]int),
		Client:           ac,
	}, nil
}

func effectiveFreshnessTime(a *AccountState) time.Time {
	if !a.RotatedAt.IsZero() {
		return a.RotatedAt
	}
	return a.CapturedAt
}

func dedupeAccounts(list []*AccountState, credFile string) []*AccountState {
	byEmail := make(map[string]*AccountState)
	var out []*AccountState
	for _, a := range list {
		key := strings.ToLower(strings.TrimSpace(a.Email))
		if key == "" {
			out = append(out, a)
			continue
		}
		prev, exists := byEmail[key]
		if !exists {
			byEmail[key] = a
			out = append(out, a)
			continue
		}
		aNamed := a.FilePath != credFile
		prevNamed := prev.FilePath != credFile
		aTime := effectiveFreshnessTime(a)
		prevTime := effectiveFreshnessTime(prev)
		newer := aTime.After(prevTime)
		tieNamed := aTime.Equal(prevTime) && aNamed && !prevNamed
		if newer || tieNamed {
			byEmail[key] = a
			for i := range out {
				if out[i] == prev {
					out[i] = a
					break
				}
			}
		}
	}
	return out
}

func (s *Server) clientFor(a *AccountState) UpstreamClient {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.clientFactory != nil {
		c := s.clientFactory(a)
		if c != nil {
			a.Client = c
			return c
		}
	}
	if a.Client != nil {
		a.Client.ReloadFromDiskIfNewer()
		return a.Client
	}
	if a.FilePath != "" {
		if ac, err := gemini.LoadAccountFromFile(a.FilePath, s.Pool, s.Rotator); err == nil {
			ac.ID = a.ID
			a.Client = ac
			return ac
		}
	}
	ac := gemini.NewAccountClient(a.ID, "", a.FilePath, s.Pool, s.Rotator)
	a.Client = ac
	return ac
}

// shouldClearTransientFailure reports whether a recorded failure is the kind a
// healthy registry read proves gone. A stall, a timeout or a transport hiccup is
// evidence about one request; a rejected session or a restricted account is a
// verdict that a successful model listing does not overturn.
func shouldClearTransientFailure(lastError string) bool {
	return !looksLikeAuthFailure(lastError) && !looksLikeAccountBan(lastError)
}

// RefreshAccount refreshes the live model list and quota for a single account,
// preserving the last known good state on failure.
func (s *Server) RefreshAccount(ctx context.Context, a *AccountState, force bool) *AccountState {
	a.mu.RLock()
	hasModels := len(a.Models) > 0
	fresh := !a.ModelsAt.IsZero() && time.Since(a.ModelsAt) < 10*time.Minute
	a.mu.RUnlock()
	if !force && hasModels && fresh {
		return a
	}

	c := s.clientFor(a)
	am, err := c.AvailableModels(ctx, force)
	if err != nil || am == nil || am.Status != 200 || len(am.Models) == 0 {
		if err == nil {
			err = transport.NewUpstreamError("invalid upstream model registry", 502, "model_discovery_error", nil, nil)
		}
		a.mu.Lock()
		a.ModelsError = err
		a.mu.Unlock()
		return a
	}

	quota := c.GetQuota(ctx)

	a.mu.Lock()
	a.Models = am.Models
	a.Capacity = am.Capacity
	a.ModelsAt = time.Now()
	a.ModelsError = nil
	// A successful registry read is proof the account is reachable and
	// authenticated right now. Without this, a transient failure leaves the
	// account labelled degraded with nothing that ever clears it: only a served
	// request resets the failure counters, and the scheduler - the one component
	// that touches every account on a timer - never serves a request. The label
	// then outlives the condition it described until an operator presses Refresh.
	// Only transient evidence is cleared. A rejected session or a restricted
	// account is not disproved by a registry read, so those keep their own
	// record and their own remedy; clearing them here would hide the very
	// condition the operator needs to see.
	if shouldClearTransientFailure(a.LastError) {
		a.Fails = 0
		a.LastFail = time.Time{}
		a.LastError = ""
	}
	a.mu.Unlock()
	if len(quota) > 0 {
		s.mergeAccountQuota(a, quota)
	}
	return a
}

// EnsureModels refreshes all accounts concurrently.
func (s *Server) EnsureModels(ctx context.Context, force bool) []*AccountState {
	accts := s.Accounts()
	var wg sync.WaitGroup
	for _, a := range accts {
		wg.Add(1)
		go func(acct *AccountState) {
			defer wg.Done()
			s.RefreshAccount(ctx, acct, force)
		}(a)
	}
	wg.Wait()
	return accts
}

// modelNames returns the model ids in registry order, for callers that need the
// same list the API serves rather than a written-out copy that can go stale.
func modelNames(models []gemini.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		if name := strings.TrimSpace(m.ModelName); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func (s *Server) allModels() []gemini.ModelInfo {
	accts := s.Accounts()
	seen := make(map[string]bool)
	var out []gemini.ModelInfo
	for _, a := range accts {
		a.mu.RLock()
		for _, m := range a.Models {
			if !seen[m.ModelName] {
				seen[m.ModelName] = true
				out = append(out, m)
			}
		}
		a.mu.RUnlock()
	}
	return out
}

func resolveModelOnAccount(a *AccountState, wanted string) *gemini.ModelInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	w := strings.TrimSpace(wanted)
	if w == "" {
		if len(a.Models) > 0 {
			m := a.Models[0]
			return &m
		}
		return nil
	}
	for _, m := range a.Models {
		if gemini.MatchesModel(m, w) {
			found := m
			return &found
		}
	}
	return nil
}

func (s *Server) markAccountQuarantined(a *AccountState, reason string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.Quarantined = true
	a.QuarantineReason = reason
	if a.Surfaces == nil {
		a.Surfaces = make(map[string]string)
	}
	a.Surfaces["geminiWeb"] = "blocked"
	c := a.Client
	a.mu.Unlock()

	if ac, ok := c.(*gemini.AccountClient); ok {
		ac.SetQuarantined(true, reason)
	}
	log.Printf("[quarantine] account=%s quarantined reason=%q", a.ID, reason)
}

func (s *Server) checkAccountRateLimit(a *AccountState) error {
	if a == nil {
		return nil
	}
	cfg := s.cfgSnapshot()
	now := s.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := accountBlockedErrorLocked(a); err != nil {
		return err
	}
	if cfg.MaxConcurrentPerAccount > 0 && a.activeStreams >= cfg.MaxConcurrentPerAccount {
		return transport.NewUpstreamError("account concurrent stream limit reached", 429, "rate_limit_error", map[string]string{"retry-after": "2"}, nil)
	}
	cutoff := now.Add(-60 * time.Second)
	kept := a.reqTimestamps[:0]
	for _, ts := range a.reqTimestamps {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	a.reqTimestamps = kept
	if cfg.MaxRPMPerAccount > 0 && len(a.reqTimestamps) >= cfg.MaxRPMPerAccount {
		waitSec := int(math.Ceil(a.reqTimestamps[0].Add(60 * time.Second).Sub(now).Seconds()))
		if waitSec < 1 {
			waitSec = 1
		}
		return transport.NewUpstreamError(
			fmt.Sprintf("account %s RPM limit (%d/min) reached", a.ID, cfg.MaxRPMPerAccount),
			429,
			"rate_limit_error",
			map[string]string{"retry-after": strconv.Itoa(waitSec)},
			nil,
		)
	}
	a.reqTimestamps = append(a.reqTimestamps, now)
	if len(a.reqTimestamps) > a.peakRPM {
		a.peakRPM = len(a.reqTimestamps)
	}
	a.totalRequests++
	return nil
}

func accountBlockedError(a *AccountState) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return accountBlockedErrorLocked(a)
}

func accountBlockedErrorLocked(a *AccountState) error {
	if a.Quarantined {
		return transport.NewUpstreamError(fmt.Sprintf("account %s is quarantined: %s", a.ID, a.QuarantineReason), 403, "account_banned", nil, nil)
	}
	if a.Disabled || a.retired {
		return transport.NewUpstreamError(fmt.Sprintf("account %s is disabled", a.ID), 403, "account_disabled", nil, nil)
	}
	return nil
}

func (s *Server) checkPreFlightQuota(a *AccountState, model gemini.ModelInfo, mid string) error {
	return s.checkPreFlightQuotaInternal(a, model, mid, false)
}

func (s *Server) checkPreFlightQuotaInternal(a *AccountState, model gemini.ModelInfo, mid string, forSelection bool) error {
	if a == nil {
		return nil
	}
	cfg := s.cfgSnapshot()
	if err := accountBlockedError(a); err != nil {
		return err
	}

	mode := strings.ToLower(strings.TrimSpace(cfg.QuotaPreflight))
	if mode == "off" {
		return nil
	}
	if !cfg.RejectDowngrade && (mode == "" || mode == "strict") {
		return nil
	}
	name := strings.ToLower(model.ModelName)
	if strings.Contains(name, "lite") {
		return nil
	}
	now := s.now()
	a.mu.RLock()
	activeSpent := spentQuotaForModel(a.Quota, name, now, false)
	a.mu.RUnlock()
	if len(activeSpent) == 0 {
		return nil
	}
	if mode == "advisory" && !forSelection {
		log.Printf("[quota] advisory preflight warning: model %s appears exhausted on account %s", mid, a.ID)
		return nil
	}
	var earliestReal time.Time
	var earliestAny time.Time
	hasReal := false
	for _, q := range activeSpent {
		isSynth := q.Synthetic || strings.HasPrefix(q.ID, "sub:")
		if t, err := time.Parse(time.RFC3339, q.ResetAt); err == nil {
			if earliestAny.IsZero() || t.Before(earliestAny) {
				earliestAny = t
			}
			if !isSynth {
				hasReal = true
				if earliestReal.IsZero() || t.Before(earliestReal) {
					earliestReal = t
				}
			}
		}
	}
	if !hasReal {
		return transport.NewUpstreamError(
			fmt.Sprintf("quota exceeded: %s is in synthetic substitution cooldown on account %s until %s", mid, a.ID, earliestAny.UTC().Format(time.RFC3339)),
			429,
			"rate_limit_error",
			map[string]string{
				"x-gsb-retry-reason": "model_substitution",
			},
			nil,
		)
	}
	retrySec := int(math.Ceil(earliestReal.Sub(now).Seconds()))
	if retrySec < 1 {
		retrySec = 1
	}
	return transport.NewUpstreamError(
		fmt.Sprintf("quota exceeded: %s is exhausted on account %s until %s", mid, a.ID, earliestReal.UTC().Format(time.RFC3339)),
		429,
		"rate_limit_error",
		map[string]string{
			"retry-after": strconv.Itoa(retrySec),
			"x-resets-at": earliestReal.UTC().Format(time.RFC3339),
		},
		nil,
	)
}

// poolAvailabilityError replaces an account-specific quota error with an
// aggregate message when a pool request has no eligible account. Account keys
// keep the precise account error so pinning semantics remain observable.
func (s *Server) poolAvailabilityError(accts []*AccountState, requested string, fallback error) error {
	upstreamErr, ok := fallback.(*transport.UpstreamError)
	if !ok || upstreamErr == nil {
		return fallback
	}

	exhausted, substitution, cooling, rateLimited, modelUnavailable, quarantined, disabled, ready := 0, 0, 0, 0, 0, 0, 0, 0
	for _, a := range accts {
		if a == nil {
			continue
		}
		a.mu.RLock()
		isQuarantined, isDisabled := a.Quarantined, a.Disabled
		a.mu.RUnlock()
		if isQuarantined {
			quarantined++
			continue
		}
		if isDisabled {
			disabled++
			continue
		}
		model := resolveModelOnAccount(a, requested)
		if model == nil {
			modelUnavailable++
			continue
		}
		if quotaErr := s.checkPreFlightQuotaInternal(a, *model, model.ModelName, true); quotaErr != nil {
			if quotaUE, ok := quotaErr.(*transport.UpstreamError); ok && quotaUE.Headers["x-gsb-retry-reason"] == "model_substitution" {
				substitution++
			} else {
				exhausted++
			}
			continue
		}
		if s.accountCooling(a) {
			cooling++
			continue
		}
		if s.accountRateLimited(a) {
			rateLimited++
			continue
		}
		ready++
	}
	if ready > 0 {
		return fallback
	}

	details := make([]string, 0, 7)
	if exhausted > 0 {
		details = append(details, fmt.Sprintf("exhausted=%d", exhausted))
	}
	if substitution > 0 {
		details = append(details, fmt.Sprintf("substitution_cooldown=%d", substitution))
	}
	if cooling > 0 {
		details = append(details, fmt.Sprintf("cooling=%d", cooling))
	}
	if rateLimited > 0 {
		details = append(details, fmt.Sprintf("rate_limited=%d", rateLimited))
	}
	if modelUnavailable > 0 {
		details = append(details, fmt.Sprintf("model_unavailable=%d", modelUnavailable))
	}
	if quarantined > 0 {
		details = append(details, fmt.Sprintf("quarantined=%d", quarantined))
	}
	if disabled > 0 {
		details = append(details, fmt.Sprintf("disabled=%d", disabled))
	}
	if len(details) == 0 {
		return fallback
	}
	if requested == "" {
		requested = "requested model"
	}
	message := fmt.Sprintf("no pool account can serve %s: %d account(s) unavailable (%s); no account failover was available", requested, len(accts), strings.Join(details, ", "))
	return transport.NewUpstreamError(message, upstreamErr.StatusCode, upstreamErr.Code, upstreamErr.Headers, upstreamErr.Cause)
}

func (s *Server) classifyEmpty(ctx context.Context, a *AccountState, modelName string) error {
	c := s.clientFor(a)
	qCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	quota := c.GetQuota(qCtx)
	cancel()
	if len(quota) > 0 {
		quota = s.mergeAccountQuota(a, quota)
	}
	a.mu.RLock()
	if len(quota) == 0 {
		quota = cloneQuotaBuckets(a.Quota)
	}
	email := a.Email
	a.mu.RUnlock()
	now := s.now()
	chosen := spentQuotaForModel(quota, modelName, now, true)
	if len(chosen) > 0 {
		labels := make([]string, 0, len(chosen))
		for _, q := range chosen {
			labels = append(labels, q.Label)
		}
		hdrs := quotaResetHeaders(chosen, now)
		ident := email
		if ident == "" {
			ident = a.ID
		}
		return transport.NewUpstreamError(
			fmt.Sprintf("quota exceeded: %s exhausted on %s", strings.Join(labels, ", "), ident),
			429,
			"rate_limit_error",
			hdrs,
			nil,
		)
	}
	return transport.NewUpstreamError("empty upstream response", 502, "empty_upstream_response", nil, nil)
}

// spentQuotaForModel includes account-wide windows alongside model-family
// buckets. Unknown resets may explain an empty response, but only a future
// reset should block preflight or tell clients when to retry.
func spentQuotaForModel(quota []gemini.QuotaBucket, modelName string, now time.Time, allowUnknownReset bool) []gemini.QuotaBucket {
	family := modelFamilyTokens(modelName)
	lite := strings.Contains(strings.ToLower(modelName), "lite")
	var spent []gemini.QuotaBucket
	for _, q := range quota {
		if q.Unlimited || q.Total <= 0 || q.Remaining > 0 {
			continue
		}
		if reset, err := time.Parse(time.RFC3339, q.ResetAt); err == nil {
			if !reset.After(now) {
				continue
			}
		} else if !allowUnknownReset || q.Synthetic || strings.HasPrefix(q.ID, "sub:") {
			continue
		}
		if q.ID == "5h" || q.ID == "weekly" {
			if !lite {
				spent = append(spent, q)
			}
			continue
		}
		if len(family) == 0 {
			spent = append(spent, q)
			continue
		}
		label := strings.ToLower(q.Label)
		for _, token := range family {
			if strings.Contains(label, token) {
				spent = append(spent, q)
				break
			}
		}
	}
	return spent
}

func modelFamilyTokens(modelName string) []string {
	name := strings.ToLower(modelName)
	var family []string
	// The frontier tier is billed against the "Gemini Pro" quota bucket. Upstream
	// renamed it (Argon, now Carbon) while keeping that bucket name, so both names
	// must map to the same family or an exhausted bucket is reported against the
	// wrong model.
	if strings.Contains(name, "argon") || strings.Contains(name, "carbon") {
		family = append(family, "argon", "carbon", "pro")
	}
	// The everyday tier was renamed from Flash to Hydrogen on 2026-11-13. It
	// still spends the "Gemini Flash" bucket, so the new name keeps that token.
	if strings.Contains(name, "hydrogen") {
		family = append(family, "hydrogen", "flash")
	}
	for _, tok := range []string{"pro", "flash", "thinking"} {
		if strings.Contains(name, tok) {
			family = append(family, tok)
		}
	}
	return family
}

// quotaExhaustionBackoff escalates a model's cooldown each time it is observed
// exhausted again after a retry. The old ceiling of 15 minutes assumed every
// limit resets within the hour, which is only true for the rolling 5-hour window.
// Gemini also enforces a weekly limit that lasts for days, so an account whose
// weekly quota is spent would be retried every 15 minutes indefinitely, burning
// real requests against an account that cannot serve them. The later steps cover
// that case while still letting a genuinely short limit recover quickly.
var quotaExhaustionBackoff = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

// quotaExhaustionCooldown returns how long to avoid a model after `strikes`
// consecutive exhaustion observations.
func quotaExhaustionCooldown(strikes int) time.Duration {
	if strikes <= 0 {
		return quotaExhaustionBackoff[0]
	}
	idx := strikes - 1
	if idx >= len(quotaExhaustionBackoff) {
		idx = len(quotaExhaustionBackoff) - 1
	}
	return quotaExhaustionBackoff[idx]
}

func (s *Server) recordModelExhausted(a *AccountState, modelName string) {
	if a == nil || modelName == "" {
		return
	}
	subID := "sub:" + strings.ToLower(strings.TrimSpace(modelName))
	now := s.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	pruneExpiredSyntheticLocked(a, now)
	if a.subStrikes == nil {
		a.subStrikes = make(map[string]int)
	}
	strikes := a.subStrikes[subID] + 1
	a.subStrikes[subID] = strikes
	dur := quotaExhaustionCooldown(strikes)
	resetAt := now.UTC().Add(dur).Format(time.RFC3339)
	bucket := gemini.QuotaBucket{
		ID:        subID,
		Label:     modelName,
		Total:     1,
		Remaining: 0,
		ResetAt:   resetAt,
		Synthetic: true,
		Strikes:   strikes,
	}
	next := make([]gemini.QuotaBucket, 0, len(a.Quota)+1)
	replaced := false
	for _, q := range a.Quota {
		if q.ID == subID {
			next = append(next, bucket)
			replaced = true
		} else {
			next = append(next, q)
		}
	}
	if !replaced {
		next = append(next, bucket)
	}
	a.Quota = next
}

func (s *Server) clearModelExhausted(a *AccountState, modelName string) {
	if a == nil || modelName == "" {
		return
	}
	subID := "sub:" + strings.ToLower(strings.TrimSpace(modelName))
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.subStrikes != nil {
		delete(a.subStrikes, subID)
	}
	if len(a.Quota) == 0 {
		return
	}
	found := false
	for _, q := range a.Quota {
		if q.ID == subID {
			found = true
			break
		}
	}
	if !found {
		return
	}
	kept := make([]gemini.QuotaBucket, 0, len(a.Quota)-1)
	for _, q := range a.Quota {
		if q.ID == subID {
			continue
		}
		kept = append(kept, q)
	}
	a.Quota = kept
}

// pruneExpiredSyntheticLocked removes synthetic substitution buckets whose
// cooldown has elapsed, together with their strike counters. Real quota buckets
// are left untouched. Callers must hold a.mu.
func pruneExpiredSyntheticLocked(a *AccountState, now time.Time) {
	if a == nil {
		return
	}
	if len(a.Quota) > 0 {
		kept := make([]gemini.QuotaBucket, 0, len(a.Quota))
		for _, q := range a.Quota {
			if (q.Synthetic || strings.HasPrefix(q.ID, "sub:")) && !syntheticCooldownActive(q, now) {
				continue
			}
			kept = append(kept, q)
		}
		a.Quota = kept
	}
	if len(a.subStrikes) == 0 {
		return
	}
	active := make(map[string]bool, len(a.Quota))
	for _, q := range a.Quota {
		if q.Synthetic || strings.HasPrefix(q.ID, "sub:") {
			if syntheticCooldownActive(q, now) {
				active[q.ID] = true
			}
		}
	}
	for subID := range a.subStrikes {
		if !active[subID] {
			delete(a.subStrikes, subID)
		}
	}
}

// clearSyntheticQuotaLocked drops all synthetic substitution state while keeping
// real upstream quota intact. It is used when a credential is genuinely
// recaptured or unquarantined. Callers must hold a.mu.
func clearSyntheticQuotaLocked(a *AccountState) {
	if a == nil {
		return
	}
	if len(a.subStrikes) > 0 {
		a.subStrikes = make(map[string]int)
	}
	if len(a.Quota) == 0 {
		return
	}
	kept := make([]gemini.QuotaBucket, 0, len(a.Quota))
	for _, q := range a.Quota {
		if q.Synthetic || strings.HasPrefix(q.ID, "sub:") {
			continue
		}
		kept = append(kept, q)
	}
	a.Quota = kept
}

func (s *Server) mergeAccountQuota(a *AccountState, fresh []gemini.QuotaBucket) []gemini.QuotaBucket {
	now := s.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	merged := make([]gemini.QuotaBucket, 0, len(fresh)+4)
	merged = append(merged, fresh...)
	hasWeekly := false
	for _, q := range fresh {
		if !q.Synthetic && (q.ID == "weekly" || q.Total > 5000) {
			hasWeekly = true
			break
		}
	}
	for _, old := range a.Quota {
		if (old.Synthetic || strings.HasPrefix(old.ID, "sub:")) && old.ResetAt != "" {
			if t, err := time.Parse(time.RFC3339, old.ResetAt); err == nil && t.After(now) {
				old.Synthetic = true
				merged = append(merged, old)
			}
			continue
		}
		if !hasWeekly && (old.ID == "weekly" || old.Total > 5000) && !old.Synthetic {
			old.ID = "weekly"
			old.Label = "Weekly Pool"
			merged = append(merged, old)
			hasWeekly = true
		}
	}
	a.Quota = merged
	pruneExpiredSyntheticLocked(a, now)
	a.QuotaAt = now
	a.QuotaBackoffStep = 0
	a.QuotaNextAllowAt = time.Time{}
	return cloneQuotaBuckets(a.Quota)
}

func (s *Server) isAccountBusy(acctID string) bool {
	if s.Pool == nil || acctID == "" {
		return false
	}
	if sess := s.Pool.Existing(acctID); sess != nil {
		return sess.Busy()
	}
	return false
}

func (s *Server) hasSessionCacheHit(a *AccountState, modelName string, hasher *prompt.HistoryHasher) bool {
	if !s.cfgSnapshot().SessionCache || a == nil || modelName == "" || hasher == nil || hasher.Len() < 3 {
		return false
	}
	for split := hasher.Len() - 1; split >= 2; split-- {
		if len(s.peekSessionCache(hasher.HashPrefix(a.ID, modelName, split))) > 0 {
			return true
		}
	}
	return false
}

// consumeSessionCacheForGeneration scans without telemetry, then consumes only
// the newest matching prefix. This prevents one request from counting a miss for
// every older prefix it probes and promotes only the continuation actually used.
func (s *Server) consumeSessionCacheForGeneration(accountID, modelName string, hasher *prompt.HistoryHasher) (string, []any, int) {
	if !s.cfgSnapshot().SessionCache || accountID == "" || modelName == "" || hasher == nil || hasher.Len() < 3 {
		return "", nil, 0
	}
	for split := hasher.Len() - 1; split >= 2; split-- {
		key := hasher.HashPrefix(accountID, modelName, split)
		if len(s.peekSessionCache(key)) == 0 {
			continue
		}
		meta, ok := s.takeSessionCache(key)
		if !ok {
			return "", nil, 0
		}
		return key, meta, split
	}
	s.cacheMisses.Add(1)
	return "", nil, 0
}

// pickFallbackAccount selects an eligible replacement and records one request
// on it. A candidate that loses the admission race is skipped, not retried.
func (s *Server) pickFallbackAccount(accts []*AccountState, tried map[string]bool, originGroup string, requested string, hasher *prompt.HistoryHasher) (*AccountState, *gemini.ModelInfo) {
	if s.rotate() {
		return s.pickRotateFallback(accts, tried, originGroup, requested)
	}
	var rejected map[string]bool
	skip := func(a *AccountState) bool {
		return rejected[a.ID] || !canTryFallback(a, tried, originGroup)
	}
	for {
		a, m := s.rankedReadyAccount(accts, requested, hasher, skip)
		if a == nil {
			return nil, nil
		}
		if s.checkAccountRateLimit(a) == nil {
			return a, m
		}
		if rejected == nil {
			rejected = make(map[string]bool)
		}
		rejected[a.ID] = true
	}
}

func canTryFallback(a *AccountState, tried map[string]bool, originGroup string) bool {
	if a == nil || tried[a.ID] {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccountGroup == originGroup
}

func (s *Server) rankedReadyAccount(accts []*AccountState, requested string, hasher *prompt.HistoryHasher, skip func(*AccountState) bool) (*AccountState, *gemini.ModelInfo) {
	var best *AccountState
	var bestModel *gemini.ModelInfo
	var bestBusy, bestCache bool
	var bestFails int
	var bestUsed time.Time
	var ranks map[string]int
	if s.fillFirst() {
		ranks = s.accountRanks(accts)
	}
	for _, a := range accts {
		if a == nil || (skip != nil && skip(a)) {
			continue
		}
		m, ok := s.accountReady(a, requested)
		if !ok {
			continue
		}
		busy := s.isAccountBusy(a.ID)
		if best != nil && !bestBusy && busy {
			continue
		}
		cached := s.hasSessionCacheHit(a, m.ModelName, hasher)
		var fails int
		var used time.Time
		if ranks == nil {
			a.mu.RLock()
			fails, used = a.Fails, a.LastUsed
			a.mu.RUnlock()
		}
		if best != nil && busy == bestBusy {
			switch {
			case cached != bestCache:
				if !cached {
					continue
				}
			case ranks != nil:
				if ranks[a.ID] >= ranks[best.ID] {
					continue
				}
			case fails != bestFails:
				if fails > bestFails {
					continue
				}
			default:
				if !used.Before(bestUsed) {
					continue
				}
			}
		}
		best, bestModel = a, m
		bestBusy, bestCache, bestFails, bestUsed = busy, cached, fails, used
	}
	return best, bestModel
}

func (s *Server) lookupQuotaResetHeaders(_ context.Context, a *AccountState, modelName string) map[string]string {
	now := s.now()
	a.mu.RLock()
	spent := spentQuotaForModel(a.Quota, modelName, now, false)
	a.mu.RUnlock()
	return quotaResetHeaders(spent, now)
}

func quotaResetHeaders(chosen []gemini.QuotaBucket, now time.Time) map[string]string {
	var earliestReal time.Time
	sawSynthetic := false
	for _, q := range chosen {
		isSynth := q.Synthetic || strings.HasPrefix(q.ID, "sub:")
		if isSynth {
			sawSynthetic = true
			continue
		}
		if q.ResetAt != "" {
			if t, err := time.Parse(time.RFC3339, q.ResetAt); err == nil && t.After(now) {
				if earliestReal.IsZero() || t.Before(earliestReal) {
					earliestReal = t
				}
			}
		}
	}
	if earliestReal.IsZero() {
		if sawSynthetic {
			return map[string]string{
				"x-gsb-retry-reason": "model_substitution",
			}
		}
		return nil
	}
	retrySec := int(math.Ceil(earliestReal.Sub(now).Seconds()))
	if retrySec < 1 {
		retrySec = 1
	}
	out := map[string]string{
		"retry-after": strconv.Itoa(retrySec),
		"x-resets-at": earliestReal.UTC().Format(time.RFC3339),
	}
	if sawSynthetic {
		out["x-gsb-retry-reason"] = "model_substitution"
	}
	return out
}

func (s *Server) markAccountFailure(a *AccountState, msg string) {
	now := s.now()
	a.mu.Lock()
	a.Fails++
	a.LastFail = now
	a.LastError = msg
	// Pull the next model refresh forward. The refresh is the probe that clears a
	// transient failure, and the account may have been scheduled for the slow
	// cadence before this failure existed: without this, the shortened interval
	// only ever applies to accounts that were already failing when their timer
	// was last set, which is never the account that just broke.
	recheck := failureRecheckInterval + randDuration(failureRecheckInterval/5)
	if due := now.Add(recheck); a.NextModelRefAt.IsZero() || a.NextModelRefAt.After(due) {
		a.NextModelRefAt = due
	}
	a.mu.Unlock()
}

// fillFirstFailCooldown temporarily demotes an account that just failed so a
// broken top-priority account is not hammered, while still letting it win again
// once the cooldown elapses (rank alone would otherwise demote it forever,
// because a demoted account never gets the success that resets Fails).
const fillFirstFailCooldown = 60 * time.Second

func (s *Server) accountCooling(a *AccountState) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	failAt := a.LastFail
	a.mu.RUnlock()
	if failAt.IsZero() {
		return false
	}
	return s.now().Sub(failAt) < fillFirstFailCooldown
}

// accountRanks assigns a selection rank to every loaded account. Accounts named
// in BRIDGE_ACCOUNT_PRIORITY keep the caller's order; the rest follow their load
// order so fill-first stays deterministic without extra configuration.
func (s *Server) accountRanks(accts []*AccountState) map[string]int {
	prio := s.cfgSnapshot().AccountPriority
	ranks := make(map[string]int, len(accts))
	base := len(prio)
	for i, a := range accts {
		if a != nil {
			ranks[a.ID] = base + i
		}
	}
	for i, id := range prio {
		if _, ok := ranks[id]; ok {
			ranks[id] = i
		}
	}
	return ranks
}

func (s *Server) fillFirst() bool {
	return strings.EqualFold(strings.TrimSpace(s.cfgSnapshot().AccountStrategy), "fill-first")
}

func (s *Server) rotate() bool {
	return strings.EqualFold(strings.TrimSpace(s.cfgSnapshot().AccountStrategy), "rotate")
}

// rotateIndex returns the position of the sticky rotation cursor, or 0 when the
// cursor account is no longer loaded.
func (s *Server) rotateIndex(accts []*AccountState) int {
	s.rotateMu.Lock()
	cur := s.rotateCursor
	s.rotateMu.Unlock()
	for i, a := range accts {
		if a != nil && a.ID == cur {
			return i
		}
	}
	return 0
}

func (s *Server) setRotateCursor(id string) {
	s.rotateMu.Lock()
	s.rotateCursor = id
	s.rotateMu.Unlock()
}

// accountRateLimited reports whether the account would reject a new request
// because of the per-account RPM or concurrency limit, without recording a
// request the way checkAccountRateLimit does.
func (s *Server) accountRateLimited(a *AccountState) bool {
	if a == nil {
		return false
	}
	cfg := s.cfgSnapshot()
	if cfg.MaxConcurrentPerAccount <= 0 && cfg.MaxRPMPerAccount <= 0 {
		return false
	}
	cutoff := s.now().Add(-60 * time.Second)
	a.mu.RLock()
	defer a.mu.RUnlock()
	if cfg.MaxConcurrentPerAccount > 0 && a.activeStreams >= cfg.MaxConcurrentPerAccount {
		return true
	}
	if cfg.MaxRPMPerAccount > 0 {
		n := 0
		for _, ts := range a.reqTimestamps {
			if ts.After(cutoff) {
				n++
			}
		}
		if n >= cfg.MaxRPMPerAccount {
			return true
		}
	}
	return false
}

// accountReady is the shared pool eligibility check. Session busy state is a
// scheduling preference, unlike a configured per-account concurrency limit.
func (s *Server) accountReady(a *AccountState, requested string) (*gemini.ModelInfo, bool) {
	if a == nil {
		return nil, false
	}
	a.mu.RLock()
	qState := a.Quarantined || a.Disabled
	a.mu.RUnlock()
	if qState {
		return nil, false
	}
	m := resolveModelOnAccount(a, requested)
	if m == nil {
		return nil, false
	}
	if err := s.checkPreFlightQuotaInternal(a, *m, m.ModelName, true); err != nil {
		return nil, false
	}
	if s.accountCooling(a) {
		return nil, false
	}
	if s.accountRateLimited(a) {
		return nil, false
	}
	return m, true
}

// resolveRotateAccount walks the account list from the sticky cursor and uses
// the first account that can serve the request. The cursor only advances when
// the current account cannot serve, so consecutive conversations stay on one
// Gemini session until that account is exhausted, banned, or failing.
func (s *Server) resolveRotateAccount(accts []*AccountState, requested string, hasher *prompt.HistoryHasher) *AccountState {
	if len(accts) == 0 {
		return nil
	}
	start := s.rotateIndex(accts)
	at := func(i int) *AccountState { return accts[(start+i)%len(accts)] }

	// Prefer the account that already holds this conversation's continuation.
	if s.cfgSnapshot().SessionCache && hasher != nil && hasher.Len() >= 3 {
		for i := 0; i < len(accts); i++ {
			a := at(i)
			m, ok := s.accountReady(a, requested)
			if !ok {
				continue
			}
			if s.hasSessionCacheHit(a, m.ModelName, hasher) {
				s.setRotateCursor(a.ID)
				return a
			}
		}
	}

	// Two-pass selection:
	// Pass 0: Prefer an account that is ready AND not currently busy.
	// Pass 1: If all ready accounts are busy, accept a busy account to queue.
	for pass := 0; pass < 2; pass++ {
		for i := 0; i < len(accts); i++ {
			a := at(i)
			if _, ok := s.accountReady(a, requested); ok {
				if pass == 0 && s.isAccountBusy(a.ID) {
					continue
				}
				s.setRotateCursor(a.ID)
				return a
			}
		}
	}

	return diagnosticChatAccount(accts, requested, start)
}

// pickRotateFallback advances the rotation from the failed account to the next
// one that can serve the request, wrapping around the list.
func (s *Server) pickRotateFallback(accts []*AccountState, tried map[string]bool, originGroup string, requested string) (*AccountState, *gemini.ModelInfo) {
	if len(accts) == 0 {
		return nil, nil
	}
	start := s.rotateIndex(accts)
	at := func(i int) *AccountState { return accts[(start+i)%len(accts)] }
	for pass := 0; pass < 2; pass++ {
		for i := 1; i <= len(accts); i++ {
			cand := at(i)
			if !canTryFallback(cand, tried, originGroup) {
				continue
			}
			m, ok := s.accountReady(cand, requested)
			if !ok {
				continue
			}
			if pass == 0 && s.isAccountBusy(cand.ID) {
				continue
			}
			if s.checkAccountRateLimit(cand) != nil {
				continue
			}
			s.setRotateCursor(cand.ID)
			return cand, m
		}
	}
	return nil, nil
}

func (s *Server) resolveChatAccount(accts []*AccountState, scope AuthScope, requested string, hasher *prompt.HistoryHasher) *AccountState {
	if scope.AccountID != "" {
		for _, a := range accts {
			if a != nil && a.ID == scope.AccountID {
				return a
			}
		}
		return nil
	}
	if scope.Group != "" {
		// A group-scoped key may only use the accounts of its own group, and it
		// keeps normal failover inside that group instead of the whole pool.
		grouped := make([]*AccountState, 0, len(accts))
		for _, a := range accts {
			if a == nil {
				continue
			}
			a.mu.RLock()
			group := a.AccountGroup
			a.mu.RUnlock()
			if group == scope.Group {
				grouped = append(grouped, a)
			}
		}
		if len(grouped) == 0 {
			return nil
		}
		accts = grouped
	}
	if !scope.Admin && scope.Group == "" {
		explicitTokens := len(s.cfgSnapshot().AccountTokens) > 0
		if explicitTokens {
			return nil
		}
	}
	if s.rotate() {
		return s.resolveRotateAccount(accts, requested, hasher)
	}
	if a, _ := s.rankedReadyAccount(accts, requested, hasher, nil); a != nil {
		return a
	}
	return diagnosticChatAccount(accts, requested, 0)
}

// With no ready account, prefer a model-capable account so preflight can
// return its specific 429 instead of reporting an unrelated missing model.
// An unprobed account may still discover the model; discard blocked accounts.
func diagnosticChatAccount(accts []*AccountState, requested string, start int) *AccountState {
	var unprobed, missing *AccountState
	for i := range len(accts) {
		a := accts[(start+i)%len(accts)]
		if a == nil {
			continue
		}
		a.mu.RLock()
		blocked, hasModels := a.Quarantined || a.Disabled, len(a.Models) > 0
		a.mu.RUnlock()
		if blocked {
			continue
		}
		if resolveModelOnAccount(a, requested) != nil {
			return a
		}
		if !hasModels && unprobed == nil {
			unprobed = a
		}
		if missing == nil {
			missing = a
		}
	}
	if unprobed != nil {
		return unprobed
	}
	return missing
}

const quotaRefreshCooldown = 45 * time.Second

func (s *Server) refreshQuotaAsync(a *AccountState, c UpstreamClient) {
	if a == nil || c == nil {
		return
	}
	now := s.now()
	a.mu.Lock()
	if a.quotaRefreshing || a.Quarantined {
		a.mu.Unlock()
		return
	}
	if !a.QuotaNextAllowAt.IsZero() && now.Before(a.QuotaNextAllowAt) {
		a.mu.Unlock()
		return
	}
	lowQuota := false
	for _, q := range a.Quota {
		if !q.Synthetic && !strings.HasPrefix(q.ID, "sub:") && !q.Unlimited && q.Total > 0 {
			if q.Remaining <= 200 || (q.Total >= 1000 && q.Remaining*20 <= q.Total) {
				lowQuota = true
				break
			}
		}
	}
	if !lowQuota && !a.QuotaAt.IsZero() && now.Sub(a.QuotaAt) < quotaRefreshCooldown {
		a.mu.Unlock()
		return
	}
	a.quotaRefreshing = true
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			a.quotaRefreshing = false
			a.mu.Unlock()
		}()
		if s.Pool != nil {
			if sess := s.Pool.Existing(a.ID); sess != nil && sess.Busy() {
				return
			}
		}
		qCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		q := c.GetQuota(qCtx)
		if len(q) > 0 {
			s.mergeAccountQuota(a, q)
		} else {
			a.mu.Lock()
			a.QuotaBackoffStep++
			shift := a.QuotaBackoffStep
			if shift > 5 {
				shift = 5
			}
			backoff := quotaRefreshCooldown * time.Duration(1<<shift)
			if backoff > 15*time.Minute {
				backoff = 15 * time.Minute
			}
			a.QuotaNextAllowAt = s.now().Add(backoff)
			a.mu.Unlock()
		}
	}()
}

type preparedGeneration struct {
	accts               []*AccountState
	account             *AccountState
	model               *gemini.ModelInfo
	mid                 string
	requested           string
	effortStr           string
	contract            *toolbridge.Contract
	hasTools            bool
	stream              bool
	buffered            bool
	toolsRaw            any
	opts                gemini.StreamOptions
	fullPrompt          string
	curPrompt           string
	flattenStats        prompt.FlattenStats
	overflowParts       []prompt.AttachmentPart
	imageParts          []prompt.AttachmentPart
	fullAttachmentParts []prompt.AttachmentPart
	attachmentParts     []prompt.AttachmentPart
	hasher              *prompt.HistoryHasher
	// cacheKey is the exact continuation entry consumed by this generation. It is
	// empty when no cached metadata was selected; callers invalidate this key when
	// the upstream rejects the continuation before falling back to a full prompt.
	cacheKey string
	// cacheSplit is the message index at which the cached continuation was
	// selected. It is zero when no continuation was used.
	cacheSplit int
	// limits are the OpenAI controls Gemini Web cannot express: output budget,
	// stop sequences, and structured-output intent. They are enforced on the
	// assembled answer in the request handler.
	limits answerLimits
}

func (s *Server) prepareGeneration(
	ctx context.Context,
	body map[string]any,
	scope AuthScope,
	messages []map[string]any,
	toolsRaw any,
	toolChoiceRaw any,
) (*preparedGeneration, error) {
	rawModel, _ := body["model"].(string)
	requested, inlineThink := gemini.ParseModelRequest(rawModel)

	// The scope decides which accounts this request may touch: an account key
	// sees only its own account, a group key only its group, the pool key all
	// of them. Scoping the list here keeps failover inside the same boundary.
	accts := s.scopedAccounts(scope)
	if len(accts) == 0 {
		return nil, transport.NewUpstreamError("no accounts configured", 503, "auth_unavailable", nil, nil)
	}

	if toolbridge.ShouldAugmentMediaTool(messages) {
		toolsRaw = toolbridge.AugmentToolsWithMediaGenerator(toolsRaw, toolChoiceRaw)
	}

	cfg := s.cfgSnapshot()
	var hasher *prompt.HistoryHasher
	if cfg.SessionCache && len(messages) >= 3 {
		hasher = prompt.NewHistoryHasher(messages, toolsRaw, toolChoiceRaw)
	}

	account := s.resolveChatAccount(accts, scope, requested, hasher)
	if account == nil {
		disabled := false
		for _, a := range accts {
			if a == nil {
				continue
			}
			a.mu.RLock()
			q := a.Quarantined
			r := a.QuarantineReason
			disabled = disabled || a.Disabled
			a.mu.RUnlock()
			if q {
				return nil, transport.NewUpstreamError(fmt.Sprintf("all accounts are quarantined (last: %s)", r), 403, "account_banned", nil, nil)
			}
		}
		if disabled {
			return nil, transport.NewUpstreamError("no usable account: accounts are disabled", 403, "account_disabled", nil, nil)
		}
		msg := "this endpoint requires a pool key (BRIDGE_API_KEY), a group key, or an account key; no usable account is loaded for this request"
		if scope.AccountID != "" {
			msg = fmt.Sprintf("account key is bound to account %q, which is not loaded", scope.AccountID)
		} else if scope.Group != "" {
			msg = fmt.Sprintf("group key is bound to group %q, which has no loaded account", scope.Group)
		}
		return nil, transport.NewUpstreamError(msg, 401, "authentication_error", nil, nil)
	}

	if err := accountBlockedError(account); err != nil {
		return nil, err
	}

	contract, err := toolbridge.ValidateToolRequest(toolsRaw, toolChoiceRaw)
	if err != nil {
		return nil, err
	}

	account.mu.RLock()
	hasModels := len(account.Models) > 0
	account.mu.RUnlock()
	if !hasModels {
		s.RefreshAccount(ctx, account, false)
	}

	account.mu.RLock()
	hasModels = len(account.Models) > 0
	mErr := account.ModelsError
	account.mu.RUnlock()
	if !hasModels && mErr != nil {
		return nil, mErr
	}

	model := resolveModelOnAccount(account, requested)
	if model == nil {
		reqLabel := requested
		if reqLabel == "" {
			reqLabel = "(unspecified)"
		}
		return nil, transport.NewUpstreamError(fmt.Sprintf("account %s cannot serve model %s", account.ID, reqLabel), 404, "model_not_found", nil, nil)
	}
	mid := model.ModelName

	if err := s.checkPreFlightQuota(account, *model, mid); err != nil {
		if scope.AccountID == "" {
			return nil, s.poolAvailabilityError(accts, requested, err)
		}
		return nil, err
	}
	if err := s.checkAccountRateLimit(account); err != nil {
		if scope.AccountID == "" {
			return nil, s.poolAvailabilityError(accts, requested, err)
		}
		return nil, err
	}

	fullPrompt, overflowParts, flattenStats, err := prompt.FlattenMessagesWithStats(messages, toolsRaw, toolChoiceRaw, prompt.FlattenOptions{
		StartIndex:      0,
		MaxPromptChars:  cfg.MaxPromptChars,
		MaxOldToolChars: cfg.MaxOldToolChars,
		AllowOverflow:   cfg.HistoryOffload,
	})
	if err != nil {
		return nil, err
	}

	imageParts := prompt.ExtractImageParts(messages)
	fullAttachmentParts := make([]prompt.AttachmentPart, 0, len(overflowParts)+len(imageParts))
	fullAttachmentParts = append(fullAttachmentParts, overflowParts...)
	fullAttachmentParts = append(fullAttachmentParts, imageParts...)
	attachmentParts := fullAttachmentParts

	hasTools := len(contract.Tools) > 0 && contract.ChoiceMode != "none"
	stream, _ := body["stream"].(bool)
	buffered := cfg.RejectDowngrade || hasTools
	if strings.EqualFold(cfg.StreamBufferMode, "eager") {
		buffered = hasTools
	}

	tempReq, _ := body["temporary"].(bool)
	effortVal := extractEffortFromBody(body)
	if (effortVal == nil || effortVal == "") && inlineThink != "" {
		effortVal = inlineThink
	}
	if !hasTools && (effortVal == nil || effortVal == "") && isLightweightTitleRequest(messages) {
		effortVal = "low"
		tempReq = true
	}
	thinkLvl := gemini.ThinkingLevelForModel(effortVal, *model, strconv.Itoa(cfg.DefaultThinking))
	effortStr := "low"
	switch thinkLvl {
	case 4:
		effortStr = "medium"
	case 2:
		effortStr = "high"
	}
	opts := gemini.StreamOptions{
		Model:           model,
		ThinkingLevel:   thinkLvl,
		Temporary:       tempReq || cfg.Temporary,
		Language:        cfg.Locale,
		DisableTools:    false,
		HasTools:        hasTools,
		ProtocolMode:    cfg.ProtocolMode,
		PersonalContext: cfg.PersonalContext,
		NoFrameTimeout:  cfg.NoFrameTimeout,
	}

	curPrompt := fullPrompt
	cacheKey := ""
	cacheSplit := 0
	if cfg.SessionCache && !opts.Temporary && hasher != nil {
		selectedKey, cachedMeta, split := s.consumeSessionCacheForGeneration(account.ID, mid, hasher)
		if len(cachedMeta) > 0 {
			opts.Metadata = cachedMeta
			cacheKey = selectedKey
			cacheSplit = split
			deltaPrompt, _, dErr := prompt.FlattenMessages(messages, toolsRaw, toolChoiceRaw, prompt.FlattenOptions{
				StartIndex:      split,
				MaxPromptChars:  cfg.MaxPromptChars,
				MaxOldToolChars: cfg.MaxOldToolChars,
				AllowOverflow:   false,
			})
			if dErr == nil {
				curPrompt = deltaPrompt
				attachmentParts = prompt.ExtractImageParts(messages[split:])
			}
		}
	}
	if len(overflowParts) > 0 && len(opts.Metadata) == 0 {
		account.mu.Lock()
		account.offloadRequests++
		account.mu.Unlock()
	}

	return &preparedGeneration{
		accts:               accts,
		account:             account,
		model:               model,
		mid:                 mid,
		requested:           requested,
		effortStr:           effortStr,
		contract:            contract,
		toolsRaw:            toolsRaw,
		hasTools:            hasTools,
		stream:              stream,
		buffered:            buffered,
		opts:                opts,
		fullPrompt:          fullPrompt,
		curPrompt:           curPrompt,
		flattenStats:        flattenStats,
		overflowParts:       overflowParts,
		imageParts:          imageParts,
		fullAttachmentParts: fullAttachmentParts,
		attachmentParts:     attachmentParts,
		hasher:              hasher,
		cacheKey:            cacheKey,
		cacheSplit:          cacheSplit,
		limits:              parseAnswerLimits(body),
	}, nil
}

var lightweightTitleRe = regexp.MustCompile(`(?i)\b(?:generate\s+a\s+(?:concise|short|brief)[,\s]+(?:single[- ]line\s+)?(?:task\s+|session\s+|chat\s+|conversation\s+)?title|write\s+a\s+(?:concise|short|brief)\s+(?:3[-–]6\s+word\s+)?title\s+for\s+this|summarize\s+this\s+(?:conversation|message|chat)\s+(?:in|into)\s+a\s+short\s+title)\b`)

func isLightweightTitleRequest(messages []map[string]any) bool {
	if len(messages) == 0 || len(messages) > 4 {
		return false
	}
	for _, m := range messages {
		if m == nil {
			continue
		}
		if s, ok := m["content"].(string); ok && len(s) <= 2000 && lightweightTitleRe.MatchString(s) {
			return true
		}
	}
	return false
}
