package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/toolbridge"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

var reSafeID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// UpstreamClient abstracts an account's Gemini Web operations so unit tests can
// inject deterministic frame streams while production uses *gemini.AccountClient.
type UpstreamClient interface {
	Bootstrap(ctx context.Context) error
	AvailableModels(ctx context.Context, force bool) (*gemini.AvailableModelsResult, error)
	GetQuota(ctx context.Context) []gemini.QuotaBucket
	UploadFile(ctx context.Context, data []byte, filename, contentType string) (gemini.UploadedFile, error)
	StreamGenerateLive(ctx context.Context, promptText string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error)
	RotateCookies(ctx context.Context) (*cookies.RotateResult, error)
	ReloadFromDiskIfNewer() bool
	InvalidateAuth()
}

// AccountState holds the server-side state and cached metadata for one Google account.
type AccountState struct {
	mu               sync.RWMutex
	ID               string
	Email            string
	FilePath         string
	CapturedAt       time.Time
	RotatedAt        time.Time
	ImportedFrom     string
	ProfileID        string
	AccountGroup     string
	ProxyURL         string
	Quarantined      bool
	QuarantineReason string
	Disabled         bool
	Surfaces         map[string]string
	NextRotateAt     time.Time
	NextModelRefAt   time.Time
	QuotaBackoffStep int
	QuotaNextAllowAt time.Time
	reqTimestamps    []time.Time
	activeStreams    int
	peakRPM          int
	totalRequests    int
	offloadRequests  int
	subStrikes       map[string]int
	Client           UpstreamClient
	Models           []gemini.ModelInfo
	Capacity         int
	Quota            []gemini.QuotaBucket
	QuotaAt          time.Time
	quotaRefreshing  bool
	ModelsAt         time.Time
	ModelsError      error
	Fails            int
	LastFail         time.Time
	LastError        string
	LastUsed         time.Time
}

// AuthScope represents the caller's resolved token scope.
type AuthScope struct {
	OK        bool
	AccountID string
	Group     string
	Admin     bool
}

// canStreamVerified is the single commit predicate for model output in both
// OpenAI-compatible streaming handlers. Eager mode and disabled downgrade
// rejection are explicit policy overrides; strict mode requires a non-empty,
// non-conflicting served identity that matches the requested model.
func canStreamVerified(eagerStream, rejectDowngrade, identityConflict bool, model gemini.ModelInfo, servedID, servedLabel string) bool {
	return eagerStream || !rejectDowngrade || (!identityConflict && servedID != "" && gemini.IsMatchingServedModel(model, servedID, servedLabel))
}

// shouldAbortForModelRejection reports whether the served identity must end the
// stream immediately, before any of that answer reaches the client.
//
// RejectDowngrade covers a different backend id. RejectLabelMismatch covers the
// narrower case where the id named the requested slot but the label names
// another tier, which is how Google renames a tier in place. Both must stop on
// the first frame that carries the verdict: waiting for the generation to finish
// spends the whole upstream latency (100-200s on the carbon tier) only to reject
// the answer at the end, and on the eager path the answer would already be on
// the wire.
func shouldAbortForModelRejection(cfg config.Config, mediaToolUsed bool, model gemini.ModelInfo, servedID, servedLabel string) bool {
	if mediaToolUsed || servedID == "" && servedLabel == "" {
		return false
	}
	if cfg.RejectDowngrade && !gemini.IsMatchingServedModel(model, servedID, servedLabel) {
		return true
	}
	return cfg.RejectLabelMismatch && gemini.ServedLabelMismatch(model, servedID, servedLabel)
}

type cachedUpload struct {
	File gemini.UploadedFile
	At   time.Time
}

// Server is the browserless Gemini Stealth Bridge HTTP daemon.
type Server struct {
	Cfg     config.Config
	Pool    *transport.Pool
	Rotator *cookies.Rotator

	mu                sync.RWMutex
	accounts          []*AccountState
	tokenToAcct       map[string]string
	tokenToGroup      map[string]string
	sessionCache      map[string][]any
	sessionCacheAt    map[string]time.Time
	sessionOrder      []string
	uploadCache       map[string]cachedUpload
	uploadOrder       []string
	lastHotLoadAt     time.Time
	startedAt         time.Time
	clock             func() time.Time
	cacheHits         atomic.Uint64
	cacheMisses       atomic.Uint64
	cacheFlushMu      sync.Mutex
	cacheFlushPending atomic.Bool
	cacheFlushWG      sync.WaitGroup
	clientFactory     func(a *AccountState) UpstreamClient

	// rotateMu guards the sticky rotation cursor used by the "rotate" strategy.
	rotateMu     sync.Mutex
	rotateCursor string

	adminAuthMu    sync.Mutex
	adminAuthFails map[string][]time.Time

	mediaMu    sync.RWMutex
	mediaItems map[string]*mediaEntry
	mediaOrder []string
}

