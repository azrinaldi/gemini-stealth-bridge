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
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	Origin        = "https://gemini.google.com"
	DefaultBL     = "boq_assistant-bard-web-server_20260415.09_p0"
	modelsTTL      = 10 * time.Minute
	xsrfTTL        = 20 * time.Minute
	xsrfPrewarmAge = 14 * time.Minute
	defaultTenant  = "bard-storage"
	defaultPushID = "feeds/mcudyrk2a4khkz"

	quotaPayloadFlash    = "[[[1,11],[2,11],[6,11]]]"
	quotaPayloadAdvanced = "[[[1,4],[6,6],[1,15]]]"
)

var (
	// Cookie versions must also order writes across replacement clients for the same ID.
	cookieVersion atomic.Uint64

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

	mu                     sync.RWMutex
	bootMu                 sync.Mutex
	persistMu              sync.Mutex
	persistRetired         atomic.Bool // set under persistMu after a successful replacement
	cookieHeader           string
	cookieGen              atomic.Uint64
	reloadGen              atomic.Uint64
	unsavedExplicitCookies bool
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
		ID:                     id,
		FilePath:               filePath,
		Pool:                   pool,
		Rotator:                rotator,
		cookieHeader:           cookieHeader,
		unsavedExplicitCookies: cookieHeader != "",
		pushID:                 defaultPushID,
		locale:                 "en",
		protocolMode:           "bridge",
		surfaces:               make(map[string]string),
	}
	if filePath != "" {
		if st, err := os.Stat(filePath); err == nil {
			ac.fileMtimeMs = st.ModTime().UnixMilli()
		}
	}
	ac.cookieGen.Store(cookieVersion.Add(1))
	ac.reqCounter.Store(100000 + time.Now().UnixNano()%800000)
	return ac
}

// SaveToFile binds the account client to path and persists its current state atomically.
func (ac *AccountClient) SaveToFile(path string) {
	ac.mu.Lock()
	ac.FilePath = path
	ac.mu.Unlock()
	ac.persistState()
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
	ac.unsavedExplicitCookies = false
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
	ac.accountGroup = accountGroup
	ac.proxyURL = proxyURL
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
	ac.persistStateWithOptions(false)
}

// SetDisabled updates the operator-disabled flag and persists it to disk.
func (ac *AccountClient) SetDisabled(disabled bool) {
	ac.mu.Lock()
	ac.disabled = disabled
	ac.mu.Unlock()
	ac.persistStateWithOptions(false)
}

// SetSurfaceStatus updates a surface status (e.g. "geminiWeb": "ok") and persists to disk.
func (ac *AccountClient) SetSurfaceStatus(surface, status string) {
	ac.mu.Lock()
	if ac.surfaces == nil {
		ac.surfaces = make(map[string]string)
	}
	ac.surfaces[surface] = status
	ac.mu.Unlock()
	ac.persistStateWithOptions(false)
}

// SetAuthUser updates the secondary Google account chooser index (/u/N/app) in memory.
func (ac *AccountClient) SetAuthUser(authUser int) {
	ac.mu.Lock()
	ac.authUser = authUser
	ac.mu.Unlock()
}

// AuthUser returns the configured secondary Google account chooser index (/u/N/app).
func (ac *AccountClient) AuthUser() int {
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	return ac.authUser
}

