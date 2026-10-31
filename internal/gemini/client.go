package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
	"log"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	Origin        = "https://gemini.google.com"
	DefaultBL     = "boq_assistant-bard-web-server_20260415.09_p0"
	modelsTTL     = 10 * time.Minute
	xsrfTTL       = 20 * time.Minute
	defaultTenant = "bard-storage"
	defaultPushID = "feeds/mcudyrk2a4khkz"

	quotaPayloadFlash    = "[[[1,11],[2,11],[6,11]]]"
	quotaPayloadAdvanced = "[[[1,4],[6,6],[1,15]]]"
)

var (
	quotaActionLabels = map[int]string{
		4:  "Gemini Pro",
		11: "Gemini Flash",
		15: "Gemini Flash Thinking",
	}

	snlm0eRe     = regexp.MustCompile(`"SNlM0e":"([^"]+)"`)
	cfb2hRe      = regexp.MustCompile(`"cfb2h":"([^"]+)"`)
	fdrfjeRe     = regexp.MustCompile(`"FdrFJe":"([^"]+)"`)
	pushIDRe     = regexp.MustCompile(`"(?:qKIAYe|q2k)":"(feeds/[a-z0-9]+)"`)
	pctxRe       = regexp.MustCompile(`"Ylro7b":"([^"]{4,400})"`)
	emailRe      = regexp.MustCompile(`"oPEP7c":"([^"]+)"`)
	emailAltRe   = regexp.MustCompile(`"(?:email|userEmail)":"([^"]+)"`)
	tierRe       = regexp.MustCompile(`"uJ6Dbe":"([^"]+)"`)
	whitespaceRe = regexp.MustCompile(`\s+`)
)

// AvailableModelsResult holds the live model catalog, capacity, and tier flags for an account.
type AvailableModelsResult struct {
	Status        int               `json:"status"`
	Headers       map[string]string `json:"-"`
	Models        []ModelInfo       `json:"models"`
	TierFlags     []int             `json:"tierFlags"`
	CapFlags      []int             `json:"capFlags"`
	Capacity      int               `json:"capacity"`
	CapacityField int               `json:"capacityField"`
}

// BatchEntry is a single parsed rpcid result from batchexecute.
type BatchEntry struct {
	RPCID   string
	Payload any
}

// AccountClient is an isolated, per-account Gemini Web client backed by a browserless
// Chrome uTLS/HTTP2 session and persistent dual-path cookie rotation.
type AccountClient struct {
	ID       string
	FilePath string
	Pool     *transport.Pool
	Rotator  *cookies.Rotator

	mu               sync.RWMutex
	bootMu           sync.Mutex
	cookieHeader     string
	cookieGen        atomic.Uint64
	email            string
	userTier         string
	at               string
	bl               string
	fSid             string
	pushID           string
	pctx             string
	bootstrappedAt   time.Time
	fileMtimeMs      int64
	modelsCache      *AvailableModelsResult
	modelsCachedAt   time.Time
	reqCounter       atomic.Int64
	capturedAt       string
	rotatedAt        string
	cookieDB         string
	importedFrom     string
	profileID        string
	tokenFetchedAt   string
	tokenSource      string
	fromDiskAT       bool
	quarantined      bool
	quarantineReason string
	disabled         bool
	accountGroup     string
	proxyURL         string
	surfaces         map[string]string
	locale           string
	blOverride       string
	protocolMode     string
	authUser         int
}

// NewAccountClient creates an AccountClient for a specific Google account.
func NewAccountClient(id, cookieHeader, filePath string, pool *transport.Pool, rotator *cookies.Rotator) *AccountClient {
	ac := &AccountClient{
		ID:           id,
		FilePath:     filePath,
		Pool:         pool,
		Rotator:      rotator,
		cookieHeader: cookieHeader,
		pushID:       defaultPushID,
		locale:       "en",
		protocolMode: "bridge",
		surfaces:     make(map[string]string),
	}
	ac.reqCounter.Store(100000 + time.Now().UnixNano()%800000)
	return ac
}

// LoadAccountFromFile loads an AccountClient from an account JSON file.
func LoadAccountFromFile(path string, pool *transport.Pool, rotator *cookies.Rotator) (*AccountClient, error) {
	rec, mtime, err := cookies.LoadAccountFile(path)
	if err != nil {
		return nil, err
	}
	id := rec.Name
	if id == "" {
		id = "default"
	}
	ac := NewAccountClient(id, rec.Cookies, path, pool, rotator)
	ac.email = rec.Email
	ac.at = rec.AT
	ac.bl = rec.BL
	ac.fSid = rec.FSid
	ac.authUser = rec.AuthUser
	ac.fileMtimeMs = mtime
	ac.capturedAt = rec.CapturedAt
	ac.rotatedAt = rec.RotatedAt
	ac.cookieDB = rec.CookieDB
	ac.importedFrom = rec.ImportedFrom
	ac.profileID = rec.ProfileID
	ac.tokenFetchedAt = rec.TokenFetchedAt
	ac.tokenSource = rec.TokenSource
	ac.quarantined = rec.Quarantined
	ac.quarantineReason = rec.QuarantineReason
	ac.disabled = rec.Disabled
	ac.accountGroup = rec.AccountGroup
	ac.proxyURL = rec.ProxyURL
	if len(rec.Surfaces) > 0 {
		for k, v := range rec.Surfaces {
			ac.surfaces[k] = v
		}
	}
	if rec.AT != "" {
		ac.fromDiskAT = true
	}
	if rec.FSid != "" && rec.LastReqID >= 100000 {
		ac.reqCounter.Store(rec.LastReqID)
	}
	return ac, nil
}