// New constructs a new Server from cfg.
func New(cfg config.Config) *Server {
	maxSessions := cfg.MaxSessions
	if maxSessions <= 0 {
		maxSessions = 16
	}
	if cfg.TokenEstimator != "" {
		prompt.SetTokenEstimatorMode(cfg.TokenEstimator)
	}
	toolbridge.SetCompactOptions(cfg.ToolSchemaCompact, cfg.ToolDescMaxChars)

	pool := transport.NewPool(transport.PoolOptions{
		ClientConfig: transport.ClientConfig{
			ProfileName:      cfg.ImpersonateProfile,
			ProxyURL:         cfg.ProxyURL,
			UserAgent:        cfg.UserAgent,
			SecCHUA:          cfg.SecCHUA,
			SecCHUAPlatform:  cfg.SecCHUAPlatform,
			AcceptLanguage:   cfg.AcceptLanguage,
			TLSExtOrder:      cfg.TLSExtOrder,
			FollowRedirects:  cfg.FollowRedirects,
			IPv6Mode:         cfg.IPv6Mode,
			DumpRequestDir:   cfg.DumpRequestDir,
			Timeout:          cfg.StreamTimeout,
			FirstByteTimeout: cfg.FirstByteTimeout,
			StallTimeout:     cfg.StallTimeout,
		},
		MaxSessions: maxSessions,
		MaxQueue:    cfg.MaxQueueDepth,
		WaitTimeout: cfg.SessionWaitTimeout,
	})
	rotator := cookies.NewRotator()

	tokMap := make(map[string]string, len(cfg.AccountTokens))
	for k, v := range cfg.AccountTokens {
		tokMap[k] = v
	}

	grpMap := make(map[string]string, len(cfg.GroupTokens))
	for k, v := range cfg.GroupTokens {
		grpMap[k] = v
	}

	s := &Server{
		Cfg:            cfg,
		Pool:           pool,
		Rotator:        rotator,
		tokenToAcct:    tokMap,
		tokenToGroup:   grpMap,
		sessionCache:   make(map[string][]any),
		sessionCacheAt: make(map[string]time.Time),
		uploadCache:    make(map[string]cachedUpload),
		startedAt:      time.Now().UTC(),
		clock:          time.Now,
	}
	s.loadSessionCacheFromDisk()
	return s
}

func (s *Server) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// extractRequestToken extracts the caller's API key from Authorization Bearer,
// x-api-key, x-goog-api-key, or ?key= query parameter.
func extractRequestToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	for _, h := range []string{"x-api-key", "x-goog-api-key"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	if r.URL != nil {
		if v := strings.TrimSpace(r.URL.Query().Get("key")); v != "" {
			return v
		}
	}
	return ""
}

// ResolveAuthScope resolves the caller's API key into an AuthScope.
func (s *Server) ResolveAuthScope(r *http.Request) AuthScope {
	token := extractRequestToken(r)
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-GSB-Admin-Key"))
	}

	s.mu.RLock()
	if s.Cfg.AdminKey != "" && token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.Cfg.AdminKey)) == 1 {
		s.mu.RUnlock()
		return AuthScope{OK: true, Admin: true}
	}
	if acctID, ok := s.tokenToAcct[token]; ok && token != "" {
		s.mu.RUnlock()
		return AuthScope{OK: true, AccountID: acctID}
	}
	if group, ok := s.tokenToGroup[token]; ok && token != "" {
		s.mu.RUnlock()
		return AuthScope{OK: true, Group: group}
	}
	explicitTokens := len(s.Cfg.AccountTokens) > 0
	s.mu.RUnlock()

	defPoolTok := s.defaultPoolToken()
	isPoolKey := (s.Cfg.APIKey == "public" && (token == "" || token == "public" || token == defPoolTok)) ||
		(s.Cfg.APIKey != "public" && token == s.Cfg.APIKey)
	// Hot-load newly added account files on the fly when an unknown account token
	// arrives and explicit token lock is not enabled, rate-limited to once per 2s
	// to avoid disk scans on repeated invalid tokens or pool-key requests.
	if !isPoolKey && token != "" && !explicitTokens && (strings.HasPrefix(token, "gsb-") || reSafeID.MatchString(token)) {
		s.mu.RLock()
		canReload := time.Since(s.lastHotLoadAt) >= 2*time.Second
		s.mu.RUnlock()
		if canReload {
			s.mu.Lock()
			s.lastHotLoadAt = time.Now()
			s.mu.Unlock()
			s.LoadCredentials()
			s.mu.RLock()
			acctID, ok := s.tokenToAcct[token]
			s.mu.RUnlock()
			if ok {
				return AuthScope{OK: true, AccountID: acctID}
			}
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	if isPoolKey {
		if len(s.accounts) == 1 {
			return AuthScope{OK: true, AccountID: s.accounts[0].ID}
		}
		return AuthScope{OK: true, Admin: true}
	}
	return AuthScope{OK: false}
}

func (s *Server) defaultPoolToken() string {
	seed := strings.TrimSpace(s.Cfg.HomeDir)
	if seed == "" {
		seed = "gemini-stealth-bridge"
	}
	sum := sha256.Sum256([]byte("gsb-pool:" + seed))
	return "gsb-pool-" + hex.EncodeToString(sum[:5])
}

func (s *Server) requireAuthScope(w http.ResponseWriter, r *http.Request) (AuthScope, bool) {
	scope := s.ResolveAuthScope(r)
	if !scope.OK {
		sendJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "authentication_error"}}, nil)
		return AuthScope{}, false
	}
	return scope, true
}

