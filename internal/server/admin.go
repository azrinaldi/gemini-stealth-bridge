package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/webui"
)

const (
	adminRateLimitMaxFails = 5
	adminRateLimitWindow   = 60 * time.Second
)

func clientRemoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// isDirectLoopbackRequest reports true only when the socket peer is a loopback
// address AND no reverse-proxy forwarding header indicates an external client.
func isDirectLoopbackRequest(r *http.Request) bool {
	host := clientRemoteHost(r)
	if host != "" && !config.IsLoopbackHost(host) {
		return false
	}
	for _, hdr := range []string{"X-Forwarded-For", "X-Real-IP", "CF-Connecting-IP", "Forwarded"} {
		val := strings.TrimSpace(r.Header.Get(hdr))
		if val == "" {
			continue
		}
		first := strings.TrimSpace(strings.Split(val, ",")[0])
		if strings.HasPrefix(strings.ToLower(first), "for=") {
			first = strings.Trim(strings.TrimPrefix(first[4:], ""), "\"[]")
		}
		if !config.IsLoopbackHost(first) {
			return false
		}
	}
	return true
}

func (s *Server) requiredAdminKey() (key string, mode string) {
	s.mu.RLock()
	adminKey := strings.TrimSpace(s.Cfg.AdminKey)
	apiKey := strings.TrimSpace(s.Cfg.APIKey)
	s.mu.RUnlock()
	if adminKey != "" {
		return adminKey, "admin_key"
	}
	if apiKey != "" && apiKey != "public" {
		return apiKey, "api_key"
	}
	return "", "local_public"
}

func extractAdminToken(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-GSB-Admin-Key")); v != "" {
		return v
	}
	return extractRequestToken(r)
}

func (s *Server) isAdminRateLimited(ip string, now time.Time) bool {
	s.adminAuthMu.Lock()
	defer s.adminAuthMu.Unlock()
	if s.adminAuthFails == nil {
		return false
	}
	cutoff := now.Add(-adminRateLimitWindow)
	list := s.adminAuthFails[ip]
	kept := list[:0]
	for _, ts := range list {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	if len(kept) == 0 {
		delete(s.adminAuthFails, ip)
		return false
	}
	s.adminAuthFails[ip] = kept
	return len(kept) >= adminRateLimitMaxFails
}

func (s *Server) recordAdminAuthFailure(ip string, now time.Time) {
	s.adminAuthMu.Lock()
	defer s.adminAuthMu.Unlock()
	if s.adminAuthFails == nil {
		s.adminAuthFails = make(map[string][]time.Time)
	}
	cutoff := now.Add(-adminRateLimitWindow)
	list := s.adminAuthFails[ip]
	kept := list[:0]
	for _, ts := range list {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	s.adminAuthFails[ip] = append(kept, now)
}

func (s *Server) clearAdminAuthFailures(ip string) {
	s.adminAuthMu.Lock()
	defer s.adminAuthMu.Unlock()
	if s.adminAuthFails != nil {
		delete(s.adminAuthFails, ip)
	}
}

// authorizeAdminAPI enforces the VPS security model on /v1/admin/* routes.
func (s *Server) authorizeAdminAPI(w http.ResponseWriter, r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		sendJSON(w, http.StatusForbidden, map[string]any{
			"error": map[string]any{
				"message": "cross-site admin requests are forbidden",
				"type":    "AuthorizationError",
				"code":    "cross_site_forbidden",
			},
		}, nil)
		return false
	}

	reqKey, _ := s.requiredAdminKey()
	loopback := isDirectLoopbackRequest(r)
	if !loopback && reqKey == "" {
		sendJSON(w, http.StatusForbidden, map[string]any{
			"error": map[string]any{
				"message": "remote access to the web control plane is locked while BRIDGE_API_KEY is 'public'; set BRIDGE_ADMIN_KEY or a non-public BRIDGE_API_KEY, or connect via SSH port forwarding (ssh -L 20133:127.0.0.1:20133)",
				"type":    "AuthorizationError",
				"code":    "remote_web_ui_locked",
			},
		}, nil)
		return false
	}

	if reqKey == "" {
		return true
	}

	ip := clientRemoteHost(r)
	now := s.now()
	if s.isAdminRateLimited(ip, now) {
		sendJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{
				"message": "too many failed authentication attempts; wait 60 seconds before retrying",
				"type":    "RateLimitError",
				"code":    "admin_auth_rate_limited",
			},
		}, nil)
		return false
	}

	token := extractAdminToken(r)
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(reqKey)) != 1 {
		if token != "" {
			s.recordAdminAuthFailure(ip, now)
		}
		sendJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{
				"message": "admin authentication required; supply BRIDGE_ADMIN_KEY or BRIDGE_API_KEY",
				"type":    "authentication_error",
				"code":    "admin_auth_required",
			},
		}, nil)
		return false
	}

	s.clearAdminAuthFailures(ip)
	return true
}