// SetProvenance records the browser cookie DB and profile provenance on the AccountClient.
func (ac *AccountClient) SetProvenance(cookieDB, profileHint string) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	ac.capturedAt = time.Now().UTC().Format(time.RFC3339)
	ac.quarantined = false
	ac.quarantineReason = ""
	if ac.surfaces == nil {
		ac.surfaces = make(map[string]string)
	}
	ac.surfaces["geminiWeb"] = "ok"
	if cookieDB != "" {
		ac.cookieDB = cookieDB
	}
	bName, profID := cookies.ParseProfileHint(profileHint)
	if bName == "" && cookieDB != "" {
		bName, profID = cookies.ParseProfileHint(cookieDB)
	}
	if bName != "" {
		ac.importedFrom = bName
	}
	if profID != "" {
		ac.profileID = profID
	}
}

// SetRuntimeOptions configures locale, BL override, protocol mode, group, and proxy URL.
func (ac *AccountClient) SetRuntimeOptions(locale, blOverride, protocolMode, accountGroup, proxyURL string) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if locale != "" {
		ac.locale = locale
	}
	ac.blOverride = blOverride
	if protocolMode != "" {
		ac.protocolMode = protocolMode
	}
	if accountGroup != "" {
		ac.accountGroup = accountGroup
	}
	if proxyURL != "" {
		ac.proxyURL = proxyURL
	}
}

// SetQuarantined updates the quarantine state and persists it to disk.
func (ac *AccountClient) SetQuarantined(quarantined bool, reason string) {
	ac.mu.Lock()
	ac.quarantined = quarantined
	ac.quarantineReason = reason
	if ac.surfaces == nil {
		ac.surfaces = make(map[string]string)
	}
	if quarantined {
		ac.surfaces["geminiWeb"] = "blocked"
	} else {
		ac.surfaces["geminiWeb"] = "ok"
	}
	ac.mu.Unlock()
	ac.persistState()
}

// SetDisabled updates the operator-disabled flag and persists it to disk.
func (ac *AccountClient) SetDisabled(disabled bool) {
	ac.mu.Lock()
	ac.disabled = disabled
	ac.mu.Unlock()
	ac.persistState()
}

// SetSurfaceStatus updates a surface status (e.g. "geminiWeb": "ok") and persists to disk.
func (ac *AccountClient) SetSurfaceStatus(surface, status string) {
	ac.mu.Lock()
	if ac.surfaces == nil {
		ac.surfaces = make(map[string]string)
	}
	ac.surfaces[surface] = status
	ac.mu.Unlock()
	ac.persistState()
}

// ReloadFromDiskIfNewer checks if the account JSON file was updated externally
// (e.g. by web-login or scp) and reloads its cookies and tokens in place.
func (ac *AccountClient) ReloadFromDiskIfNewer() bool {
	ac.mu.RLock()
	path := ac.FilePath
	lastMtime := ac.fileMtimeMs
	ac.mu.RUnlock()
	if path == "" {
		return false
	}
	rec, mtime, err := cookies.LoadAccountFile(path)
	if err != nil || mtime <= lastMtime || rec.Cookies == "" {
		return false
	}
	ac.mu.Lock()
	changed := rec.Cookies != ac.cookieHeader
	ac.cookieHeader = rec.Cookies
	if rec.Email != "" {
		ac.email = rec.Email
	}
	if rec.AT != "" {
		ac.at = rec.AT
		ac.fromDiskAT = true
	}
	if rec.BL != "" {
		ac.bl = rec.BL
	}
	if rec.FSid != "" {
		ac.fSid = rec.FSid
	}
	ac.authUser = rec.AuthUser

	if rec.CapturedAt != "" {
		ac.capturedAt = rec.CapturedAt
	}
	if rec.RotatedAt != "" {
		ac.rotatedAt = rec.RotatedAt
	}
	ac.quarantined = rec.Quarantined
	ac.quarantineReason = rec.QuarantineReason
	ac.fileMtimeMs = mtime
	if changed {
		ac.cookieGen.Add(1)
		ac.bootstrappedAt = time.Time{}
		ac.modelsCache = nil
	}
	ac.mu.Unlock()

	if changed && ac.Pool != nil {
		if s := ac.Pool.Existing(ac.sessionKey()); s != nil {
			s.QueueCookieUpdate(rec.Cookies)
		}
	}
	return changed
}

// Email returns the account's Google email address.
func (ac *AccountClient) Email() string {
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	return ac.email
}