func (s *Server) requireAuthAndJSONBody(w http.ResponseWriter, r *http.Request) (AuthScope, map[string]any, bool) {
	scope, ok := s.requireAuthScope(w, r)
	if !ok {
		return AuthScope{}, nil, false
	}
	body, ok := readJSONBody(w, r)
	if !ok {
		return AuthScope{}, nil, false
	}
	return scope, body, true
}

// ServeHTTP implements http.Handler for all bridge endpoints.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	ctx := withRequestBaseURL(r.Context(), r, s.Cfg.Port)

	switch {
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && (strings.HasPrefix(p, "/v1/media/") || strings.HasPrefix(p, "/media/")):
		s.HandleGetMedia(w, r)
		return
	case (p == "/mcp" || p == "/v1/mcp"):
		s.HandleMCP(w, r)
		return
	case s.Cfg.WebUI && r.Method == http.MethodGet && (p == "/" || p == "/ui" || p == "/ui/" || p == "/dashboard"):
		s.HandleWebUI(w, r)
		return

	case r.Method == http.MethodGet && (p == "/health" || p == "/"):
		accts := s.Accounts()
		models := len(s.allModels())
		sendJSON(w, 200, map[string]any{
			"status":    "ok",
			"service":   "gemini-stealth-bridge",
			"transport": "stealth-utls-h2",
			"profile":   s.Cfg.ImpersonateProfile,
			"accounts":  len(accts),
			"models":    models,
			"upstream":  "gemini.google.com",
		}, nil)
		return

	case r.Method == http.MethodGet && p == "/readyz":
		accts := s.Accounts()
		acctReports := make([]map[string]any, 0, len(accts))
		healthyCount := 0
		quarantinedCount := 0
		refInterval := s.Cfg.ModelRefreshEvery
		if refInterval <= 0 {
			refInterval = 30 * time.Minute
		}
		now := s.now()
		for _, a := range accts {
			a.mu.RLock()
			mFresh := !a.ModelsAt.IsZero() && now.Sub(a.ModelsAt) < 2*refInterval
			qFresh := !a.QuotaAt.IsZero() && now.Sub(a.QuotaAt) < 30*time.Minute
			authOK := a.ModelsError == nil && !a.Quarantined
			hasModels := len(a.Models) > 0
			qState := a.Quarantined
			id := a.ID
			a.mu.RUnlock()
			if qState {
				quarantinedCount++
			}
			if authOK && hasModels {
				healthyCount++
			}
			acctReports = append(acctReports, map[string]any{
				"id":           id,
				"models_fresh": mFresh,
				"quota_fresh":  qFresh,
				"auth_ok":      authOK,
				"quarantined":  qState,
			})
		}
		statusStr := "ok"
		httpCode := 200
		if len(accts) == 0 || healthyCount == 0 || quarantinedCount > 0 || healthyCount < len(accts) {
			statusStr = "degraded"
			if len(accts) == 0 || healthyCount == 0 {
				if r.URL.Query().Get("strict") == "1" {
					httpCode = 503
				}
			}
		}
		sendJSON(w, httpCode, map[string]any{
			"status":      statusStr,
			"healthy":     healthyCount,
			"quarantined": quarantinedCount,
			"total":       len(accts),
			"accounts":    acctReports,
		}, nil)
		return

	case r.Method == http.MethodGet && (p == "/v1/sessions" || p == "/sessions"):
		if _, ok := s.requireAuthScope(w, r); !ok {
			return
		}
		sendJSON(w, 200, map[string]any{
			"transport":     "stealth-utls-h2",
			"profile":       s.Cfg.ImpersonateProfile,
			"sessions":      s.Pool.Status(),
			"cache_entries": s.sessionCacheLen(),
			"cache_hits":    s.cacheHits.Load(),
			"cache_misses":  s.cacheMisses.Load(),
		}, nil)
		return

	case r.Method == http.MethodGet && (p == "/v1/accounts" || p == "/accounts"):
		scope, ok := s.requireAuthScope(w, r)
		if !ok {
			return
		}
		liveReq := r.URL.Query().Get("live") == "1" || r.URL.Query().Get("fresh") == "1"
		s.EnsureModels(ctx, liveReq)
		pool := s.scopedAccounts(scope)
		list := make([]map[string]any, 0, len(pool))
		now := s.now()
		for _, a := range pool {
			list = append(list, s.accountStatusMap(a, now))
		}
		sendJSON(w, 200, map[string]any{"accounts": list}, nil)
		return
	case r.Method == http.MethodGet && (p == "/v1/quota" || p == "/quota"):
		scope, ok := s.requireAuthScope(w, r)
		if !ok {
			return
		}
		freshReq := r.URL.Query().Get("fresh") == "1"
		pool := s.scopedAccounts(scope)
		allCold := true
		for _, a := range pool {
			a.mu.RLock()
			if !a.QuotaAt.IsZero() {
				allCold = false
			}
			a.mu.RUnlock()
		}
		if freshReq || allCold {
			s.EnsureModels(ctx, false)
			var wg sync.WaitGroup
			for _, a := range pool {
				wg.Add(1)
				go func(acct *AccountState) {
					defer wg.Done()
					q := s.clientFor(acct).GetQuota(ctx)
					if len(q) > 0 {
						s.mergeAccountQuota(acct, q)
					}
				}(a)
			}
			wg.Wait()
		} else {
			for _, a := range pool {
				a.mu.RLock()
				stale := time.Since(a.QuotaAt) > quotaRefreshCooldown
				a.mu.RUnlock()
				if stale {
					s.refreshQuotaAsync(a, s.clientFor(a))
				}
			}
		}
		list := make([]map[string]any, 0, len(pool))
		for _, a := range pool {
			a.mu.RLock()
			var qAtStr string
			if !a.QuotaAt.IsZero() {
				qAtStr = a.QuotaAt.UTC().Format(time.RFC3339)
			}
			list = append(list, map[string]any{
				"id":      a.ID,
				"email":   a.Email,
				"quota":   a.Quota,
				"quotaAt": qAtStr,
			})
			a.mu.RUnlock()
		}
		sendJSON(w, 200, map[string]any{"accounts": list}, nil)
		return

	case r.Method == http.MethodGet && (p == "/v1/models" || p == "/models"):
		scope, ok := s.requireAuthScope(w, r)
		if !ok {
			return
		}
		accts := s.Accounts()
		if len(accts) == 0 {
			sendJSON(w, 503, map[string]any{"error": map[string]any{"message": "no accounts configured", "type": "auth_unavailable"}}, nil)
			return
		}
		pool := s.scopedAccounts(scope)
		freshReq := r.URL.Query().Get("fresh") == "1"
		hasWarmModels := false
		needsBgRefresh := false
		refEvery := s.Cfg.ModelRefreshEvery
		if refEvery <= 0 {
			refEvery = 30 * time.Minute
		}
		for _, a := range pool {
			a.mu.RLock()
			if len(a.Models) > 0 {
				hasWarmModels = true
				if !a.ModelsAt.IsZero() && time.Since(a.ModelsAt) >= refEvery {
					needsBgRefresh = true
				}
			}
			a.mu.RUnlock()
		}
		if freshReq {
			s.EnsureModels(ctx, true)
		} else if !hasWarmModels {
			s.EnsureModels(ctx, false)
		} else if needsBgRefresh {
			go func() {
				bgCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				s.EnsureModels(bgCtx, true)
			}()
		}

		allEmpty := len(pool) > 0
		var firstErr error
		for _, a := range pool {
			a.mu.RLock()
			if len(a.Models) > 0 {
				allEmpty = false
			}
			if firstErr == nil && a.ModelsError != nil {
				firstErr = a.ModelsError
			}
			a.mu.RUnlock()
		}
		if allEmpty && firstErr != nil {
			sendChatError(w, false, firstErr)
			return
		}

		seen := make(map[string]bool)
		var models []gemini.ModelInfo
		for _, a := range pool {
			a.mu.RLock()
			for _, m := range a.Models {
				if !seen[m.ModelName] {
					seen[m.ModelName] = true
					models = append(models, m)
				}
			}
			a.mu.RUnlock()
		}
		payload := s.BuildModelsPayload(models)
		rawBytes, _ := json.Marshal(payload)
		sum := sha256.Sum256(rawBytes)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Header().Set("ETag", etag)
		if inm := strings.TrimSpace(r.Header.Get("If-None-Match")); inm != "" && inm == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		sendJSON(w, 200, payload, nil)
		return
	case r.Method == http.MethodPost && (p == "/v1/chat/completions" || p == "/chat/completions"):
		if scope, body, ok := s.requireAuthAndJSONBody(w, r); ok {
			s.HandleChat(ctx, w, body, scope)
		}
		return

	case r.Method == http.MethodPost && (p == "/v1/responses" || p == "/responses"):
		if scope, body, ok := s.requireAuthAndJSONBody(w, r); ok {
			s.HandleResponses(ctx, w, body, scope)
		}
		return

	case r.Method == http.MethodPost && (p == "/v1/images/generations" || p == "/images/generations"):
		if scope, body, ok := s.requireAuthAndJSONBody(w, r); ok {
			s.HandleImageGeneration(ctx, w, body, scope)
		}
		return

	case r.Method == http.MethodPost && (p == "/v1/videos/generations" || p == "/videos/generations"):
		if scope, body, ok := s.requireAuthAndJSONBody(w, r); ok {
			s.HandleVideoGeneration(ctx, w, body, scope)
		}
		return

	case r.Method == http.MethodPost && (p == "/v1/refresh" || p == "/refresh"):
		if r.Header.Get("Origin") != "" {
			sendJSON(w, 403, map[string]any{"error": map[string]any{"message": "origin not allowed", "type": "authentication_error"}}, nil)
			return
		}
		if _, ok := s.requireAuthScope(w, r); !ok {
			return
		}
		s.LoadCredentials()
		accts := s.EnsureModels(ctx, true)
		sendJSON(w, 200, map[string]any{
			"status":   "ok",
			"accounts": len(accts),
			"models":   len(s.allModels()),
		}, nil)
		return

	case s.Cfg.WebUI && strings.HasPrefix(p, "/v1/admin/"):
		s.HandleAdmin(ctx, w, r)
		return

	default:
		sendJSON(w, 404, map[string]any{
			"error": map[string]any{
				"message": "not found: " + p,
				"type":    "invalid_request_error",
			},
		}, nil)
	}
}

