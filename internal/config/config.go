package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Version is the build version of gemini-stealth-bridge (overridable via -ldflags).
var Version = "0.4.1"

// Config holds all runtime settings for the gemini-stealth-bridge daemon.
// Environment variables are 100% compatible with gemini-app-bridge.
type Config struct {
	Host                    string
	Port                    int
	HomeDir                 string
	CredFile                string
	AccountsDir             string
	CookieDB                string
	APIKey                  string
	AccountTokens           map[string]string
	GroupTokens             map[string]string
	IncludeGrounding        bool
	SearchTimeout           time.Duration
	Temporary               bool
	RejectDowngrade         bool
	LiveThoughts            bool
	SessionCache            bool
	KeepToolPreamble        bool
	LiveToolCalls           bool
	MaxPromptChars          int
	MaxOldToolChars         int
	RotateCookiesEvery      time.Duration
	ModelRefreshEvery       time.Duration
	StreamTimeout           time.Duration
	FirstByteTimeout        time.Duration
	StallTimeout            time.Duration
	IdleSessionTimeout      time.Duration
	MaxSessions             int
	MaxQueueDepth           int
	SessionWaitTimeout      time.Duration
	ProxyURL                string
	ImpersonateProfile      string
	DefaultThinking         int
	UserAgent               string
	SecCHUA                 string
	SecCHUAPlatform         string
	ContextWindow           int
	MaxOutputTokens         int
	TokenEstimator          string
	EarlySSE                bool
	EarlySSEGrace           time.Duration
	StreamBufferMode        string
	MaxAttempts             int
	MaxUpstreamAttempts     int
	RetryBudget             time.Duration
	ToolSchemaCompact       bool
	ToolDescMaxChars        int
	QuotaPreflight          string
	LogRequests             bool
	DumpRequestDir          string
	TLSExtOrder             string
	FollowRedirects         bool
	IPv6Mode                string
	UAOS                    string
	ProtocolMode            string
	BLOverride              string
	Locale                  string
	AcceptLanguage          string
	MaxRPMPerAccount        int
	MaxConcurrentPerAccount int
	QuietHours              string
	AccountGroups           map[string]string
	AccountStrategy         string
	AccountPriority         []string
	HistoryOffload          bool
	PersonalContext         string
	EgressPerAccount        map[string]string
	WebUI                   bool
	AdminKey                string
	EnvFile                 string
	FieldSources            map[string]string
	Warnings                []string
	// RejectLabelMismatch turns a relabel into an error instead of a reported
	// fact. Upstream can rename a tier in place (same backend id, new catalogue
	// name), so the served label is the only evidence of what actually answered.
	// Reporting is the default because rejecting would take the renamed tier
	// offline; strict deployments can opt in.
	RejectLabelMismatch bool
}

// ThinkingLevels maps Gemini Web's 3 thinking levels ("low", "medium", "high") to
// Google's Assistant_Lamda_ThinkingLevel protobuf enum (otAQ7b field 24):
//
//	1 = THINKING_LEVEL_STANDARD ("low", default)
//	4 = THINKING_LEVEL_MEDIUM   ("medium")
//	2 = THINKING_LEVEL_EXTENDED ("high")
var ThinkingLevels = map[string]int{
	"low":    1,
	"medium": 4,
	"high":   2,
}