// SetTokens sets pre-existing SNlM0e (at), cfb2h (bl), and FdrFJe (fSid) tokens.
func (ac *AccountClient) SetTokens(at, bl, fSid string) {
	ac.mu.Lock()
	if at != "" {
		ac.at = at
		ac.fromDiskAT = false
		ac.bootstrappedAt = time.Now()
	}
	if bl != "" {
		ac.bl = bl
	}
	if fSid != "" {
		ac.fSid = fSid
	}
	ac.mu.Unlock()
}

// CookieHeader returns the current Cookie header string.
func (ac *AccountClient) CookieHeader() string {
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	return ac.cookieHeader
}

// InvalidateAuth clears the cached SNlM0e token so the next call re-bootstraps.
func (ac *AccountClient) InvalidateAuth() {
	ac.mu.Lock()
	ac.bootstrappedAt = time.Now().Add(-2 * xsrfTTL)
	ac.mu.Unlock()
}

func (ac *AccountClient) sessionKey() string {
	if ac.ID != "" {
		return ac.ID
	}
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	if ac.email != "" {
		return ac.email
	}
	return "default"
}

func (ac *AccountClient) absorbAndPersistCookies(setCookies []string) bool {
	return ac.absorbAndPersistCookiesWithGen(setCookies, 0, false)
}

func (ac *AccountClient) absorbAndPersistCookiesWithGen(setCookies []string, expectedGen uint64, enforceGen bool) bool {
	if len(setCookies) == 0 {
		return false
	}
	ac.mu.Lock()
	if enforceGen && ac.cookieGen.Load() != expectedGen {
		ac.mu.Unlock()
		return false
	}
	merged, changed := cookies.AbsorbSetCookie(ac.cookieHeader, setCookies)
	if !changed {
		ac.mu.Unlock()
		return false
	}
	ac.cookieHeader = merged
	ac.cookieGen.Add(1)
	ac.mu.Unlock()

	if ac.Pool != nil {
		if s := ac.Pool.Existing(ac.sessionKey()); s != nil {
			s.QueueCookieUpdate(merged)
		}
	}
	ac.persistState()
	return true
}

func (ac *AccountClient) persistState() {
	ac.mu.Lock()
	path := ac.FilePath
	if path == "" {
		ac.mu.Unlock()
		return
	}
	nowISO := time.Now().UTC().Format(time.RFC3339)
	if ac.capturedAt == "" {
		ac.capturedAt = nowISO
	}
	ac.rotatedAt = nowISO
	surfCopy := make(map[string]string, len(ac.surfaces))
	for k, v := range ac.surfaces {
		surfCopy[k] = v
	}
	rec := cookies.CredentialFile{
		Kind:             "gemini-web",
		Name:             ac.ID,
		Email:            ac.email,
		Cookies:          ac.cookieHeader,
		AT:               ac.at,
		BL:               ac.bl,
		FSid:             ac.fSid,
		AuthUser:         ac.authUser,
		CapturedAt:       ac.capturedAt,
		RotatedAt:        ac.rotatedAt,
		CookieDB:         ac.cookieDB,
		ImportedFrom:     ac.importedFrom,
		ProfileID:        ac.profileID,
		TokenFetchedAt:   ac.tokenFetchedAt,
		TokenSource:      ac.tokenSource,
		LastReqID:        ac.reqCounter.Load(),
		Quarantined:      ac.quarantined,
		QuarantineReason: ac.quarantineReason,
		Disabled:         ac.disabled,
		AccountGroup:     ac.accountGroup,
		ProxyURL:         ac.proxyURL,
		Surfaces:         surfCopy,
	}
	ac.mu.Unlock()
	if mtime, err := cookies.SaveCredentialFile(path, rec); err == nil {
		ac.mu.Lock()
		ac.fileMtimeMs = mtime
		ac.mu.Unlock()
	}
}

func classifyUpstreamAuthOrBan(status int, headers map[string]string, body string, defaultCode string) *transport.UpstreamError {
	if status == 403 && (strings.Contains(body, "TOS_VIOLATION") || strings.Contains(body, "PERMISSION_DENIED")) {
		return transport.NewUpstreamError("account banned or restricted upstream (TOS_VIOLATION / PERMISSION_DENIED)", 403, "account_banned", headers, nil)
	}
	if status == 302 || status == 301 || status == 303 || status == 307 || status == 308 {
		loc := ""
		if headers != nil {
			loc = headers["location"]
		}
		if strings.Contains(loc, "accounts.google.com") || strings.Contains(loc, "ServiceLogin") || loc != "" {
			return transport.NewUpstreamError("upstream redirected to Google sign-in", 401, "auth_redirect", headers, nil)
		}
	}
	if status == 429 {
		return transport.NewUpstreamError("upstream rate limited", 429, "rate_limit_error", headers, nil)
	}
	code := defaultCode
	outStatus := status
	if status == 401 || status == 403 {
		code = "upstream_auth_error"
	}
	return transport.NewUpstreamError(fmt.Sprintf("upstream HTTP %d", status), outStatus, code, headers, nil)
}

func (ac *AccountClient) acquireSession(ctx context.Context) (*transport.Session, error) {
	ac.ReloadFromDiskIfNewer()
	ac.mu.RLock()
	proxy := ac.proxyURL
	ac.mu.RUnlock()
	return ac.Pool.AcquireWithProxy(ctx, ac.sessionKey(), ac.CookieHeader(), proxy)
}