func (s *Server) accountStatusMap(a *AccountState, now time.Time) map[string]any {
	rpmCutoff := now.Add(-60 * time.Second)
	a.mu.RLock()
	defer a.mu.RUnlock()
	mNames := make([]string, 0, len(a.Models))
	for _, m := range a.Models {
		mNames = append(mNames, m.ModelName)
	}
	var capAtStr, rotAtStr, modAtStr, qAtStr, lastUsedStr string
	if !a.CapturedAt.IsZero() {
		capAtStr = a.CapturedAt.UTC().Format(time.RFC3339)
	}
	if !a.RotatedAt.IsZero() {
		rotAtStr = a.RotatedAt.UTC().Format(time.RFC3339)
	}
	if !a.ModelsAt.IsZero() {
		modAtStr = a.ModelsAt.UTC().Format(time.RFC3339)
	}
	if !a.QuotaAt.IsZero() {
		qAtStr = a.QuotaAt.UTC().Format(time.RFC3339)
	}
	if !a.LastUsed.IsZero() {
		lastUsedStr = a.LastUsed.UTC().Format(time.RFC3339)
	}
	recentRPM := 0
	for _, ts := range a.reqTimestamps {
		if ts.After(rpmCutoff) {
			recentRPM++
		}
	}
	surfCopy := make(map[string]string, len(a.Surfaces))
	for k, v := range a.Surfaces {
		surfCopy[k] = v
	}
	status, statusReason := AccountStatus(a, now)
	quotaCopy := append([]gemini.QuotaBucket(nil), a.Quota...)
	var lastFailStr string
	if !a.LastFail.IsZero() {
		lastFailStr = a.LastFail.UTC().Format(time.RFC3339)
	}
	lastError := a.LastError
	return map[string]any{
		"id":               a.ID,
		"email":            a.Email,
		"status":           status,
		"statusReason":     statusReason,
		"capacity":         a.Capacity,
		"fails":            a.Fails,
		"lastError":        lastError,
		"lastFailAt":       lastFailStr,
		"models":           mNames,
		"quota":            quotaCopy,
		"quotaWindow":      quotaWindowInfo(quotaCopy, now),
		"capturedAt":       capAtStr,
		"rotatedAt":        rotAtStr,
		"modelsAt":         modAtStr,
		"quotaAt":          qAtStr,
		"lastUsedAt":       lastUsedStr,
		"quarantined":      a.Quarantined,
		"quarantineReason": a.QuarantineReason,
		"disabled":         a.Disabled,
		"accountGroup":     a.AccountGroup,
		"importedFrom":     a.ImportedFrom,
		"profileId":        a.ProfileID,
		"proxyConfigured":  a.ProxyURL != "" || s.Cfg.ProxyURL != "",
		"activeStreams":    a.activeStreams,
		"recentRpm":        recentRPM,
		"peakRpm":          a.peakRPM,
		"totalRequests":    a.totalRequests,
		"offloadRequests":  a.offloadRequests,
		"surfaces":         surfCopy,
	}
}

func readJSONBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	const maxRequestBodyBytes = 32 * 1024 * 1024
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid request body", "type": "invalid_request_error"}}, nil)
		return nil, false
	}
	if len(raw) > maxRequestBodyBytes {
		sendJSON(w, 413, map[string]any{"error": map[string]any{"message": "request body exceeds 32MB limit", "type": "invalid_request_error"}}, nil)
		return nil, false
	}
	var body map[string]any
	if len(raw) == 0 {
		body = map[string]any{}
	} else if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid JSON", "type": "invalid_request_error"}}, nil)
		return nil, false
	}
	return body, true
}

// ThinkingLevelName maps the configured numeric thinking level to the effort name
// advertised in model metadata. An unknown value yields "" so callers fall back to
// the model's own default instead of advertising a level the daemon cannot apply.
func ThinkingLevelName(level int) string {
	for name, enum := range gemini.ThinkingLevels {
		if enum == level {
			return name
		}
	}
	return ""
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// BuildModelsPayload constructs the canonical /v1/models response object with
// OpenAI, OpenRouter, and gsb capability metadata.
func (s *Server) BuildModelsPayload(models []gemini.ModelInfo) map[string]any {
	createdSec := s.startedAt.Unix()
	if createdSec <= 0 {
		createdSec = time.Now().Unix()
	}
	ctxWinFallback := s.Cfg.ContextWindow
	if ctxWinFallback <= 0 {
		ctxWinFallback = 1048576
	}
	maxOutFallback := s.Cfg.MaxOutputTokens
	if maxOutFallback <= 0 {
		maxOutFallback = 65536
	}
	// The configured default is what requests without an explicit effort actually
	// use, so it is what the metadata must advertise. Reporting a hardcoded "high"
	// here would tell clients the opposite of the daemon's behaviour.
	configuredDefault := ThinkingLevelName(s.Cfg.DefaultThinking)

	data := make([]map[string]any, 0, len(models))
	for _, x := range models {
		caps := gemini.ResolveModelCapabilities(x)
		isReasoning := caps.Reasoning
		limits, limitsSource := gemini.ResolveModelLimits(x, ctxWinFallback, maxOutFallback)
		ctxWin := limits.ContextWindow
		maxOut := limits.MaxOutput
		defThinkStr := caps.DefaultEffort
		if isReasoning && configuredDefault != "" && containsString(caps.EffortLevels, configuredDefault) {
			defThinkStr = configuredDefault
		}
		if defThinkStr == "" {
			if isReasoning {
				defThinkStr = "high"
			} else {
				defThinkStr = "low"
			}
		}
		aliases := x.Aliases
		if aliases == nil {
			aliases = []string{}
		}
		var suppParams []string
		efforts := caps.EffortLevels
		if efforts == nil {
			efforts = []string{}
		}
		if isReasoning {
			suppParams = []string{"tools", "tool_choice", "reasoning", "reasoning_effort", "max_tokens", "max_completion_tokens"}
		} else {
			suppParams = []string{"tools", "tool_choice", "max_tokens", "max_completion_tokens"}
		}

		entry := map[string]any{
			"id":                    x.ModelName,
			"object":                "model",
			"created":               createdSec,
			"owned_by":              "google",
			"display_name":          x.DisplayName,
			"description":           x.Description,
			"aliases":               aliases,
			"context_length":        ctxWin,
			"context_window":        ctxWin,
			"max_completion_tokens": maxOut,
			"max_output_tokens":     maxOut,
			"reasoning":             isReasoning,
			"supported_parameters":  suppParams,
			"architecture": map[string]any{
				"modality":          "text+image->text",
				"input_modalities":  []string{"text", "image"},
				"output_modalities": []string{"text"},
			},
			"top_provider": map[string]any{
				"context_length":        ctxWin,
				"max_completion_tokens": maxOut,
				"is_moderated":          false,
			},
			"pricing": map[string]any{
				"prompt":     "0",
				"completion": "0",
				"image":      "0",
				"request":    "0",
			},
			"thinking": map[string]any{
				"mode":           "effort",
				"default":        defThinkStr,
				"defaultLevel":   defThinkStr,
				"efforts":        efforts,
				"requiresEffort": isReasoning,
			},
			"compat": map[string]any{
				"supportsReasoningEffort": isReasoning,
			},
			"capabilities": map[string]any{
				"vision":                  caps.Vision,
				"pdf":                     false,
				"audioInput":              false,
				"videoInput":              false,
				"imageOutput":             false,
				"audioOutput":             false,
				"search":                  caps.Search,
				"tools":                   caps.Tools,
				"reasoning":               isReasoning,
				"thinkingFormat":          "gemini-level",
				"thinkingCanDisable":      false,
				"thinkingEffortSupported": isReasoning,
				"thinkingLevels":          efforts,
				"defaultThinkingLevel":    defThinkStr,
				"contextWindow":           ctxWin,
				"maxOutput":               maxOut,
				// "documented" means the number came from Google's published model
				// page; "config" means it is the operator's configured fallback.
				"limitsSource": limitsSource,
			},
		}
		data = append(data, entry)
	}
	return map[string]any{"object": "list", "data": data}
}

// quotaWindowInfo describes what the quota numbers actually cover, so a caller
// does not mistake the rolling 5-hour window for the account's whole allowance.
//
// Gemini enforces two limits at once: a short rolling window (about 5 hours) and a
// weekly cap. The 5-hour window is not a separate allowance; it is a slice of the
// weekly one, so when the weekly cap is spent the short window cannot be used
// either. The quota RPC gsb consumes only exposes the short window, which is why
// this field exists: it states the scope plainly instead of letting a healthy
// remaining count imply the account is fully available.
func quotaWindowInfo(quota []gemini.QuotaBucket, now time.Time) map[string]any {
	info := map[string]any{
		"scope":             "rolling",
		"label":             "5h",
		"nestedUnderWeekly": true,
		"weeklyExposed":     false,
		"note":              "gsb only receives the rolling ~5h window; the weekly cap is not exposed by the upstream RPC, so a nonzero remaining count does not guarantee the account is usable",
	}
	soonest := time.Time{}
	for _, q := range quota {
		if q.ID == "weekly" && q.Total > 0 {
			info["weeklyExposed"] = true
			info["weeklyRemaining"] = q.Remaining
			info["weeklyTotal"] = q.Total
			if q.ResetAt != "" {
				info["weeklyResetAt"] = q.ResetAt
			}
			info["note"] = "both rolling 5h and 7d weekly credit windows are exposed; the 5h window is nested under the weekly allowance"
		}
		if q.Synthetic || q.Unlimited || q.ResetAt == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, q.ResetAt)
		if err != nil || !t.After(now) {
			continue
		}
		if soonest.IsZero() || t.Before(soonest) {
			soonest = t
		}
	}
	if !soonest.IsZero() {
		info["nextResetAt"] = soonest.UTC().Format(time.RFC3339)
		info["nextResetInSeconds"] = int(soonest.Sub(now).Seconds())
	}
	return info
}