// HandleWebUI serves GET /ui when Cfg.WebUI is enabled.
func (s *Server) HandleWebUI(w http.ResponseWriter, r *http.Request) {
	reqKey, _ := s.requiredAdminKey()
	if !isDirectLoopbackRequest(r) && reqKey == "" {
		sendJSON(w, http.StatusForbidden, map[string]any{
			"error": map[string]any{
				"message": "remote access to /ui is locked while BRIDGE_API_KEY is 'public'; set BRIDGE_ADMIN_KEY or a non-public BRIDGE_API_KEY, or use SSH port forwarding (ssh -L 20133:127.0.0.1:20133)",
				"type":    "AuthorizationError",
				"code":    "remote_web_ui_locked",
			},
		}, nil)
		return
	}

	page, ok := webui.PageHTML()
	if !ok {
		sendJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{
				"message": "web control plane was excluded at compile time (-tags nowebui)",
				"type":    "invalid_request_error",
				"code":    "webui_compiled_out",
			},
		}, nil)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self' http://127.0.0.1:20134 http://localhost:20134; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

func (s *Server) buildAdminConfigPayload() map[string]any {
	_, authMode := s.requiredAdminKey()
	s.rotateMu.Lock()
	rotCursor := s.rotateCursor
	s.rotateMu.Unlock()
	if rotCursor == "" && s.rotate() {
		if accts := s.Accounts(); len(accts) > 0 {
			rotCursor = accts[0].ID
		}
	}

	s.mu.RLock()
	cfg := s.Cfg
	sources := make(map[string]string, len(cfg.FieldSources))
	for k, v := range cfg.FieldSources {
		sources[k] = v
	}
	prio := append([]string(nil), cfg.AccountPriority...)
	s.mu.RUnlock()

	defThink := ThinkingLevelName(cfg.DefaultThinking)
	if defThink == "" {
		defThink = "low"
	}
	uptimeSec := int64(s.now().Sub(s.startedAt).Seconds())
	if uptimeSec < 0 {
		uptimeSec = 0
	}

	return map[string]any{
		"version":      config.Version,
		"uptimeSec":    uptimeSec,
		"host":         cfg.Host,
		"port":         cfg.Port,
		"homeDir":      cfg.HomeDir,
		"accountsDir":  cfg.AccountsDir,
		"envFile":      cfg.EnvFile,
		"authMode":     authMode,
		"poolKey":      s.displayPoolKey(cfg.APIKey),
		"rotateCursor": rotCursor,
		// The live model ids, so the Web UI can show the connection copy chips
		// from the same registry the API serves. A hardcoded list there went
		// stale the moment upstream renamed a model.
		"models": modelNames(s.allModels()),
		"metrics": map[string]any{
			"cacheEntries":   s.sessionCacheLen(),
			"cacheHits":      s.cacheHits.Load(),
			"cacheMisses":    s.cacheMisses.Load(),
			"activeSessions": len(s.Pool.Status()),
		},
		"config": map[string]any{
			"defaultThinking":         defThink,
			"accountStrategy":         cfg.AccountStrategy,
			"accountPriority":         prio,
			"rejectDowngrade":         cfg.RejectDowngrade,
			"rejectLabelMismatch":     cfg.RejectLabelMismatch,
			"quotaPreflight":          cfg.QuotaPreflight,
			"maxRpmPerAccount":        cfg.MaxRPMPerAccount,
			"maxConcurrentPerAccount": cfg.MaxConcurrentPerAccount,
			"liveThoughts":            cfg.LiveThoughts,
			"liveToolCalls":           cfg.LiveToolCalls,
			"keepToolPreamble":        cfg.KeepToolPreamble,
			"historyOffload":          cfg.HistoryOffload,
		},
		"sources": map[string]string{
			"defaultThinking":         sourceOrDefault(sources, "BRIDGE_DEFAULT_THINKING"),
			"accountStrategy":         sourceOrDefault(sources, "BRIDGE_ACCOUNT_STRATEGY"),
			"accountPriority":         sourceOrDefault(sources, "BRIDGE_ACCOUNT_PRIORITY"),
			"rejectDowngrade":         sourceOrDefault(sources, "BRIDGE_REJECT_DOWNGRADE"),
			"rejectLabelMismatch":     sourceOrDefault(sources, "BRIDGE_REJECT_LABEL_MISMATCH"),
			"quotaPreflight":          sourceOrDefault(sources, "BRIDGE_QUOTA_PREFLIGHT"),
			"maxRpmPerAccount":        sourceOrDefault(sources, "BRIDGE_MAX_RPM_PER_ACCOUNT"),
			"maxConcurrentPerAccount": sourceOrDefault(sources, "BRIDGE_MAX_CONCURRENT_PER_ACCOUNT"),
			"liveThoughts":            sourceOrDefault(sources, "BRIDGE_LIVE_THOUGHTS"),
			"liveToolCalls":           sourceOrDefault(sources, "BRIDGE_LIVE_TOOL_CALLS"),
			"keepToolPreamble":        sourceOrDefault(sources, "BRIDGE_TOOL_PREAMBLE"),
			"historyOffload":          sourceOrDefault(sources, "BRIDGE_HISTORY_OFFLOAD"),
		},
	}
}