// Bootstrap loads https://gemini.google.com/app over the account's stealth session
// and extracts SNlM0e (at), cfb2h (bl), FdrFJe (f.sid), pushId, pctx, and email.
func (ac *AccountClient) Bootstrap(ctx context.Context) error {
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return err
	}
	ac.bootMu.Lock()
	defer ac.bootMu.Unlock()
	return ac.bootstrapOnClient(ctx, sess.Client)
}

func (ac *AccountClient) bootstrapOnClient(ctx context.Context, sc *transport.StealthClient) error {
	ac.mu.RLock()
	authUser := ac.authUser
	ac.mu.RUnlock()
	appURL := Origin + "/app"
	if authUser > 0 {
		appURL = Origin + "/u/" + strconv.Itoa(authUser) + "/app"
	}
	doFetchURL := func(target string) (*transport.Response, error) {
		res, err := sc.Do(ctx, transport.RequestOptions{
			Method:  "GET",
			URL:     target,
			Kind:    transport.ReqKindNavigate,
			Headers: map[string]string{"cookie": ac.CookieHeader()},
			Timeout: 30 * time.Second,
		})
		if err != nil {
			return nil, err
		}
		ac.absorbAndPersistCookies(res.SetCookies)
		return res, nil
	}
	doFetchApp := func() (*transport.Response, error) { return doFetchURL(appURL) }

	res, err := doFetchApp()
	if err != nil {
		return err
	}
	// A stored authUser may not resolve (e.g. the account is no longer on the
	// Google account chooser, or the cookies belong to the profile's primary
	// account). Fall back to the default /app surface instead of failing.
	if authUser > 0 && (res.Status == 301 || res.Status == 302 || res.Status == 303 || res.Status == 307 || res.Status == 308) {
		appURL = Origin + "/app"
		res, err = doFetchApp()
		if err != nil {
			return err
		}
	}

	at := ""
	if res.Status == 200 {
		at = matchFirst(snlm0eRe, res.Body)
	}
	// Self-heal expired __Secure-1PSIDTS / SIDCC before declaring session dead:
	// if /app failed or returned no SNlM0e token, run RotateCookies once and retry.
	var lastRotRes *cookies.RotateResult
	if res.Status != 200 || at == "" {
		if rotRes, rotErr := ac.rotateOnClient(ctx, sc); rotErr == nil && rotRes != nil {
			lastRotRes = rotRes
			if rotRes.Rotated {
				if retryRes, retryErr := doFetchApp(); retryErr == nil {
					res = retryRes
					if res.Status == 200 {
						at = matchFirst(snlm0eRe, res.Body)
					}
				}
			}
		}
	}

	if lastRotRes != nil && lastRotRes.Status == 429 && at == "" {
		return transport.NewUpstreamError("cookie rotation rate limited (HTTP 429)", 429, "rate_limit_error", res.Headers, nil)
	}

	if res.Status != 200 {
		return classifyUpstreamAuthOrBan(res.Status, res.Headers, res.Body, "upstream_http_error")
	}

	bl := matchFirst(cfb2hRe, res.Body)
	fSid := matchFirst(fdrfjeRe, res.Body)
	pushID := matchFirst(pushIDRe, res.Body)
	pctx := matchFirst(pctxRe, res.Body)
	email := matchFirst(emailRe, res.Body)
	if email == "" {
		email = matchFirst(emailAltRe, res.Body)
	}
	tier := matchFirst(tierRe, res.Body)

	if at == "" {
		if strings.Contains(res.Body, "TOS_VIOLATION") || strings.Contains(res.Body, "PERMISSION_DENIED") {
			return transport.NewUpstreamError("account banned or restricted upstream (TOS_VIOLATION / PERMISSION_DENIED)", 403, "account_banned", res.Headers, nil)
		}
		return transport.NewUpstreamError(
			"could not extract SNlM0e (at token); the saved session is expired or incomplete — run `gsb --login <name>` to sign in again, then re-import (browser cookie exports copied without signing in usually expire immediately)",
			401,
			"upstream_auth_error",
			res.Headers,
			nil,
		)
	}
	nowISO := time.Now().UTC().Format(time.RFC3339)
	ac.mu.Lock()
	ac.at = at
	ac.tokenFetchedAt = nowISO
	ac.tokenSource = "bridge"
	ac.fromDiskAT = false
	if ac.blOverride != "" {
		ac.bl = ac.blOverride
		log.Printf("[gemini] account %s using BRIDGE_BL_OVERRIDE=%s", ac.ID, ac.blOverride)
	} else if bl != "" {
		ac.bl = bl
	} else if ac.bl == "" {
		log.Printf("[gemini] WARNING: account %s could not extract cfb2h build label; falling back to DefaultBL", ac.ID)
		ac.bl = DefaultBL
	}
	if fSid != "" {
		ac.fSid = fSid
	}
	if pushID != "" {
		ac.pushID = pushID
	}
	if pctx != "" {
		ac.pctx = pctx
	}
	if email != "" {
		ac.email = email
	}
	if tier != "" {
		ac.userTier = tier
	}
	ac.bootstrappedAt = time.Now()
	ac.mu.Unlock()

	ac.persistState()
	return nil
}