// ScheduleAccountTimers initializes staggered per-account rotation and model refresh deadlines.
func (s *Server) ScheduleAccountTimers(now time.Time) {
	accts := s.Accounts()
	baseRotate := cookies.ClampRotateInterval(0, s.Cfg.RotateCookiesEvery)
	for i, a := range accts {
		a.mu.Lock()
		if a.NextRotateAt.IsZero() {
			a.NextRotateAt = now.Add(jitterDuration(baseRotate/10, baseRotate/2) + time.Duration(i)*2500*time.Millisecond)
		}
		if a.NextModelRefAt.IsZero() {
			a.NextModelRefAt = now.Add(jitterDuration(5*time.Second, 30*time.Second) + time.Duration(i)*2500*time.Millisecond)
		}
		a.mu.Unlock()
	}
	s.decollideRotationTimers()
}

func (s *Server) decollideRotationTimers() {
	accts := s.Accounts()
	for i := range accts {
		for j := i + 1; j < len(accts); j++ {
			accts[i].mu.RLock()
			ti := accts[i].NextRotateAt
			accts[i].mu.RUnlock()
			accts[j].mu.Lock()
			tj := accts[j].NextRotateAt
			diff := tj.Sub(ti)
			if diff < 0 {
				diff = -diff
			}
			if diff < 2*time.Second {
				accts[j].NextRotateAt = ti.Add(2500 * time.Millisecond)
			}
			accts[j].mu.Unlock()
		}
	}
}