// FromEnv constructs a Config from process environment variables.
func FromEnv() *Config {
	var warnings []string
	home, _ := os.UserHomeDir()
	bridgeHome := envStr("BRIDGE_HOME", filepath.Join(home, ".gemini-stealth-bridge"))
	envFilePath := filepath.Join(bridgeHome, "config.env")
	fileEnv := LoadEnvFile(envFilePath)
	sources := make(map[string]string)

	lookup := func(key string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			sources[key] = "env"
			return v
		}
		if v := strings.TrimSpace(fileEnv[key]); v != "" {
			sources[key] = "file"
			return v
		}
		sources[key] = "default"
		return ""
	}
	lookupStr := func(key, fallback string) string {
		if v := lookup(key); v != "" {
			return v
		}
		return fallback
	}
	lookupBool := func(key string, fallback bool) bool {
		v := strings.ToLower(lookup(key))
		if v == "" {
			return fallback
		}
		switch v {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		default:
			return fallback
		}
	}
	lookupPositiveInt := func(key string, fallback, minVal int) int {
		v := lookup(key)
		if v == "" {
			return fallback
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			warnings = append(warnings, fmt.Sprintf("invalid %s=%q; falling back to %d", key, v, fallback))
			return fallback
		}
		if n < minVal {
			warnings = append(warnings, fmt.Sprintf("%s=%d below minimum %d; clamping to %d", key, n, minVal, minVal))
			return minVal
		}
		return n
	}
	lookupNonNegInt := func(key string, fallback int) int {
		v := lookup(key)
		if v == "" {
			return fallback
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			warnings = append(warnings, fmt.Sprintf("invalid %s=%q; falling back to %d", key, v, fallback))
			return fallback
		}
		return n
	}
	lookupClampInt := func(key string, fallback, minVal, maxVal int) int {
		v := lookup(key)
		if v == "" {
			return fallback
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			warnings = append(warnings, fmt.Sprintf("invalid %s=%q; falling back to %d", key, v, fallback))
			return fallback
		}
		if n < minVal {
			return minVal
		}
		if n > maxVal {
			return maxVal
		}
		return n
	}
	lookupEnum := func(key, fallback string, allowed []string) string {
		raw := lookup(key)
		if raw == "" {
			return fallback
		}
		low := strings.ToLower(raw)
		for _, a := range allowed {
			if low == a {
				return low
			}
		}
		warnings = append(warnings, fmt.Sprintf("invalid %s=%q; falling back to %q", key, raw, fallback))
		return fallback
	}

	credFile := lookupStr("WEB_CRED_FILE", filepath.Join(bridgeHome, "web-credentials.json"))
	accountsDir := filepath.Join(bridgeHome, "accounts")

	tokens := parseKeyValMap(lookup("BRIDGE_ACCOUNT_TOKENS"))
	groupTokens := parseKeyValMap(lookup("BRIDGE_GROUP_TOKENS"))
	accountGroups := parseKeyValMap(lookup("BRIDGE_ACCOUNT_GROUP"))
	egressPerAccount := parseKeyValMap(lookup("BRIDGE_EGRESS_PER_ACCOUNT"))

	rawThink := lookup("BRIDGE_DEFAULT_THINKING")
	defThinkKey := strings.ToLower(rawThink)
	if defThinkKey == "" {
		defThinkKey = "low"
	}
	defThink, ok := ThinkingLevels[defThinkKey]
	if !ok {
		warnings = append(warnings, fmt.Sprintf("invalid BRIDGE_DEFAULT_THINKING=%q; falling back to \"low\"", rawThink))
		defThink = 1
	}

	impersonate := lookupStr("BRIDGE_IMPERSONATE", "chrome_150_psk")
	rawUAOS := lookup("BRIDGE_UA_OS")
	targetOS := runtime.GOOS
	normalizedUAOS := ""
	if rawUAOS != "" {
		switch strings.ToLower(rawUAOS) {
		case "linux":
			targetOS = "linux"
			normalizedUAOS = "linux"
		case "windows", "win":
			targetOS = "windows"
			normalizedUAOS = "windows"
		case "macos", "darwin", "mac":
			targetOS = "darwin"
			normalizedUAOS = "macos"
		default:
			warnings = append(warnings, fmt.Sprintf("invalid BRIDGE_UA_OS=%q; falling back to host OS %q", rawUAOS, runtime.GOOS))
		}
	}
	ua, chUA, platform := browserHeadersForOS(targetOS, impersonate)

	tokenEstimator := lookupEnum("BRIDGE_TOKEN_ESTIMATOR", "script", []string{"script", "bytes4"})
	streamBuffer := lookupEnum("BRIDGE_STREAM_BUFFER", "verified", []string{"verified", "eager"})
	quotaPreflight := lookupEnum("BRIDGE_QUOTA_PREFLIGHT", "strict", []string{"strict", "advisory", "off"})
	tlsExtOrder := lookupEnum("BRIDGE_TLS_EXT_ORDER", "fixed", []string{"fixed", "random"})
	protocolMode := lookupEnum("BRIDGE_PROTOCOL_MODE", "bridge", []string{"bridge", "web"})
	personalContext := lookupEnum("BRIDGE_PERSONAL_CONTEXT", "auto", []string{"auto", "off", "on"})
	accountStrategy := lookupEnum("BRIDGE_ACCOUNT_STRATEGY", "rotate", []string{"rotate", "lru", "fill-first"})
	accountPriority := parseCSVList(lookup("BRIDGE_ACCOUNT_PRIORITY"))
	ipv6Default := "auto"
	if lookup("BRIDGE_DISABLE_IPV6") == "1" {
		ipv6Default = "off"
	}
	ipv6Mode := lookupEnum("BRIDGE_IPV6", ipv6Default, []string{"auto", "off", "on"})

	locale := lookupStr("BRIDGE_LOCALE", "en")
	acceptLang := deriveAcceptLanguage(locale)

	maxAttempts := lookupClampInt("BRIDGE_MAX_ATTEMPTS", 3, 1, 10)
	maxUpstream := lookupClampInt("BRIDGE_MAX_UPSTREAM_ATTEMPTS", 3, 1, 10)

	quietHours := lookup("BRIDGE_QUIET_HOURS")
	if quietHours != "" && !isValidQuietHours(quietHours) {
		warnings = append(warnings, fmt.Sprintf("invalid BRIDGE_QUIET_HOURS=%q; expected HH:MM-HH:MM", quietHours))
	}

	hostVal := lookup("BRIDGE_HOST")
	if hostVal == "" {
		hostVal = lookupStr("HOST", "127.0.0.1")
	}
	portVal := lookupPositiveInt("BRIDGE_PORT", 0, 1)
	if portVal <= 0 {
		portVal = lookupPositiveInt("PORT", 20133, 1)
	}

	return &Config{
		Host:                    hostVal,
		Port:                    portVal,
		HomeDir:                 bridgeHome,
		CredFile:                credFile,
		AccountsDir:             accountsDir,
		CookieDB:                lookup("CHROME_COOKIES"),
		APIKey:                  lookupStr("BRIDGE_API_KEY", "public"),
		AccountTokens:           tokens,
		GroupTokens:             groupTokens,
		IncludeGrounding:        lookupBool("BRIDGE_INCLUDE_GROUNDING", true),
		SearchTimeout:           time.Duration(lookupPositiveInt("BRIDGE_SEARCH_TIMEOUT_MS", 180000, 10000)) * time.Millisecond,
		Temporary:               lookupBool("BRIDGE_TEMPORARY", false),
		RejectDowngrade:         lookupBool("BRIDGE_REJECT_DOWNGRADE", true),
		RejectLabelMismatch:     lookupBool("BRIDGE_REJECT_LABEL_MISMATCH", false),
		LiveThoughts:            lookupBool("BRIDGE_LIVE_THOUGHTS", true),
		SessionCache:            lookupBool("BRIDGE_SESSION_CACHE", true),
		KeepToolPreamble:        lookupBool("BRIDGE_TOOL_PREAMBLE", false),
		LiveToolCalls:           lookupBool("BRIDGE_LIVE_TOOL_CALLS", false),
		MaxPromptChars:          lookupPositiveInt("BRIDGE_MAX_PROMPT_CHARS", 850000, 1024),
		MaxOldToolChars:         lookupPositiveInt("BRIDGE_MAX_OLD_TOOL_CHARS", 32000, 256),
		RotateCookiesEvery:      time.Duration(lookupPositiveInt("BRIDGE_ROTATE_COOKIES_MS", 540000, 1000)) * time.Millisecond,
		ModelRefreshEvery:       time.Duration(lookupPositiveInt("BRIDGE_MODEL_REFRESH_MS", 1800000, 1000)) * time.Millisecond,
		StreamTimeout:           time.Duration(lookupPositiveInt("BRIDGE_STREAM_TIMEOUT_MS", 600000, 1000)) * time.Millisecond,
		FirstByteTimeout:        time.Duration(lookupPositiveInt("BRIDGE_FIRST_BYTE_TIMEOUT_MS", 60000, 1000)) * time.Millisecond,
		StallTimeout:            time.Duration(lookupPositiveInt("BRIDGE_STALL_TIMEOUT_MS", 90000, 1000)) * time.Millisecond,
		IdleSessionTimeout:      time.Duration(lookupPositiveInt("BRIDGE_IDLE_SESSION_MS", 180000, 1000)) * time.Millisecond,
		MaxSessions:             lookupPositiveInt("BRIDGE_MAX_SESSIONS", 16, 1),
		MaxQueueDepth:           lookupPositiveInt("BRIDGE_MAX_QUEUE", 4, 1),
		SessionWaitTimeout:      time.Duration(lookupPositiveInt("BRIDGE_SESSION_WAIT_MS", 10000, 100)) * time.Millisecond,
		ProxyURL:                lookup("BRIDGE_PROXY_SERVER"),
		ImpersonateProfile:      impersonate,
		DefaultThinking:         defThink,
		UserAgent:               ua,
		SecCHUA:                 chUA,
		SecCHUAPlatform:         platform,
		ContextWindow:           lookupPositiveInt("BRIDGE_CONTEXT_WINDOW", 1048576, 4096),
		MaxOutputTokens:         lookupPositiveInt("BRIDGE_MAX_OUTPUT_TOKENS", 65536, 256),
		TokenEstimator:          tokenEstimator,
		EarlySSE:                lookupBool("BRIDGE_EARLY_SSE", false),
		EarlySSEGrace:           time.Duration(lookupNonNegInt("BRIDGE_EARLY_SSE_GRACE_MS", 20000)) * time.Millisecond,
		StreamBufferMode:        streamBuffer,
		MaxAttempts:             maxAttempts,
		MaxUpstreamAttempts:     maxUpstream,
		RetryBudget:             time.Duration(lookupNonNegInt("BRIDGE_RETRY_BUDGET_MS", 150000)) * time.Millisecond,
		ToolSchemaCompact:       lookupBool("BRIDGE_TOOL_SCHEMA_COMPACT", true),
		ToolDescMaxChars:        lookupPositiveInt("BRIDGE_TOOL_DESC_MAX_CHARS", 400, 64),
		QuotaPreflight:          quotaPreflight,
		LogRequests:             lookupBool("BRIDGE_LOG_REQUESTS", false),
		DumpRequestDir:          lookup("BRIDGE_DUMP_REQUEST"),
		TLSExtOrder:             tlsExtOrder,
		FollowRedirects:         lookupBool("BRIDGE_FOLLOW_REDIRECTS", true),
		IPv6Mode:                ipv6Mode,
		UAOS:                    normalizedUAOS,
		ProtocolMode:            protocolMode,
		BLOverride:              lookup("BRIDGE_BL_OVERRIDE"),
		Locale:                  locale,
		AcceptLanguage:          acceptLang,
		MaxRPMPerAccount:        lookupNonNegInt("BRIDGE_MAX_RPM_PER_ACCOUNT", 24),
		MaxConcurrentPerAccount: lookupNonNegInt("BRIDGE_MAX_CONCURRENT_PER_ACCOUNT", 2),
		HistoryOffload:          lookupBool("BRIDGE_HISTORY_OFFLOAD", true),
		PersonalContext:         personalContext,
		QuietHours:              quietHours,
		AccountGroups:           accountGroups,
		AccountStrategy:         accountStrategy,
		AccountPriority:         accountPriority,
		EgressPerAccount:        egressPerAccount,
		WebUI:                   lookupBool("BRIDGE_WEB_UI", false),
		AdminKey:                lookup("BRIDGE_ADMIN_KEY"),
		EnvFile:                 envFilePath,
		FieldSources:            sources,
		Warnings:                warnings,
	}
}