func sourceOrDefault(m map[string]string, key string) string {
	if v := m[key]; v != "" {
		return v
	}
	return "default"
}

func (s *Server) displayPoolKey(apiKey string) string {
	if apiKey == "" || apiKey == "public" {
		return s.defaultPoolToken()
	}
	return apiKey
}

// HandleAdmin routes /v1/admin/* requests when Cfg.WebUI is enabled.
func (s *Server) HandleAdmin(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAdminAPI(w, r) {
		return
	}
	p := r.URL.Path

	switch {
	case r.Method == http.MethodGet && p == "/v1/admin/config":
		sendJSON(w, http.StatusOK, s.buildAdminConfigPayload(), nil)
		return

	case r.Method == http.MethodPatch && p == "/v1/admin/config":
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		s.handleAdminPatchConfig(w, body)
		return

	case r.Method == http.MethodPost && p == "/v1/admin/accounts/import":
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		s.handleAdminImportAccount(ctx, w, body)
		return

	case r.Method == http.MethodPost && p == "/v1/admin/accounts/browser-login":
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		loginCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		defer cancel()
		res, err := cookies.RunIsolatedBrowserLogin(loginCtx, "")
		if err != nil {
			sendJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"message": err.Error(), "type": "invalid_request_error"},
			}, nil)
			return
		}
		body["cookie"] = res.CookieHeader
		s.handleAdminImportAccount(ctx, w, body)
		return

	case strings.HasPrefix(p, "/v1/admin/accounts/"):
		rest := strings.TrimPrefix(p, "/v1/admin/accounts/")
		parts := strings.Split(rest, "/")
		acctID := strings.TrimSpace(parts[0])
		if !reSafeID.MatchString(acctID) {
			sendJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"message": fmt.Sprintf("invalid account id %q", acctID), "type": "invalid_request_error"},
			}, nil)
			return
		}
		if len(parts) == 1 && r.Method == http.MethodDelete {
			s.handleAdminDeleteAccount(w, acctID)
			return
		}
		if len(parts) == 2 && r.Method == http.MethodPost {
			action := strings.TrimSpace(parts[1])
			switch action {
			case "probe":
				s.handleAdminProbeAccount(ctx, w, acctID)
				return
			case "rotate":
				s.handleAdminRotateAccount(ctx, w, acctID)
				return
			case "unquarantine":
				s.handleAdminUnquarantineAccount(w, acctID)
				return
			case "toggle":
				s.handleAdminToggleAccount(w, acctID)
				return
			case "move":
				body, ok := readJSONBody(w, r)
				if !ok {
					return
				}
				s.handleAdminMoveAccount(w, acctID, body)
				return
			}
		}
	}

	sendJSON(w, http.StatusNotFound, map[string]any{
		"error": map[string]any{
			"message": "not found: " + p,
			"type":    "invalid_request_error",
		},
	}, nil)
}