func (ac *AccountClient) ensureBootstrapped(ctx context.Context, sc *transport.StealthClient) error {
	ac.mu.RLock()
	at := ac.at
	fromDisk := ac.fromDiskAT
	bootAt := ac.bootstrappedAt
	age := time.Since(bootAt)
	ac.mu.RUnlock()
	if at != "" && !fromDisk && !bootAt.IsZero() && age < xsrfTTL {
		return nil
	}
	ac.bootMu.Lock()
	defer ac.bootMu.Unlock()
	ac.mu.RLock()
	at = ac.at
	bl := ac.bl
	fSid := ac.fSid
	fromDisk = ac.fromDiskAT
	bootAt = ac.bootstrappedAt
	age = time.Since(bootAt)
	ac.mu.RUnlock()
	if at != "" && !fromDisk && !bootAt.IsZero() && age < xsrfTTL {
		return nil
	}
	if fromDisk && at != "" && bl != "" && fSid != "" {
		if res, err := ac.doBatchExec(ctx, sc, "otAQ7b", "[]"); err == nil && res != nil && res.Status == 200 {
			entries := ParseBatch(res.Body)
			p := findBatchPayload(entries, "otAQ7b")
			if p != nil && !isStatus1016(p) {
				ac.mu.Lock()
				ac.fromDiskAT = false
				ac.bootstrappedAt = time.Now()
				ac.mu.Unlock()
				return nil
			}
		}
	}
	return ac.bootstrapOnClient(ctx, sc)
}

// RotateCookies runs the dual-path cookie rotation for this account and persists
// any rotated cookies to disk and to the active session without blocking active generation leases.
func (ac *AccountClient) RotateCookies(ctx context.Context) (*cookies.RotateResult, error) {
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return nil, err
	}
	return ac.rotateOnClient(ctx, sess.Client)
}

func (ac *AccountClient) rotateOnClient(ctx context.Context, sc *transport.StealthClient) (*cookies.RotateResult, error) {
	if ac.Rotator == nil {
		ac.Rotator = cookies.NewRotator()
	}
	res, err := ac.Rotator.RotateAccount(ctx, ac.sessionKey(), ac.CookieHeader(), sc)
	if err != nil {
		return nil, err
	}
	if res.Rotated && res.CookieHeader != "" {
		ac.mu.Lock()
		ac.cookieHeader = res.CookieHeader
		ac.cookieGen.Add(1)
		ac.mu.Unlock()
		if ac.Pool != nil {
			if s := ac.Pool.Existing(ac.sessionKey()); s != nil {
				s.QueueCookieUpdate(res.CookieHeader)
			}
		}
		ac.persistState()
	}
	return res, nil
}

// BatchExec calls /_/BardChatUi/data/batchexecute for the given RPC calls.
func (ac *AccountClient) BatchExec(ctx context.Context, rpcID, payloadJSON string) (*transport.Response, []BatchEntry, error) {
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return nil, nil, err
	}
	if bErr := ac.ensureBootstrapped(ctx, sess.Client); bErr != nil {
		return nil, nil, bErr
	}
	res, err := ac.doBatchExec(ctx, sess.Client, rpcID, payloadJSON)
	if err != nil {
		return nil, nil, err
	}
	return res, ParseBatch(res.Body), nil
}