// Validate returns a combined error if any configuration warning or invalid format was recorded.
func (c Config) Validate() error {
	var errs []string
	errs = append(errs, c.Warnings...)
	if c.QuietHours != "" && !isValidQuietHours(c.QuietHours) {
		msg := fmt.Sprintf("invalid BRIDGE_QUIET_HOURS=%q; expected HH:MM-HH:MM", c.QuietHours)
		found := false
		for _, w := range errs {
			if w == msg {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, msg)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Load returns a value copy of FromEnv().
func Load() Config {
	return *FromEnv()
}

// IsLoopbackHost reports whether host is a loopback address.
func IsLoopbackHost(h string) bool {
	if h == "localhost" || h == "::1" || h == "::ffff:127.0.0.1" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// browserHeadersForOS aligns User-Agent and Sec-CH-UA* headers with both the
// host kernel's TCP stack (avoiding OS/JA4T cross-layer contradiction) and the
// active Chrome uTLS profile version.
func browserHeadersForOS(goos, profile string) (ua, secCHUA, platform string) {
	ver := "150"
	p := strings.ToLower(profile)
	switch {
	case strings.Contains(p, "152"):
		ver = "152"
	case strings.Contains(p, "150"):
		ver = "150"
	case strings.Contains(p, "146"):
		ver = "146"
	case strings.Contains(p, "144"):
		ver = "144"
	case strings.Contains(p, "133"):
		ver = "133"
	case strings.Contains(p, "131"):
		ver = "131"
	}

	secCHUA = `"Google Chrome";v="` + ver + `", "Chromium";v="` + ver + `", "Not?A_Brand";v="24"`
	switch goos {
	case "darwin":
		platform = `"macOS"`
		ua = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ver + ".0.0.0 Safari/537.36"
	case "windows":
		platform = `"Windows"`
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ver + ".0.0.0 Safari/537.36"
	default:
		platform = `"Linux"`
		ua = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ver + ".0.0.0 Safari/537.36"
	}
	return ua, secCHUA, platform
}

func envStr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

// parseCSVList splits a comma- or semicolon-separated list, trimming blanks and
// dropping duplicates while preserving the caller's order.
func parseCSVList(raw string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, item := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' }) {
		v := strings.TrimSpace(item)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func parseKeyValMap(raw string) map[string]string {
	out := make(map[string]string)
	for _, pair := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' }) {
		if idx := strings.IndexByte(pair, ':'); idx > 0 {
			k := strings.TrimSpace(pair[:idx])
			v := strings.TrimSpace(pair[idx+1:])
			if k != "" && v != "" {
				out[k] = v
			}
		}
	}
	return out
}

func envPositiveInt(key string, fallback, minVal int, warnings *[]string) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		if warnings != nil {
			*warnings = append(*warnings, fmt.Sprintf("invalid %s=%q; falling back to %d", key, v, fallback))
		}
		return fallback
	}
	if n < minVal {
		if warnings != nil {
			*warnings = append(*warnings, fmt.Sprintf("%s=%d below minimum %d; clamping to %d", key, n, minVal, minVal))
		}
		return minVal
	}
	return n
}

func envNonNegInt(key string, fallback int, warnings *[]string) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		if warnings != nil {
			*warnings = append(*warnings, fmt.Sprintf("invalid %s=%q; falling back to %d", key, v, fallback))
		}
		return fallback
	}
	return n
}