func (s *Server) handleAdminPatchConfig(w http.ResponseWriter, body map[string]any) {
	envUpdates := make(map[string]string)

	s.mu.Lock()
	cfg := s.Cfg
	if cfg.FieldSources == nil {
		cfg.FieldSources = make(map[string]string)
	}

	if raw, exists := body["defaultThinking"]; exists {
		val := strings.ToLower(strings.TrimSpace(fmt.Sprint(raw)))
		enumVal, ok := config.ThinkingLevels[val]
		if !ok {
			s.mu.Unlock()
			sendJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"message": fmt.Sprintf("invalid defaultThinking %q (expected low, medium, or high)", val), "type": "invalid_request_error"},
			}, nil)
			return
		}
		cfg.DefaultThinking = enumVal
		envUpdates["BRIDGE_DEFAULT_THINKING"] = val
	}

	if raw, exists := body["accountStrategy"]; exists {
		val := strings.ToLower(strings.TrimSpace(fmt.Sprint(raw)))
		if val != "rotate" && val != "lru" && val != "fill-first" {
			s.mu.Unlock()
			sendJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"message": fmt.Sprintf("invalid accountStrategy %q (expected rotate, lru, or fill-first)", val), "type": "invalid_request_error"},
			}, nil)
			return
		}
		cfg.AccountStrategy = val
		envUpdates["BRIDGE_ACCOUNT_STRATEGY"] = val
	}

	if raw, exists := body["accountPriority"]; exists {
		var prio []string
		switch v := raw.(type) {
		case string:
			for _, item := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' }) {
				t := strings.TrimSpace(item)
				if t != "" {
					prio = append(prio, t)
				}
			}
		case []any:
			for _, item := range v {
				t := strings.TrimSpace(fmt.Sprint(item))
				if t != "" {
					prio = append(prio, t)
				}
			}
		default:
			s.mu.Unlock()
			sendJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"message": "accountPriority must be a comma-separated string or array of account IDs", "type": "invalid_request_error"},
			}, nil)
			return
		}
		cfg.AccountPriority = prio
		envUpdates["BRIDGE_ACCOUNT_PRIORITY"] = strings.Join(prio, ",")
	}

	if raw, exists := body["quotaPreflight"]; exists {
		val := strings.ToLower(strings.TrimSpace(fmt.Sprint(raw)))
		if val != "strict" && val != "advisory" && val != "off" {
			s.mu.Unlock()
			sendJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"message": fmt.Sprintf("invalid quotaPreflight %q (expected strict, advisory, or off)", val), "type": "invalid_request_error"},
			}, nil)
			return
		}
		cfg.QuotaPreflight = val
		envUpdates["BRIDGE_QUOTA_PREFLIGHT"] = val
	}

	if raw, exists := body["maxRpmPerAccount"]; exists {
		n, ok := parseJSONInt(raw)
		if !ok || n < 0 || n > 600 {
			s.mu.Unlock()
			sendJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"message": "maxRpmPerAccount must be an integer between 0 and 600", "type": "invalid_request_error"},
			}, nil)
			return
		}
		cfg.MaxRPMPerAccount = n
		envUpdates["BRIDGE_MAX_RPM_PER_ACCOUNT"] = strconv.Itoa(n)
	}

	if raw, exists := body["maxConcurrentPerAccount"]; exists {
		n, ok := parseJSONInt(raw)
		if !ok || n < 0 || n > 64 {
			s.mu.Unlock()
			sendJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"message": "maxConcurrentPerAccount must be an integer between 0 and 64", "type": "invalid_request_error"},
			}, nil)
			return
		}
		cfg.MaxConcurrentPerAccount = n
		envUpdates["BRIDGE_MAX_CONCURRENT_PER_ACCOUNT"] = strconv.Itoa(n)
	}

	boolFields := []struct {
		jsonKey string
		envKey  string
		target  *bool
	}{
		{"rejectDowngrade", "BRIDGE_REJECT_DOWNGRADE", &cfg.RejectDowngrade},
		{"rejectLabelMismatch", "BRIDGE_REJECT_LABEL_MISMATCH", &cfg.RejectLabelMismatch},
		{"liveThoughts", "BRIDGE_LIVE_THOUGHTS", &cfg.LiveThoughts},
		{"liveToolCalls", "BRIDGE_LIVE_TOOL_CALLS", &cfg.LiveToolCalls},
		{"keepToolPreamble", "BRIDGE_TOOL_PREAMBLE", &cfg.KeepToolPreamble},
		{"historyOffload", "BRIDGE_HISTORY_OFFLOAD", &cfg.HistoryOffload},
	}
	for _, bf := range boolFields {
		if raw, exists := body[bf.jsonKey]; exists {
			b, ok := raw.(bool)
			if !ok {
				s.mu.Unlock()
				sendJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{"message": fmt.Sprintf("%s must be a boolean", bf.jsonKey), "type": "invalid_request_error"},
				}, nil)
				return
			}
			*bf.target = b
			if b {
				envUpdates[bf.envKey] = "1"
			} else {
				envUpdates[bf.envKey] = "0"
			}
		}
	}

	persist, _ := body["persist"].(bool)
	envPath := cfg.EnvFile
	if envPath == "" && cfg.HomeDir != "" {
		envPath = filepath.Join(cfg.HomeDir, "config.env")
		cfg.EnvFile = envPath
	}
	for k := range envUpdates {
		if persist {
			cfg.FieldSources[k] = "file"
		} else if cfg.FieldSources[k] == "" {
			cfg.FieldSources[k] = "runtime"
		}
	}
	s.Cfg = cfg
	s.mu.Unlock()

	if persist && len(envUpdates) > 0 {
		if err := config.SaveEnvFile(envPath, envUpdates); err != nil {
			sendJSON(w, http.StatusInternalServerError, map[string]any{
				"error": map[string]any{"message": fmt.Sprintf("failed to save %s: %v", envPath, err), "type": "server_error"},
			}, nil)
			return
		}
	}

	payload := s.buildAdminConfigPayload()
	payload["persisted"] = persist
	sendJSON(w, http.StatusOK, payload, nil)
}