// nextRefreshAt computes the next model-refresh deadline for one account. An
// account holding failure evidence is re-checked soon, because that check is
// what clears the evidence; a healthy account keeps the slow cadence. now is
// returned unchanged so callers can thread one clock value through.
func (s *Server) nextRefreshAt(now time.Time, a *AccountState) (time.Time, time.Time) {
	refEvery := s.Cfg.ModelRefreshEvery
	if refEvery <= 0 {
		refEvery = 30 * time.Minute
	}
	interval := refEvery + randDuration(refEvery/5)
	if a != nil {
		a.mu.RLock()
		failing := a.Fails > 0 || !a.LastFail.IsZero()
		a.mu.RUnlock()
		if failing {
			interval = failureRecheckInterval + randDuration(failureRecheckInterval/5)
		}
	}
	return now, now.Add(interval)
}

// failureRecheckInterval is how often an account that recorded a failure is
// re-checked. The recheck doubles as the probe that clears a transient failure
// label, so it must be short enough that the operator never presses Refresh
// manually just to see the account come back.
const failureRecheckInterval = 2 * time.Minute

// RunSchedulerStep evaluates one tick of the per-account background scheduler at now.
func (s *Server) RunSchedulerStep(ctx context.Context, now time.Time) {
	if inQuietHours(now, s.Cfg.QuietHours) {
		return
	}
	accts := s.Accounts()
	refEvery := s.Cfg.ModelRefreshEvery
	if refEvery <= 0 {
		refEvery = 30 * time.Minute
	}
	for _, a := range accts {
		a.mu.RLock()
		quarantined := a.Quarantined
		nextRot := a.NextRotateAt
		nextRef := a.NextModelRefAt
		lastUsed := a.LastUsed
		a.mu.RUnlock()
		if quarantined {
			continue
		}
		if s.Cfg.RotateCookiesEvery > 0 && !nextRot.IsZero() && !now.Before(nextRot) {
			busy := false
			if s.Pool != nil {
				if sess := s.Pool.Existing(a.ID); sess != nil && sess.Busy() {
					busy = true
				}
			}
			if !busy {
				c := s.clientFor(a)
				rCtx, rCancel := context.WithTimeout(ctx, 15*time.Second)
				res, err := c.RotateCookies(rCtx)
				rCancel()
				base := cookies.ClampRotateInterval(0, s.Cfg.RotateCookiesEvery)
				if res != nil && res.NextInterval > 0 {
					base = cookies.ClampRotateInterval(res.NextInterval, s.Cfg.RotateCookiesEvery)
				}
				if lastUsed.IsZero() || now.Sub(lastUsed) > 2*base {
					base = cookies.ClampRotateInterval(base*2, s.Cfg.RotateCookiesEvery)
				}
				nextTime := now.Add(base + randDuration(base/3))
				a.mu.Lock()
				if res != nil && res.Rotated {
					a.RotatedAt = now
					// A rotation that produced fresh cookies proves the session is
					// still renewable, so an expiry verdict recorded earlier is stale.
					if looksLikeAuthFailure(a.LastError) {
						a.LastError = ""
						a.LastFail = time.Time{}
						a.Fails = 0
					}
				}
				a.NextRotateAt = nextTime
				a.mu.Unlock()
				// /RotateCookies rejecting the cookie chain means the session cannot
				// be renewed at all: it is expired, or bound to a browser key gsb
				// does not hold. Record it now so the account reports `expired`
				// instead of dying silently hours later on the next request.
				if res != nil && (res.Status == 401 || res.Status == 403) {
					a.mu.Lock()
					a.LastError = "session expired; run `gsb --login " + a.ID + "` to sign in again"
					a.LastFail = now
					a.mu.Unlock()
				}
				s.decollideRotationTimers()
				if err != nil {
					log.Printf("[cookies] %s rotate error: %v", a.ID, err)
				} else if res != nil && res.Rotated {
					log.Printf("[cookies] %s rotated session cookies (HTTP %d)", a.ID, res.Status)
				} else if res != nil && (res.Status == 401 || res.Status == 403) {
					log.Printf("[cookies] %s rotation rejected (HTTP %d): session cannot be renewed, re-login required", a.ID, res.Status)
				}
			}
		}
		if !nextRef.IsZero() && !now.Before(nextRef) {
			rCtx, rCancel := context.WithTimeout(ctx, 45*time.Second)
			s.RefreshAccount(rCtx, a, true)
			rCancel()
			_, next := s.nextRefreshAt(now, a)
			a.mu.Lock()
			a.NextModelRefAt = next
			a.mu.Unlock()
		}
	}
}