func (ac *AccountClient) doBatchExec(ctx context.Context, sc *transport.StealthClient, rpcID, payloadJSON string) (*transport.Response, error) {
	ac.mu.RLock()
	at := ac.at
	bl := ac.bl
	fSid := ac.fSid
	cookie := ac.cookieHeader
	hl := ac.locale
	authUser := ac.authUser
	ac.mu.RUnlock()
	if hl == "" {
		hl = "en"
	}

	sourcePath := "/app"
	if authUser > 0 {
		sourcePath = "/u/" + strconv.Itoa(authUser) + "/app"
	}

	reqID := ac.nextReqID()
	q := url.Values{}
	q.Set("rpcids", rpcID)
	q.Set("source-path", sourcePath)
	q.Set("bl", bl)
	q.Set("f.sid", fSid)
	q.Set("hl", hl)
	q.Set("_reqid", strconv.FormatInt(reqID, 10))
	q.Set("rt", "c")
	if authUser > 0 {
		q.Set("authuser", strconv.Itoa(authUser))
	}

	freq, _ := json.Marshal([][][]any{{{rpcID, payloadJSON, nil, "generic"}}})
	form := url.Values{}
	form.Set("at", at)
	form.Set("f.req", string(freq))

	modelHdr, _ := json.Marshal([]any{
		1, nil, nil, nil, nil, nil, nil, nil, []int{4, 5, 6, 8},
		nil, nil, nil, nil, nil, nil, nil, fSid,
	})

	headers := map[string]string{
		"content-type":              "application/x-www-form-urlencoded;charset=utf-8",
		"x-same-domain":             "1",
		"x-goog-ext-525001261-jspb": string(modelHdr),
		"x-goog-ext-73010989-jspb":  "[0]",
		"cookie":                    cookie,
	}
	if auth := cookies.SAPISIDHash(cookie, Origin); auth != "" {
		headers["authorization"] = auth
	}

	res, err := sc.Do(ctx, transport.RequestOptions{
		Method:  "POST",
		URL:     Origin + "/_/BardChatUi/data/batchexecute?" + q.Encode(),
		Kind:    transport.ReqKindCORS,
		Headers: headers,
		Body:    []byte(form.Encode()),
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	ac.absorbAndPersistCookies(res.SetCookies)
	return res, nil
}

// ParseBatch parses a batchexecute response body into BatchEntry items.
func ParseBatch(text string) []BatchEntry {
	body := strings.TrimLeft(text, ")]}' \t\r\n")
	var out []BatchEntry
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || isDigits(t) {
			continue
		}
		var arr []any
		if err := json.Unmarshal([]byte(t), &arr); err != nil {
			continue
		}
		for _, entryAny := range arr {
			entry, ok := entryAny.([]any)
			if !ok || len(entry) < 3 {
				continue
			}
			tag, _ := entry[0].(string)
			if tag != "wrb.fr" {
				continue
			}
			rpcID, _ := entry[1].(string)
			payload := entry[2]
			if s, ok := payload.(string); ok {
				var parsed any
				if err := json.Unmarshal([]byte(s), &parsed); err == nil {
					payload = parsed
				}
			}
			out = append(out, BatchEntry{RPCID: rpcID, Payload: payload})
		}
	}
	return out
}

// AvailableModels queries the live otAQ7b RPC (`GET_USER_STATUS`), re-bootstrapping
// on code 1016 if needed, and computes the account's live model catalog and capacity.
func (ac *AccountClient) AvailableModels(ctx context.Context, force bool) (*AvailableModelsResult, error) {
	if !force {
		ac.mu.RLock()
		cached := ac.modelsCache
		cachedAt := ac.modelsCachedAt
		ac.mu.RUnlock()
		if cached != nil && len(cached.Models) > 0 && time.Since(cachedAt) < modelsTTL {
			return cached, nil
		}
	}

	res, entries, err := ac.BatchExec(ctx, "otAQ7b", "[]")
	if err != nil {
		return nil, err
	}
	if res.Status != 200 {
		return nil, classifyUpstreamAuthOrBan(res.Status, res.Headers, res.Body, "model_discovery_error")
	}

	p := findBatchPayload(entries, "otAQ7b")
	if isStatus1016(p) {
		ac.InvalidateAuth()
		if bErr := ac.Bootstrap(ctx); bErr != nil {
			return nil, bErr
		}
		res, entries, err = ac.BatchExec(ctx, "otAQ7b", "[]")
		if err != nil {
			return nil, err
		}
		p = findBatchPayload(entries, "otAQ7b")
	}

	if isStatus1016(p) {
		return nil, transport.NewUpstreamError("session is unauthenticated or the saved cookies have expired (code 1016) — run `gsb --login <name>` to sign in again and re-import the account", 401, "upstream_auth_error", res.Headers, nil)
	}

	pArr, ok := p.([]any)
	if !ok || len(pArr) <= 15 {
		return nil, transport.NewUpstreamError("malformed upstream model registry", 502, "model_discovery_error", res.Headers, nil)
	}
	rawModels, ok := pArr[15].([]any)
	if !ok || len(rawModels) == 0 {
		return nil, transport.NewUpstreamError("malformed upstream model registry", 502, "model_discovery_error", res.Headers, nil)
	}
	if len(pArr) > 16 && pArr[16] != nil {
		if _, ok := pArr[16].([]any); !ok {
			return nil, transport.NewUpstreamError("malformed upstream model registry", 502, "model_discovery_error", res.Headers, nil)
		}
	}
	if len(pArr) > 17 && pArr[17] != nil {
		if _, ok := pArr[17].([]any); !ok {
			return nil, transport.NewUpstreamError("malformed upstream model registry", 502, "model_discovery_error", res.Headers, nil)
		}
	}

	var tierFlags, capFlags []int
	if len(pArr) > 16 {
		tierFlags = toIntSlice(pArr[16])
	}
	if len(pArr) > 17 {
		capFlags = toIntSlice(pArr[17])
	}
	capacity, capacityField := ComputeCapacity(tierFlags, capFlags)

	models := make([]ModelInfo, 0, len(rawModels))
	thinkingPolicy := ParseThinkingPolicy(payloadAt(pArr, 24))
	for _, mdAny := range rawModels {
		md, ok := mdAny.([]any)
		if !ok || len(md) == 0 {
			return nil, transport.NewUpstreamError("malformed upstream model entry", 502, "model_discovery_error", res.Headers, nil)
		}
		modelID, ok := md[0].(string)
		if !ok || strings.TrimSpace(modelID) == "" {
			return nil, transport.NewUpstreamError("malformed upstream model entry", 502, "model_discovery_error", res.Headers, nil)
		}
		category := stringAt(md, 1)
		if category == "" {
			category = stringAt(md, 10)
		}
		displayName := stringAt(md, 11)
		if displayName == "" {
			displayName = stringAt(md, 19)
		}
		if displayName == "" {
			displayName = stringAt(md, 1)
		}
		description := stringAt(md, 12)
		if description == "" {
			description = stringAt(md, 2)
		}
		modelNumber := 1
		if n, ok := intAt(md, 17); ok {
			modelNumber = n
		} else if n, ok := intAt(md, 9); ok {
			modelNumber = n
		}

		catSlug := whitespaceRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(category)), "-")
		modelName := "gemini-" + modelID
		if catSlug != "" {
			modelName = "gemini-" + catSlug
		}
		dispSlug := whitespaceRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(displayName)), "-")
		var aliases []string
		if dispSlug != "" && "gemini-"+dispSlug != modelName {
			aliases = append(aliases, "gemini-"+dispSlug)
		}
		disp := displayName
		if disp == "" {
			disp = category
		}
		if disp == "" {
			disp = modelID
		}

		models = append(models, ModelInfo{
			ModelID:             modelID,
			ModelName:           modelName,
			DisplayName:         disp,
			Aliases:             aliases,
			Description:         description,
			Capacity:            capacity,
			CapacityField:       capacityField,
			ModelNumber:         modelNumber,
			ThinkingLevels:      thinkingPolicy[modelNumber],
			ThinkingPolicyKnown: len(thinkingPolicy) > 0,
		})
	}

	result := &AvailableModelsResult{
		Status:        res.Status,
		Headers:       res.Headers,
		Models:        models,
		TierFlags:     tierFlags,
		CapFlags:      capFlags,
		Capacity:      capacity,
		CapacityField: capacityField,
	}

	ac.mu.Lock()
	ac.modelsCache = result
	ac.modelsCachedAt = time.Now()
	ac.mu.Unlock()

	return result, nil
}