func parseJSONInt(v any) (int, bool) {
	switch val := v.(type) {
	case float64:
		if float64(int(val)) != val {
			return 0, false
		}
		return int(val), true
	case int:
		return val, true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(val))
		return n, err == nil
	default:
		return 0, false
	}
}

func (s *Server) findAccountByID(id string) *AccountState {
	for _, a := range s.Accounts() {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (s *Server) handleAdminImportAccount(ctx context.Context, w http.ResponseWriter, body map[string]any) {
	acctID, _ := body["id"].(string)
	acctID = strings.TrimSpace(acctID)
	if acctID != "" && !reSafeID.MatchString(acctID) {
		sendJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": "account id must be 1-64 characters of A-Z, a-z, 0-9, '.', '_', or '-'",
				"type":    "invalid_request_error",
			},
		}, nil)
		return
	}
	rawCookie, _ := body["cookie"].(string)
	rawCookie = strings.TrimSpace(rawCookie)
	if rawCookie == "" {
		sendJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": "cookie payload is required (paste a raw Cookie header or Cookie-Editor JSON array)",
				"type":    "invalid_request_error",
			},
		}, nil)
		return
	}

	hdr, _, err := cookies.ParseImportPayload(rawCookie)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "invalid_request_error"},
		}, nil)
		return
	}
	if cookies.ExtractSAPISID(hdr) == "" {
		sendJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": "cookie payload does not contain SAPISID; sign in to https://gemini.google.com/app first",
				"type":    "invalid_request_error",
			},
		}, nil)
		return
	}

	acctGroup, _ := body["accountGroup"].(string)
	acctGroup = strings.TrimSpace(acctGroup)
	proxyURL, _ := body["proxyUrl"].(string)
	proxyURL = strings.TrimSpace(proxyURL)

	s.mu.RLock()
	acctsDir := s.Cfg.AccountsDir
	locale := s.Cfg.Locale
	blOverride := s.Cfg.BLOverride
	protoMode := s.Cfg.ProtocolMode
	s.mu.RUnlock()

	if err := os.MkdirAll(acctsDir, 0o700); err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": fmt.Sprintf("create accounts dir: %v", err), "type": "server_error"},
		}, nil)
		return
	}

	if acctID == "" {
		// Bootstrap a temporary AccountClient first to discover the Google email and derive a clean ID.
		tmpClient := gemini.NewAccountClient("pending", hdr, "", s.Pool, s.Rotator)
		tmpClient.SetRuntimeOptions(locale, blOverride, protoMode, acctGroup, proxyURL)
		bootCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		bootErr := tmpClient.Bootstrap(bootCtx)
		cancel()
		if bootErr != nil {
			sendChatError(w, false, fmt.Errorf("bootstrap with imported cookies failed: %w", bootErr))
			return
		}
		acctID = deriveAccountIDFromEmail(tmpClient.Email(), s.Accounts())
	}

	outPath := filepath.Join(acctsDir, acctID+".json")
	ac := gemini.NewAccountClient(acctID, hdr, outPath, s.Pool, s.Rotator)
	ac.SetProvenance("web-ui", "cookie-export")
	ac.SetRuntimeOptions(locale, blOverride, protoMode, acctGroup, proxyURL)

	// Persist initial credential file so custom clientFactory or live bootstrap can load it.
	nowISO := s.now().UTC().Format(time.RFC3339)
	initialRec := cookies.CredentialFile{
		Kind:         "gemini-web",
		Name:         acctID,
		Cookies:      hdr,
		CapturedAt:   nowISO,
		RotatedAt:    nowISO,
		ImportedFrom: "web-ui",
		ProfileID:    "cookie-export",
		AccountGroup: acctGroup,
		ProxyURL:     proxyURL,
	}
	if _, err := cookies.SaveCredentialFile(outPath, initialRec); err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": fmt.Sprintf("save credential file: %v", err), "type": "server_error"},
		}, nil)
		return
	}

	s.LoadCredentials()
	acct := s.findAccountByID(acctID)
	if acct == nil {
		sendJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": "account was saved but could not be loaded into pool", "type": "server_error"},
		}, nil)
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := s.probeSingleAccount(probeCtx, acct); err != nil {
		_ = os.Remove(outPath)
		s.LoadCredentials()
		sendChatError(w, false, fmt.Errorf("bootstrap with imported cookies failed: %w", err))
		return
	}

	sendJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"account": s.accountStatusMap(acct, s.now()),
	}, nil)
}