// StartBackgroundWorkers starts per-account jittered model warming, dual-path cookie rotation,
// and idle session reaping until ctx is cancelled.
func (s *Server) StartBackgroundWorkers(ctx context.Context) {
	s.ScheduleAccountTimers(s.now())

	go func() {
		wCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s.EnsureModels(wCtx, false)
		cancel()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.RunSchedulerStep(ctx, s.now())
			}
		}
	}()

	// Reap idle transport sessions every 60 seconds
	if s.Cfg.IdleSessionTimeout > 0 {
		go func() {
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.Pool.ReapIdle(s.Cfg.IdleSessionTimeout)
				}
			}
		}()
	}
}

func jitterDuration(minDur, maxDur time.Duration) time.Duration {
	if maxDur <= minDur {
		return minDur
	}
	return minDur + randDuration(maxDur-minDur)
}

func randDuration(maxDur time.Duration) time.Duration {
	if maxDur <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(maxDur)))
}

func inQuietHours(now time.Time, spec string) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return false
	}
	parts := strings.Split(spec, "-")
	if len(parts) != 2 {
		return false
	}
	startMin, ok1 := parseHHMM(parts[0])
	endMin, ok2 := parseHHMM(parts[1])
	if !ok1 || !ok2 {
		return false
	}
	curMin := now.Hour()*60 + now.Minute()
	if startMin <= endMin {
		return curMin >= startMin && curMin < endMin
	}
	return curMin >= startMin || curMin < endMin
}

func parseHHMM(s string) (int, bool) {
	hm := strings.Split(strings.TrimSpace(s), ":")
	if len(hm) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(hm[0])
	m, err2 := strconv.Atoi(hm[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

func (s *Server) logRequestJSON(entry map[string]any) {
	if !s.Cfg.LogRequests || entry == nil {
		return
	}
	if _, ok := entry["ts"]; !ok {
		entry["ts"] = s.now().UTC().Format(time.RFC3339Nano)
	}
	if b, err := json.Marshal(entry); err == nil {
		log.Printf("%s", string(b))
	}
}

func setGSBHeaders(w http.ResponseWriter, headersSent bool, acctID, modelName string, attempt, upstreamStatus int, retryReason string) {
	if headersSent || w == nil {
		return
	}
	if acctID != "" {
		w.Header().Set("X-GSB-Account", acctID)
	}
	if modelName != "" {
		w.Header().Set("X-GSB-Model", modelName)
	}
	if attempt >= 0 {
		w.Header().Set("X-GSB-Attempt", strconv.Itoa(attempt+1))
	}
	if upstreamStatus > 0 {
		w.Header().Set("X-GSB-Upstream-Status", strconv.Itoa(upstreamStatus))
	}
	if retryReason != "" {
		w.Header().Set("X-GSB-Retry-Reason", retryReason)
	}
}

func retryHeaders(hdrs map[string]string) map[string]string {
	if len(hdrs) == 0 {
		return nil
	}
	out := make(map[string]string)
	for _, k := range []string{"retry-after", "x-resets-at", "x-gsb-retry-reason"} {
		val := hdrs[k]
		if val == "" {
			switch k {
			case "retry-after":
				val = hdrs["Retry-After"]
			case "x-resets-at":
				val = hdrs["X-Resets-At"]
			case "x-gsb-retry-reason":
				val = hdrs["X-GSB-Retry-Reason"]
			}
		}
		if val != "" && !strings.ContainsAny(val, "\r\n") {
			if k == "x-gsb-retry-reason" {
				out["X-GSB-Retry-Reason"] = val
			} else {
				out[k] = val
			}
		}
	}
	return out
}

func sendJSON(w http.ResponseWriter, status int, obj any, extraHeaders map[string]string) {
	b, _ := json.Marshal(obj)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	for k, v := range extraHeaders {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func sendChatError(w http.ResponseWriter, headersSent bool, err error) {
	status := 502
	code := ""
	var hdrs map[string]string
	var ue *transport.UpstreamError
	if errors.As(err, &ue) && ue != nil {
		if ue.StatusCode >= 400 && ue.StatusCode <= 599 {
			status = ue.StatusCode
		}
		code = ue.Code
		hdrs = ue.Headers
	}
	errType := "upstream_error"
	switch status {
	case 429:
		errType = "rate_limit_error"
	case 401, 403:
		errType = "authentication_error"
	case 400:
		errType = "invalid_request_error"
	}
	if code == "" {
		code = errType
	}
	payload := map[string]any{
		"error": map[string]any{
			"message": err.Error(),
			"type":    errType,
			"code":    code,
		},
	}
	if headersSent {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", string(b))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	} else {
		sendJSON(w, status, payload, retryHeaders(hdrs))
	}
}