// GetQuota queries the qpEbW RPC (`CHECK_GEMINI_QUOTA`) for both Flash and Pro/Advanced buckets.
func (ac *AccountClient) GetQuota(ctx context.Context) []QuotaBucket {
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return nil
	}
	if bErr := ac.ensureBootstrapped(ctx, sess.Client); bErr != nil {
		return nil
	}

	payloads := []string{quotaPayloadFlash, quotaPayloadAdvanced}
	batchResults := make([][]BatchEntry, 0, len(payloads))
	for _, pl := range payloads {
		res, rErr := ac.doBatchExec(ctx, sess.Client, "qpEbW", pl)
		if rErr != nil || res == nil {
			break
		}
		if res.Status == 401 || res.Status == 403 || res.Status == 429 || res.Status >= 500 {
			break
		}
		batchResults = append(batchResults, ParseBatch(res.Body))
	}

	var out []QuotaBucket
	for _, entries := range batchResults {
		if len(entries) == 0 {
			continue
		}
		p := findBatchPayload(entries, "qpEbW")
		pArr, ok := p.([]any)
		if !ok || len(pArr) == 0 {
			continue
		}
		items, ok := pArr[0].([]any)
		if !ok {
			continue
		}
		for _, itemAny := range items {
			item, ok := itemAny.([]any)
			if !ok || len(item) < 6 {
				continue
			}
			idList, _ := item[0].([]any)
			if len(idList) < 2 {
				continue
			}
			actionID, ok := intAt(idList, 1)
			if !ok {
				continue
			}
			label, exists := quotaActionLabels[actionID]
			if !exists {
				continue
			}
			var idParts []string
			for _, v := range idList {
				if f, ok := v.(float64); ok {
					idParts = append(idParts, strconv.Itoa(int(f)))
				}
			}
			var usagePct *float64
			if f, ok := item[2].(float64); ok {
				val := f
				usagePct = &val
			}
			var resetAt string
			if rArr, ok := item[3].([]any); ok && len(rArr) > 0 {
				if ts, ok := rArr[0].(float64); ok && ts > 0 {
					resetAt = time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)
				}
			}
			total, _ := intAt(item, 4)
			remaining, _ := intAt(item, 5)
			out = append(out, QuotaBucket{
				ID:              strings.Join(idParts, "-"),
				Label:           label,
				UsagePercentage: usagePct,
				Total:           total,
				Remaining:       remaining,
				ResetAt:         resetAt,
				Unlimited:       total == 0 && remaining == 0,
			})
		}
	}
	return ac.enrichWithGraphqlUsagePools(ctx, sess.Client, out)
}

const (
	graphqlUsageQuerySig  = "2/eV2urSc0npZclHCYNGf6nOVD14WHCs52VfsJoHiZgEs="
	graphqlUsageVariables = `{"g1Request":{"entrypoints":["GEMINI_USAGE_SETTINGS_UPGRADE_BANNER","GEMINI_USAGE_SETTINGS_AI_CREDITS_UPSELL_CARD","GEMINI_USAGE_SETTINGS_UPGRADE_AND_CREDITS_BANNER"]},"workspaceRequest":{"surface":"GEMINI_CHAT_USAGE_LIMITS_UPSELL"}}`
)

type graphqlUsagePool struct {
	PoolType       string  `json:"poolType"`
	PercentageUsed float64 `json:"percentageUsed"`
	Gxu            struct {
		WindowRefreshesAt string `json:"windowRefreshesAt"`
	} `json:"gxu"`
}