func (s *Server) probeSingleAccount(ctx context.Context, a *AccountState) error {
	c := s.clientFor(a)
	if err := c.Bootstrap(ctx); err != nil {
		a.mu.Lock()
		a.ModelsError = err
		a.LastError = err.Error()
		a.LastFail = s.now()
		a.Fails++
		a.mu.Unlock()
		return err
	}
	res, err := c.AvailableModels(ctx, true)
	if err != nil {
		a.mu.Lock()
		a.ModelsError = err
		a.LastError = err.Error()
		a.LastFail = s.now()
		a.Fails++
		a.mu.Unlock()
		return err
	}
	quota := c.GetQuota(ctx)
	now := s.now()
	a.mu.Lock()
	if ac, ok := c.(*gemini.AccountClient); ok {
		if em := ac.Email(); em != "" {
			a.Email = em
		}
	}
	a.Models = res.Models
	a.Capacity = res.Capacity
	a.ModelsAt = now
	a.ModelsError = nil
	a.Fails = 0
	a.LastError = ""
	a.LastFail = time.Time{}
	if a.Surfaces == nil {
		a.Surfaces = make(map[string]string)
	}
	a.Surfaces["geminiWeb"] = "ok"
	a.mu.Unlock()
	if len(quota) > 0 {
		s.mergeAccountQuota(a, quota)
	}
	return nil
}