func envClampInt(key string, fallback, minVal, maxVal int, warnings *[]string) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		if warnings != nil {
			*warnings = append(*warnings, fmt.Sprintf("invalid %s=%q; falling back to %d", key, v, fallback))
		}
		return fallback
	}
	if n < minVal {
		return minVal
	}
	if n > maxVal {
		return maxVal
	}
	return n
}

func envEnum(key, fallback string, allowed []string, warnings *[]string) string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	low := strings.ToLower(raw)
	for _, a := range allowed {
		if low == a {
			return low
		}
	}
	if warnings != nil {
		*warnings = append(*warnings, fmt.Sprintf("invalid %s=%q; falling back to %q", key, raw, fallback))
	}
	return fallback
}

func deriveAcceptLanguage(locale string) string {
	loc := strings.TrimSpace(locale)
	if loc == "" || strings.EqualFold(loc, "en") || strings.EqualFold(loc, "en-us") {
		return "en-US,en;q=0.9"
	}
	if strings.Contains(loc, "-") {
		base := strings.SplitN(loc, "-", 2)[0]
		return fmt.Sprintf("%s,%s;q=0.9,en-US;q=0.8,en;q=0.7", loc, base)
	}
	return fmt.Sprintf("%s,en-US;q=0.9,en;q=0.8", loc)
}