func (ac *AccountClient) enrichWithGraphqlUsagePools(ctx context.Context, sc *transport.StealthClient, base []QuotaBucket) []QuotaBucket {
	if sc == nil {
		return base
	}
	q := url.Values{}
	q.Set("query_signature", graphqlUsageQuerySig)
	q.Set("variables", graphqlUsageVariables)
	u := Origin + "/_/BardChatUi/graphql/schemas/GEMINI_WEB_GRAPHQL/executeQuery?" + q.Encode()
	res, err := sc.Do(ctx, transport.RequestOptions{
		Method: "GET",
		URL:    u,
		Kind:   transport.ReqKindCORS,
		Headers: map[string]string{
			"cookie": ac.CookieHeader(),
			"accept": "application/json",
		},
		Timeout: 10 * time.Second,
	})
	if err != nil || res == nil || res.Status != 200 {
		return base
	}
	ac.absorbAndPersistCookies(res.SetCookies)

	body := strings.TrimSpace(res.Body)
	if strings.HasPrefix(body, ")]}'") {
		body = strings.TrimSpace(body[4:])
	}
	var chunks []struct {
		Data struct {
			Viewer struct {
				Rpcs struct {
					GetUsageInfo struct {
						Pools []graphqlUsagePool `json:"pools"`
					} `json:"getUsageInfo"`
				} `json:"rpcs"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &chunks); err != nil || len(chunks) == 0 {
		return base
	}
	var pool5h, poolWeekly *graphqlUsagePool
	for _, ch := range chunks {
		for i := range ch.Data.Viewer.Rpcs.GetUsageInfo.Pools {
			p := &ch.Data.Viewer.Rpcs.GetUsageInfo.Pools[i]
			switch p.PoolType {
			case "GXU_FIVE_HOURLY":
				pool5h = p
			case "GXU_WEEKLY":
				poolWeekly = p
			}
		}
	}
	if pool5h == nil && poolWeekly == nil {
		return base
	}

	exact5hRem, exact5hTot := -1, 2400
	exactWeeklyRem, exactWeeklyTot := -1, 48384
	for _, b := range base {
		if b.Total > 5000 {
			exactWeeklyTot = b.Total
			exactWeeklyRem = b.Remaining
		} else if b.Total > 0 {
			exact5hTot = b.Total
			exact5hRem = b.Remaining
		}
	}

	var enriched []QuotaBucket
	if pool5h != nil {
		pct := math.Round(pool5h.PercentageUsed * 100)
		rem := exact5hRem
		if rem < 0 {
			rem = int(math.Round(float64(exact5hTot) * (1.0 - pool5h.PercentageUsed)))
			if rem < 0 {
				rem = 0
			}
		}
		resetAt := normalizeRFC3339(pool5h.Gxu.WindowRefreshesAt)
		enriched = append(enriched, QuotaBucket{
			ID:              "5h",
			Label:           "5h Window",
			UsagePercentage: &pct,
			Total:           exact5hTot,
			Remaining:       rem,
			ResetAt:         resetAt,
		})
	}
	if poolWeekly != nil {
		pct := math.Round(poolWeekly.PercentageUsed * 100)
		rem := exactWeeklyRem
		if rem < 0 {
			rem = int(math.Round(float64(exactWeeklyTot) * (1.0 - poolWeekly.PercentageUsed)))
			if rem < 0 {
				rem = 0
			}
		}
		resetAt := normalizeRFC3339(poolWeekly.Gxu.WindowRefreshesAt)
		enriched = append(enriched, QuotaBucket{
			ID:              "weekly",
			Label:           "Weekly Pool",
			UsagePercentage: &pct,
			Total:           exactWeeklyTot,
			Remaining:       rem,
			ResetAt:         resetAt,
		})
	}
	if len(enriched) == 2 && enriched[1].Remaining < enriched[0].Remaining {
		enriched[0].Remaining = enriched[1].Remaining
	}
	enriched = append(enriched, base...)
	return enriched
}

func normalizeRFC3339(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return raw
}

func (ac *AccountClient) nextReqID() int64 {
	return ac.reqCounter.Add(100000)
}

func findBatchPayload(entries []BatchEntry, rpcID string) any {
	for _, e := range entries {
		if e.RPCID == rpcID {
			return e.Payload
		}
	}
	return nil
}

func isStatus1016(p any) bool {
	arr, ok := p.([]any)
	if !ok || len(arr) <= 14 {
		return false
	}
	if f, ok := arr[14].(float64); ok && int(f) == 1016 {
		return true
	}
	return false
}

func toIntSlice(v any) []int {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]int, 0, len(arr))
	for _, item := range arr {
		if f, ok := item.(float64); ok {
			out = append(out, int(f))
		}
	}
	return out
}

func stringAt(arr []any, idx int) string {
	if idx >= 0 && idx < len(arr) {
		if s, ok := arr[idx].(string); ok {
			return s
		}
	}
	return ""
}

func intAt(arr []any, idx int) (int, bool) {
	if idx >= 0 && idx < len(arr) {
		if f, ok := arr[idx].(float64); ok && float64(int(f)) == f {
			return int(f), true
		}
	}
	return 0, false
}

func payloadAt(arr []any, idx int) any {
	if idx >= 0 && idx < len(arr) {
		return arr[idx]
	}
	return nil
}

func matchFirst(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}