func (s *Server) handleAdminProbeAccount(ctx context.Context, w http.ResponseWriter, acctID string) {
	acct := s.findAccountByID(acctID)
	if acct == nil {
		sendJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"message": fmt.Sprintf("account %q not found", acctID), "type": "invalid_request_error"},
		}, nil)
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	probeErr := s.probeSingleAccount(probeCtx, acct)
	statusCode := http.StatusOK
	resp := map[string]any{
		"status":  "ok",
		"account": s.accountStatusMap(acct, s.now()),
	}
	if probeErr != nil {
		resp["status"] = "error"
		resp["probeError"] = probeErr.Error()
	}
	sendJSON(w, statusCode, resp, nil)
}

func (s *Server) handleAdminRotateAccount(ctx context.Context, w http.ResponseWriter, acctID string) {
	acct := s.findAccountByID(acctID)
	if acct == nil {
		sendJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"message": fmt.Sprintf("account %q not found", acctID), "type": "invalid_request_error"},
		}, nil)
		return
	}
	rotCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := s.clientFor(acct)
	res, err := c.RotateCookies(rotCtx)
	if err != nil {
		sendChatError(w, false, err)
		return
	}
	now := s.now()
	if res != nil && res.Rotated {
		acct.mu.Lock()
		acct.RotatedAt = now
		acct.mu.Unlock()
	}
	out := map[string]any{
		"status":  "ok",
		"account": s.accountStatusMap(acct, now),
	}
	if res != nil {
		out["rotated"] = res.Rotated
		out["upstreamStatus"] = res.Status
	}
	sendJSON(w, http.StatusOK, out, nil)
}

func (s *Server) handleAdminUnquarantineAccount(w http.ResponseWriter, acctID string) {
	acct := s.findAccountByID(acctID)
	if acct == nil {
		sendJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"message": fmt.Sprintf("account %q not found", acctID), "type": "invalid_request_error"},
		}, nil)
		return
	}

	acct.mu.Lock()
	acct.Quarantined = false
	acct.QuarantineReason = ""
	acct.Fails = 0
	acct.LastFail = time.Time{}
	acct.LastError = ""
	acct.ModelsError = nil
	acct.QuotaBackoffStep = 0
	acct.QuotaNextAllowAt = time.Time{}
	clearSyntheticQuotaLocked(acct)
	if acct.Surfaces == nil {
		acct.Surfaces = make(map[string]string)
	}
	acct.Surfaces["geminiWeb"] = "ok"
	filePath := acct.FilePath
	client := acct.Client
	acct.mu.Unlock()

	if ac, ok := client.(*gemini.AccountClient); ok {
		ac.SetQuarantined(false, "")
		ac.SetSurfaceStatus("geminiWeb", "ok")
	} else if filePath != "" {
		if rec, _, err := cookies.LoadAccountFile(filePath); err == nil && rec != nil {
			rec.Quarantined = false
			rec.QuarantineReason = ""
			if rec.Surfaces == nil {
				rec.Surfaces = make(map[string]string)
			}
			rec.Surfaces["geminiWeb"] = "ok"
			_, _ = cookies.SaveCredentialFile(filePath, *rec)
		}
	}

	sendJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"account": s.accountStatusMap(acct, s.now()),
	}, nil)
}