func isValidQuietHours(s string) bool {
	parts := strings.Split(strings.TrimSpace(s), "-")
	if len(parts) != 2 {
		return false
	}
	return isValidHHMM(parts[0]) && isValidHHMM(parts[1])
}

func isValidHHMM(s string) bool {
	hm := strings.Split(strings.TrimSpace(s), ":")
	if len(hm) != 2 || len(hm[0]) != 2 || len(hm[1]) != 2 {
		return false
	}
	h, err1 := strconv.Atoi(hm[0])
	m, err2 := strconv.Atoi(hm[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return h >= 0 && h <= 23 && m >= 0 && m <= 59
}

// LoadEnvFile reads KEY=VALUE pairs from a dotenv-style configuration file.
// Missing files return an empty map without error.
func LoadEnvFile(path string) map[string]string {
	out := make(map[string]string)
	if strings.TrimSpace(path) == "" {
		return out
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "export ") {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
		}
		idx := strings.IndexByte(trimmed, '=')
		if idx <= 0 {
			continue
		}
		k := strings.TrimSpace(trimmed[:idx])
		v := strings.TrimSpace(trimmed[idx+1:])
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		if k != "" {
			out[k] = v
		}
	}
	return out
}

// SaveEnvFile merges updates into path atomically with 0600 permissions.
func SaveEnvFile(path string, updates map[string]string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("empty config.env path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	merged := LoadEnvFile(path)
	for k, v := range updates {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		merged[k] = strings.TrimSpace(v)
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Gemini Stealth Bridge runtime configuration (managed by gsb)\n")
	for _, k := range keys {
		val := strings.ReplaceAll(merged[k], "\n", " ")
		val = strings.ReplaceAll(val, "\r", " ")
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(val)
		b.WriteByte('\n')
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