// ReloadFromDiskIfNewer checks if the account JSON file was updated externally
// (e.g. by web-login or scp) and reloads its cookies and tokens in place.
func (ac *AccountClient) ReloadFromDiskIfNewer() bool {
	ac.mu.RLock()
	path := ac.FilePath
	lastMtime := ac.fileMtimeMs
	unsaved := ac.unsavedExplicitCookies
	ac.mu.RUnlock()
	if path == "" || unsaved {
		return false
	}
	st, err := os.Stat(path)
	if err != nil || st.ModTime().UnixMilli() <= lastMtime {
		return false
	}

	ac.persistMu.Lock()
	if ac.persistRetired.Load() {
		ac.persistMu.Unlock()
		return false
	}
	rec, mtime, err := cookies.LoadAccountFile(path)
	if err != nil || rec.Cookies == "" {
		ac.persistMu.Unlock()
		return false
	}
	ac.mu.Lock()
	if ac.unsavedExplicitCookies || mtime <= ac.fileMtimeMs {
		ac.mu.Unlock()
		ac.persistMu.Unlock()
		return false
	}
	cookiesChanged := rec.Cookies != ac.cookieHeader
	prevAT := ac.at
	ac.cookieHeader = rec.Cookies
	if rec.Email != "" {
		ac.email = rec.Email
	}
	if rec.AT != "" {
		ac.at = rec.AT
		ac.fromDiskAT = true
		if rec.AT != prevAT {
			ac.bootstrappedAt = time.Time{}
		}
	} else if cookiesChanged {
		ac.at = ""
		ac.fromDiskAT = false
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
	if rec.CookieDB != "" {
		ac.cookieDB = rec.CookieDB
	}
	if rec.ImportedFrom != "" {
		ac.importedFrom = rec.ImportedFrom
	}
	if rec.ProfileID != "" {
		ac.profileID = rec.ProfileID
	}
	if rec.TokenFetchedAt != "" {
		ac.tokenFetchedAt = rec.TokenFetchedAt
	}
	if rec.TokenSource != "" {
		ac.tokenSource = rec.TokenSource
	}
	if rec.LastReqID >= 100000 && rec.LastReqID > ac.reqCounter.Load() {
		ac.reqCounter.Store(rec.LastReqID)
	}
	ac.quarantined = rec.Quarantined
	ac.quarantineReason = rec.QuarantineReason
	ac.disabled = rec.Disabled
	if rec.AccountGroup != "" {
		ac.accountGroup = rec.AccountGroup
	}
	if rec.ProxyURL != "" {
		ac.proxyURL = rec.ProxyURL
	}
	if len(rec.Surfaces) > 0 {
		if ac.surfaces == nil {
			ac.surfaces = make(map[string]string, len(rec.Surfaces))
		}
		for k, v := range rec.Surfaces {
			ac.surfaces[k] = v
		}
	}
	ac.fileMtimeMs = mtime
	var newGen uint64
	if cookiesChanged {
		newGen = cookieVersion.Add(1)
		ac.cookieGen.Store(newGen)
		ac.reloadGen.Add(1)
		ac.bootstrappedAt = time.Time{}
		ac.modelsCache = nil
	}
	key := ac.sessionKeyLocked()
	ac.mu.Unlock()
	ac.persistMu.Unlock()

	if cookiesChanged && ac.Pool != nil {
		if s := ac.Pool.Existing(key); s != nil {
			s.QueueCookieUpdateWithGen(rec.Cookies, newGen)
		}
	}
	return true
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

func (ac *AccountClient) sessionKeyLocked() string {
	if ac.ID != "" {
		return ac.ID
	}
	if ac.email != "" {
		return ac.email
	}
	return "default"
}

func (ac *AccountClient) sessionKey() string {
	if ac.ID != "" {
		return ac.ID
	}
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	return ac.sessionKeyLocked()
}

func (ac *AccountClient) absorbAndPersistCookies(setCookies []string) bool {
	return ac.absorbAndPersistCookiesWithGen(setCookies, 0, false)
}

func (ac *AccountClient) absorbAndPersistCookiesWithGen(setCookies []string, expectedGen uint64, enforceGen bool) bool {
	changed, _ := ac.absorbAndPersistCookiesWithGenNext(setCookies, expectedGen, enforceGen)
	return changed
}

func (ac *AccountClient) absorbAndPersistCookiesWithGenNext(setCookies []string, expectedGen uint64, enforceGen bool) (bool, uint64) {
	if len(setCookies) == 0 {
		return false, expectedGen
	}
	ac.mu.Lock()
	if ac.persistRetired.Load() || enforceGen && ac.cookieGen.Load() != expectedGen {
		curGen := ac.cookieGen.Load()
		ac.mu.Unlock()
		return false, curGen
	}
	merged, changed := cookies.AbsorbSetCookie(ac.cookieHeader, setCookies)
	if !changed {
		curGen := ac.cookieGen.Load()
		ac.mu.Unlock()
		return false, curGen
	}
	ac.cookieHeader = merged
	newGen := cookieVersion.Add(1)
	ac.cookieGen.Store(newGen)
	key := ac.sessionKeyLocked()
	ac.mu.Unlock()

	if ac.Pool != nil {
		if s := ac.Pool.Existing(key); s != nil {
			s.QueueCookieUpdateWithGen(merged, newGen)
		}
	}
	ac.persistState()
	return true, newGen
}

// RetireCredentialWriter commits a replacement or deletion in the same
// critical section as credential saves. A failed commit leaves the writer live.
func (ac *AccountClient) RetireCredentialWriter(commit func() error) error {
	ac.persistMu.Lock()
	defer ac.persistMu.Unlock()
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if err := commit(); err != nil {
		return err
	}
	ac.persistRetired.Store(true)
	return nil
}

func (ac *AccountClient) persistState() {
	ac.persistStateWithOptions(true)
}

func (ac *AccountClient) persistStateWithOptions(bumpRotatedAt bool) {
	ac.persistMu.Lock()
	defer ac.persistMu.Unlock()
	if ac.persistRetired.Load() {
		return
	}

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
	if bumpRotatedAt {
		ac.rotatedAt = nowISO
	}
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
		ac.unsavedExplicitCookies = false
		if mtime > ac.fileMtimeMs {
			ac.fileMtimeMs = mtime
		}
		ac.mu.Unlock()
	}
}

func headerCaseInsensitive(headers map[string]string, key string) string {
	if len(headers) == 0 {
		return ""
	}
	if v, ok := headers[key]; ok {
		return v
	}
	for k, v := range headers {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func sanitizeRedirectLocation(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		if idx := strings.IndexAny(raw, "?#"); idx >= 0 {
			return raw[:idx]
		}
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

func isSignInRedirect(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		base := raw
		if idx := strings.IndexAny(base, "?#"); idx >= 0 {
			base = base[:idx]
		}
		return strings.Contains(base, "accounts.google.com") || strings.Contains(base, "ServiceLogin")
	}
	host := strings.ToLower(u.Hostname())
	if host == "accounts.google.com" || host == "accounts.youtube.com" || strings.HasSuffix(host, ".accounts.google.com") {
		return true
	}
	pathLower := strings.ToLower(u.Path)
	for _, marker := range []string{
		"servicelogin",
		"/v3/signin",
		"/signin",
		"accountchooser",
		"interactivelogin",
		"checkcookie",
	} {
		if strings.Contains(pathLower, marker) {
			return true
		}
	}
	return false
}

func classifyUpstreamAuthOrBan(status int, headers map[string]string, body string, defaultCode string) *transport.UpstreamError {
	if status == 403 && (strings.Contains(body, "TOS_VIOLATION") || strings.Contains(body, "PERMISSION_DENIED")) {
		return transport.NewUpstreamError("account banned or restricted upstream (TOS_VIOLATION / PERMISSION_DENIED)", 403, "account_banned", headers, nil)
	}
	var safeLoc string
	if status == 302 || status == 301 || status == 303 || status == 307 || status == 308 {
		loc := headerCaseInsensitive(headers, "location")
		safeLoc = sanitizeRedirectLocation(loc)
		if isSignInRedirect(loc) {
			msg := "upstream redirected to Google sign-in"
			if safeLoc != "" {
				msg += " (" + safeLoc + ")"
			}
			return transport.NewUpstreamError(msg, 401, "auth_redirect", headers, nil)
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
	msg := fmt.Sprintf("upstream HTTP %d", status)
	if safeLoc != "" {
		msg += " (redirect to " + safeLoc + ")"
	}
	return transport.NewUpstreamError(msg, outStatus, code, headers, nil)
}

func (ac *AccountClient) acquireSession(ctx context.Context) (*transport.Session, error) {
	ac.ReloadFromDiskIfNewer()
	ac.mu.RLock()
	if ac.persistRetired.Load() {
		ac.mu.RUnlock()
		return nil, transport.NewUpstreamError("account credentials changed; retry the request", 403, "account_disabled", nil, nil)
	}
	proxy := ac.proxyURL
	cookie := ac.cookieHeader
	gen := ac.cookieGen.Load()
	key := ac.sessionKeyLocked()
	ac.mu.RUnlock()
	return ac.Pool.AcquireWithProxyWithGen(ctx, key, cookie, proxy, gen)
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

const maxBootstrapRedirects = 4

func isSafeBootstrapRedirect(curURL string, res *transport.Response, authUser int) (string, bool) {
	if res == nil || (res.Status != 301 && res.Status != 302 && res.Status != 303 && res.Status != 307 && res.Status != 308) {
		return "", false
	}
	loc := strings.TrimSpace(headerCaseInsensitive(res.Headers, "location"))
	if loc == "" || isSignInRedirect(loc) {
		return "", false
	}
	baseU, err := url.Parse(curURL)
	if err != nil {
		return "", false
	}
	locU, err := url.Parse(loc)
	if err != nil {
		return "", false
	}
	resolved := baseU.ResolveReference(locU)
	if !strings.EqualFold(resolved.Scheme, "https") || !strings.EqualFold(resolved.Hostname(), "gemini.google.com") {
		return "", false
	}
	if p := resolved.Port(); p != "" && p != "443" {
		return "", false
	}
	if authUser > 0 {
		prefix := "/u/" + strconv.Itoa(authUser) + "/"
		exact := "/u/" + strconv.Itoa(authUser)
		if !strings.HasPrefix(resolved.Path, prefix) && resolved.Path != exact {
			return "", false
		}
	}
	return resolved.String(), true
}

func (ac *AccountClient) bootstrapOnClient(ctx context.Context, sc *transport.StealthClient) error {
	doFetchURL := func(target string, au int, chainGen uint64) (*transport.Response, uint64, error) {
		curURL := target
		var res *transport.Response
		for hop := 0; ; hop++ {
			ac.mu.RLock()
			cookie := ac.cookieHeader
			if hop > 0 && ac.cookieGen.Load() != chainGen {
				ac.mu.RUnlock()
				return res, chainGen, nil
			}
			ac.mu.RUnlock()
			hopRes, err := sc.Do(ctx, transport.RequestOptions{
				Method:  "GET",
				URL:     curURL,
				Kind:    transport.ReqKindNavigate,
				Headers: map[string]string{"cookie": cookie},
				Timeout: 30 * time.Second,
			})
			if err != nil {
				return nil, chainGen, err
			}
			res = hopRes
			absorbed, postGen := ac.absorbAndPersistCookiesWithGenNext(res.SetCookies, chainGen, true)
			if absorbed {
				chainGen = postGen
			}
			if nextURL, ok := isSafeBootstrapRedirect(curURL, res, au); ok && hop < maxBootstrapRedirects {
				if ac.cookieGen.Load() != chainGen {
					return res, chainGen, nil
				}
				curURL = nextURL
				continue
			}
			return res, chainGen, nil
		}
	}
	doFetchApp := func() (*transport.Response, uint64, error) {
		ac.mu.RLock()
		authUser := ac.authUser
		chainGen := ac.cookieGen.Load()
		ac.mu.RUnlock()
		appURL := Origin + "/app"
		if authUser > 0 {
			appURL = Origin + "/u/" + strconv.Itoa(authUser) + "/app"
		}
		return doFetchURL(appURL, authUser, chainGen)
	}

	for attempt := 0; attempt < 2; attempt++ {
		res, bootFetchGen, err := doFetchApp()
		if err != nil {
			return err
		}
		if res.Status == 429 {
			return classifyUpstreamAuthOrBan(res.Status, res.Headers, res.Body, "upstream_http_error")
		}

		at := ""
		if res.Status == 200 {
			at = matchFirst(snlm0eRe, res.Body)
		}
		// A refresh can renew rotating cookies, but bootstrap must retry only the
		// configured account slot.
		if res.Status != 200 || at == "" {
			if ac.cookieGen.Load() != bootFetchGen {
				if attempt == 0 {
					continue
				}
				ac.mu.RLock()
				hasValidAT := ac.at != "" && ac.fromDiskAT
				ac.mu.RUnlock()
				if hasValidAT {
					return nil
				}
				return transport.NewUpstreamError("session cookies changed during bootstrap; please retry", 503, "upstream_transient", nil, nil)
			}
			bootStartGen := ac.cookieGen.Load()
			if rotRes, rotErr := ac.rotateOnClient(ctx, sc); rotErr == nil && rotRes != nil {
				if rotRes.Status == 429 {
					var headers map[string]string
					if rotRes.RetryAfter != "" {
						headers = map[string]string{"retry-after": rotRes.RetryAfter}
					}
					return transport.NewUpstreamError("cookie rotation rate limited (HTTP 429)", 429, "rate_limit_error", headers, nil)
				}
				if rotRes.Rotated || ac.cookieGen.Load() != bootStartGen {
					retryRes, retryGen, retryErr := doFetchApp()
					if retryErr != nil {
						return retryErr
					}
					res = retryRes
					bootFetchGen = retryGen
					if res.Status == 200 {
						at = matchFirst(snlm0eRe, res.Body)
					}
					if (res.Status != 200 || at == "") && ac.cookieGen.Load() != bootFetchGen {
						if attempt == 0 {
							continue
						}
						ac.mu.RLock()
						hasValidAT := ac.at != "" && ac.fromDiskAT
						ac.mu.RUnlock()
						if hasValidAT {
							return nil
						}
						return transport.NewUpstreamError("session cookies changed during bootstrap; please retry", 503, "upstream_transient", nil, nil)
					}
				}
			}
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
			ac.mu.RLock()
			curCookie := ac.cookieHeader
			ac.mu.RUnlock()
			parsed := cookies.ParseHeader(curCookie)
			hint := ""
			switch {
			case parsed["__Secure-1PSID"] == "" && parsed["HSID"] == "":
				hint = " (missing HttpOnly session cookies like __Secure-1PSID/HSID/OSID — do not use document.cookie from F12 Console; copy the Cookie header from F12 Network tab or export via Cookie-Editor on https://gemini.google.com/app)"
			case parsed["OSID"] == "" && parsed["__Secure-OSID"] == "":
				hint = " (missing gemini.google.com host cookie OSID / __Secure-OSID — export cookies while on https://gemini.google.com/app, not google.com or accounts.google.com)"
			case parsed["__Secure-1PSIDTS"] == "":
				hint = " (missing __Secure-1PSIDTS cookie — export all cookies from https://gemini.google.com/app or use gsb --login <name>)"
			}
			return transport.NewUpstreamError(
				"could not extract SNlM0e (at token); the saved session is expired or incomplete — run `gsb --login <name>` to sign in again, then re-import (browser cookie exports copied without signing in usually expire immediately)"+hint,
				401,
				"upstream_auth_error",
				res.Headers,
				nil,
			)
		}
		nowISO := time.Now().UTC().Format(time.RFC3339)
		ac.mu.Lock()
		if ac.cookieGen.Load() != bootFetchGen {
			hasValidAT := ac.at != "" && ac.fromDiskAT
			ac.mu.Unlock()
			if attempt == 0 {
				continue
			}
			if hasValidAT {
				return nil
			}
			return transport.NewUpstreamError("session cookies changed during bootstrap; please retry", 503, "upstream_transient", nil, nil)
		}
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

// NeedsXSRFPrewarm reports whether the account has a live session whose SNlM0e
// token is approaching xsrfTTL (or was loaded from disk and never verified in
// this process) and should be refreshed proactively in the background.
func (ac *AccountClient) NeedsXSRFPrewarm(maxAge time.Duration) bool {
	if maxAge <= 0 {
		maxAge = xsrfPrewarmAge
	}
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	if ac.persistRetired.Load() || strings.TrimSpace(ac.cookieHeader) == "" {
		return false
	}
	if !ac.bootstrappedAt.IsZero() {
		return time.Since(ac.bootstrappedAt) >= maxAge
	}
	return ac.fromDiskAT && ac.at != ""
}

// PrewarmXSRF refreshes or verifies the account's SNlM0e token off the hot path.
func (ac *AccountClient) PrewarmXSRF(ctx context.Context) error {
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return err
	}
	ac.bootMu.Lock()
	defer ac.bootMu.Unlock()
	ac.mu.RLock()
	at := ac.at
	bl := ac.bl
	fSid := ac.fSid
	fromDisk := ac.fromDiskAT
	bootAt := ac.bootstrappedAt
	ac.mu.RUnlock()
	if at != "" && !fromDisk && !bootAt.IsZero() && time.Since(bootAt) < xsrfPrewarmAge {
		return nil
	}
	if fromDisk && at != "" && bl != "" && fSid != "" {
		if res, err := ac.doBatchExec(ctx, sess.Client, "otAQ7b", "[]"); err == nil && res != nil && res.Status == 200 {
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
	return ac.bootstrapOnClient(ctx, sess.Client)
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
	ac.mu.RLock()
	startCookie := ac.cookieHeader
	startReloadGen := ac.reloadGen.Load()
	key := ac.sessionKeyLocked()
	ac.mu.RUnlock()

	res, err := ac.Rotator.RotateAccount(ctx, key, startCookie, sc)
	if err != nil {
		return nil, err
	}
	if res.Rotated && (len(res.SetCookies) > 0 || res.CookieHeader != "") {
		ac.mu.Lock()
		if ac.persistRetired.Load() || ac.reloadGen.Load() != startReloadGen {
			curCookie := ac.cookieHeader
			ac.mu.Unlock()
			return &cookies.RotateResult{
				Status:       res.Status,
				Rotated:      false,
				CookieHeader: curCookie,
				NextInterval: res.NextInterval,
				RetryAfter:   res.RetryAfter,
			}, nil
		}
		var merged string
		var changed bool
		if len(res.SetCookies) > 0 {
			merged, changed = cookies.AbsorbSetCookie(ac.cookieHeader, res.SetCookies)
		} else if res.CookieHeader != "" && res.CookieHeader != ac.cookieHeader {
			merged, changed = res.CookieHeader, true
		}
		if !changed {
			curCookie := ac.cookieHeader
			ac.mu.Unlock()
			return &cookies.RotateResult{
				Status:       res.Status,
				Rotated:      false,
				CookieHeader: curCookie,
				NextInterval: res.NextInterval,
				RetryAfter:   res.RetryAfter,
			}, nil
		}
		ac.cookieHeader = merged
		newGen := cookieVersion.Add(1)
		ac.cookieGen.Store(newGen)
		ac.mu.Unlock()
		if ac.Pool != nil {
			if s := ac.Pool.Existing(key); s != nil {
				s.QueueCookieUpdateWithGen(merged, newGen)
			}
		}
		ac.persistState()
		return &cookies.RotateResult{
			Status:       res.Status,
			Rotated:      true,
			CookieHeader: merged,
			SetCookies:   res.SetCookies,
			NextInterval: res.NextInterval,
			RetryAfter:   res.RetryAfter,
		}, nil
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
	startGen := ac.cookieGen.Load()
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
	ac.absorbAndPersistCookiesWithGen(res.SetCookies, startGen, true)
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

// registryModelNames derives the published id and aliases for one registry
// entry.
//
// The catalogue keeps the short tier name in the category ("Carbon") while the
// description carries the versioned product name ("4 Carbon"). Clients and
// routers resolve capabilities from the model id, so the versioned spelling is
// the one published as the id: a bare "gemini-carbon" matches no versioned
// capability row downstream and silently loses its reasoning flags and 1M
// window. The short spelling stays as an alias so pinned configurations and the
// UI keep resolving.
func registryModelNames(modelID, category, displayName, description string) (string, []string) {
	catSlug := whitespaceRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(category)), "-")
	modelName := "gemini-" + modelID
	if catSlug != "" {
		modelName = "gemini-" + catSlug
	}
	aliases := make([]string, 0, 2)
	for _, key := range DocumentedModelKeys(ModelInfo{Description: description, DisplayName: displayName, ModelName: modelName}) {
		if key == "" || key == "gemini-"+modelID {
			continue
		}
		if key != modelName {
			aliases = append(aliases, modelName)
			modelName = key
		}
		break
	}
	dispSlug := whitespaceRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(displayName)), "-")
	if dispSlug != "" {
		if alias := "gemini-" + dispSlug; alias != modelName && !slices.Contains(aliases, alias) {
			aliases = append(aliases, alias)
		}
	}
	return modelName, aliases
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
		if res.Status != 200 {
			return nil, classifyUpstreamAuthOrBan(res.Status, res.Headers, res.Body, "model_discovery_error")
		}
		p = findBatchPayload(entries, "otAQ7b")
	}

	if code := accountStatusCode(p); code != 0 && code != 1000 {
		if code == 1014 {
			// A temporary access restriction, not a verdict on the account. It is
			// reported as a retryable degradation so the pool fails over now and
			// comes back to this account once the restriction lifts.
			return nil, transport.NewUpstreamError(
				"upstream temporarily restricted this account (code 1014): "+accountStatusCodes[code]+" — retrying later usually succeeds",
				503,
				"upstream_degraded",
				res.Headers,
				nil,
			)
		}
		if code == 1016 {
			return nil, transport.NewUpstreamError("session is unauthenticated or the saved cookies have expired (code 1016) — run `gsb --login <name>` to sign in again and re-import the account", 401, "upstream_auth_error", res.Headers, nil)
		}
		reason, known := accountStatusCodes[code]
		if !known {
			reason = "account status is not one this bridge recognizes"
		}
		return nil, transport.NewUpstreamError(
			fmt.Sprintf("upstream rejected this account (code %d): %s — retrying will not help; fix the account on gemini.google.com", code, reason),
			403,
			"account_banned",
			res.Headers,
			nil,
		)
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

		modelName, aliases := registryModelNames(modelID, category, displayName, description)
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
	ac.mu.RLock()
	cookie := ac.cookieHeader
	startGen := ac.cookieGen.Load()
	authUser := ac.authUser
	ac.mu.RUnlock()

	q := url.Values{}
	q.Set("query_signature", graphqlUsageQuerySig)
	q.Set("variables", graphqlUsageVariables)
	if authUser > 0 {
		q.Set("authuser", strconv.Itoa(authUser))
	}
	u := Origin + "/_/BardChatUi/graphql/schemas/GEMINI_WEB_GRAPHQL/executeQuery?" + q.Encode()
	res, err := sc.Do(ctx, transport.RequestOptions{
		Method: "GET",
		URL:    u,
		Kind:   transport.ReqKindCORS,
		Headers: map[string]string{
			"cookie": cookie,
			"accept": "application/json",
		},
		Timeout: 10 * time.Second,
	})
	if err != nil || res == nil || res.Status != 200 {
		return base
	}
	ac.absorbAndPersistCookiesWithGen(res.SetCookies, startGen, true)

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

// accountStatusCodes maps the numeric status GetUserStatus returns to a human
// explanation. Only a handful were recognized before, so an account restricted
// for a terms-of-service or regional reason surfaced as a generic upstream
// error and the operator went looking in the wrong place. Every code below is
// an account-level verdict that retrying cannot fix.
var accountStatusCodes = map[int]string{
	1014: "access is temporarily restricted (possibly regional or a transient session problem)",
	1016: "session is unauthenticated or the saved cookies have expired",
	1021: "account access is rejected; check the account's Google settings",
	1033: "account did not pass Google's safety or trust checks for some features",
	1040: "the latest Terms of Service must be accepted on this account",
	1042: "the account's accepted Terms of Service are out of date",
	1054: "access is blocked by a parent or guardian",
	1057: "access requires parent or guardian approval",
	1060: "Gemini is not available in this account's country or region",
}

// accountStatusCode reads the numeric status field of a GetUserStatus payload.
// A nil or 1000 status means the account is authorized.
func accountStatusCode(p any) int {
	arr, ok := p.([]any)
	if !ok || len(arr) <= 14 {
		return 0
	}
	if f, ok := arr[14].(float64); ok {
		return int(f)
	}
	return 0
}

func isStatus1016(p any) bool {
	return accountStatusCode(p) == 1016
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