func (s *Server) handleAdminDeleteAccount(w http.ResponseWriter, acctID string) {
	acct := s.findAccountByID(acctID)
	if acct == nil {
		sendJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"message": fmt.Sprintf("account %q not found", acctID), "type": "invalid_request_error"},
		}, nil)
		return
	}

	acct.mu.RLock()
	filePath := acct.FilePath
	acct.mu.RUnlock()

	if filePath != "" {
		_ = os.Remove(filePath)
	} else {
		s.mu.RLock()
		acctsDir := s.Cfg.AccountsDir
		s.mu.RUnlock()
		if acctsDir != "" {
			_ = os.Remove(filepath.Join(acctsDir, acctID+".json"))
		}
	}

	s.mu.Lock()
	kept := make([]*AccountState, 0, len(s.accounts))
	for _, a := range s.accounts {
		if a.ID != acctID {
			kept = append(kept, a)
		}
	}
	s.accounts = kept
	for tok, target := range s.tokenToAcct {
		if target == acctID {
			delete(s.tokenToAcct, tok)
		}
	}
	s.mu.Unlock()

	sendJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"deleted":  acctID,
		"accounts": len(kept),
	}, nil)
}

func deriveAccountIDFromEmail(email string, existing []*AccountState) string {
	local := strings.TrimSpace(email)
	if idx := strings.IndexByte(local, '@'); idx > 0 {
		local = local[:idx]
	}
	var b strings.Builder
	for _, r := range local {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	base := b.String()
	if len(base) > 48 {
		base = base[:48]
	}
	if base == "" || !reSafeID.MatchString(base) {
		base = fmt.Sprintf("acct-%d", time.Now().Unix()%100000)
	}
	return base
}

func (s *Server) handleAdminToggleAccount(w http.ResponseWriter, acctID string) {
	acct := s.findAccountByID(acctID)
	if acct == nil {
		sendJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"message": fmt.Sprintf("account %q not found", acctID), "type": "invalid_request_error"},
		}, nil)
		return
	}

	acct.mu.Lock()
	acct.Disabled = !acct.Disabled
	newDisabled := acct.Disabled
	filePath := acct.FilePath
	client := acct.Client
	acct.mu.Unlock()

	if ac, ok := client.(*gemini.AccountClient); ok {
		ac.SetDisabled(newDisabled)
	} else if filePath != "" {
		if rec, _, err := cookies.LoadAccountFile(filePath); err == nil && rec != nil {
			rec.Disabled = newDisabled
			_, _ = cookies.SaveCredentialFile(filePath, *rec)
		}
	}

	sendJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"disabled": newDisabled,
		"account":  s.accountStatusMap(acct, s.now()),
	}, nil)
}

func (s *Server) handleAdminMoveAccount(w http.ResponseWriter, acctID string, body map[string]any) {
	dir, _ := body["direction"].(string)
	dir = strings.ToLower(strings.TrimSpace(dir))
	if dir != "up" && dir != "down" {
		sendJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "direction must be 'up' or 'down'", "type": "invalid_request_error"},
		}, nil)
		return
	}

	s.mu.Lock()
	idx := -1
	for i, a := range s.accounts {
		if a.ID == acctID {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		sendJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"message": fmt.Sprintf("account %q not found", acctID), "type": "invalid_request_error"},
		}, nil)
		return
	}
	targetIdx := idx - 1
	if dir == "down" {
		targetIdx = idx + 1
	}
	if targetIdx >= 0 && targetIdx < len(s.accounts) {
		s.accounts[idx], s.accounts[targetIdx] = s.accounts[targetIdx], s.accounts[idx]
	}
	prio := make([]string, 0, len(s.accounts))
	for _, a := range s.accounts {
		prio = append(prio, a.ID)
	}
	s.Cfg.AccountPriority = prio
	if s.Cfg.FieldSources == nil {
		s.Cfg.FieldSources = make(map[string]string)
	}
	s.Cfg.FieldSources["BRIDGE_ACCOUNT_PRIORITY"] = "file"
	envPath := s.Cfg.EnvFile
	if envPath == "" && s.Cfg.HomeDir != "" {
		envPath = filepath.Join(s.Cfg.HomeDir, "config.env")
		s.Cfg.EnvFile = envPath
	}
	s.mu.Unlock()

	if envPath != "" {
		_ = config.SaveEnvFile(envPath, map[string]string{
			"BRIDGE_ACCOUNT_PRIORITY": strings.Join(prio, ","),
		})
	}

	sendJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"accountPriority": prio,
	}, nil)
}
