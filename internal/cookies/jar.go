package cookies

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// EssentialNames lists the Google session cookies required by gemini.google.com
// in canonical wire order. Note: AEC is optional and may be absent in modern browser profiles.
var EssentialNames = []string{
	"SID", "HSID", "SSID", "APISID", "SAPISID",
	"OSID", "__Secure-OSID",
	"__Secure-1PSID", "__Secure-1PAPISID", "__Secure-3PSID", "__Secure-3PAPISID",
	"__Secure-1PSIDTS", "__Secure-3PSIDTS",
	"SIDCC", "__Secure-1PSIDCC", "__Secure-3PSIDCC", "NID", "AEC",
	"LSID", "__Host-1PLSID", "__Host-3PLSID", "__Host-GAPS",
	"ACCOUNT_CHOOSER",
}

var essentialSet = func() map[string]bool {
	m := make(map[string]bool, len(EssentialNames))
	for _, n := range EssentialNames {
		m[n] = true
	}
	return m
}()

// CredentialFile represents the JSON structure stored in ~/.gemini-app-bridge/accounts/<id>.json.
type CredentialFile struct {
	Kind             string            `json:"kind"`
	Name             string            `json:"name,omitempty"`
	Email            string            `json:"email"`
	Cookies          string            `json:"cookies"`
	AT               string            `json:"at,omitempty"`
	BL               string            `json:"bl,omitempty"`
	FSid             string            `json:"fSid,omitempty"`
	AuthUser         int               `json:"authUser,omitempty"`
	CapturedAt       string            `json:"capturedAt"`
	RotatedAt        string            `json:"rotatedAt,omitempty"`
	CookieDB         string            `json:"cookieDb,omitempty"`
	ImportedFrom     string            `json:"importedFrom,omitempty"`
	ProfileID        string            `json:"profileId,omitempty"`
	TokenFetchedAt   string            `json:"tokenFetchedAt,omitempty"`
	TokenSource      string            `json:"tokenSource,omitempty"`
	LastReqID        int64             `json:"lastReqId,omitempty"`
	Quarantined      bool              `json:"quarantined,omitempty"`
	QuarantineReason string            `json:"quarantineReason,omitempty"`
	Disabled         bool              `json:"disabled,omitempty"`
	AccountGroup     string            `json:"accountGroup,omitempty"`
	ProxyURL         string            `json:"proxyUrl,omitempty"`
	Surfaces         map[string]string `json:"surfaces,omitempty"`
}

func trimCookieHeaderPrefix(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 7 && strings.EqualFold(s[:7], "cookie:") {
		s = strings.TrimSpace(s[7:])
	}
	return s
}

// ParseHeader splits a Cookie header string (or multi-line HTTP request headers)
// into key-value pairs.
func ParseHeader(header string) map[string]string {
	if strings.ContainsAny(header, "\r\n") {
		var cookieLines []string
		for _, line := range strings.Split(strings.ReplaceAll(header, "\r\n", "\n"), "\n") {
			t := strings.TrimSpace(line)
			if len(t) >= 7 && strings.EqualFold(t[:7], "cookie:") {
				if val := strings.TrimSpace(t[7:]); val != "" {
					cookieLines = append(cookieLines, val)
				}
			}
		}
		if len(cookieLines) > 0 {
			header = strings.Join(cookieLines, "; ")
		}
	}
	header = trimCookieHeaderPrefix(header)
	out := make(map[string]string, len(EssentialNames))
	for _, part := range strings.Split(header, ";") {
		part = trimCookieHeaderPrefix(part)
		if i := strings.IndexByte(part, '='); i > 0 {
			k := strings.TrimSpace(part[:i])
			v := strings.TrimSpace(part[i+1:])
			if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
				v = v[1 : len(v)-1]
			}
			if k != "" && v != "" {
				out[k] = v
			}
		}
	}
	return out
}

// CanonicalHeader formats a cookie map into the canonical EssentialNames order.
func CanonicalHeader(m map[string]string) string {
	var parts []string
	for _, name := range EssentialNames {
		if v := m[name]; v != "" {
			parts = append(parts, name+"="+v)
		}
	}
	return strings.Join(parts, "; ")
}

// ExtractSAPISID returns the SAPISID value from a Cookie header string.
func ExtractSAPISID(header string) string {
	return ParseHeader(header)["SAPISID"]
}

// Subset returns a Cookie header containing only the requested cookie names.
func Subset(header string, names []string) string {
	m := ParseHeader(header)
	var parts []string
	for _, n := range names {
		if v := m[n]; v != "" {
			parts = append(parts, n+"="+v)
		}
	}
	return strings.Join(parts, "; ")
}

// AbsorbSetCookie merges Set-Cookie response headers into an existing Cookie
// header. Cookies in EssentialNames with deletion markers (empty value, "deleted",
// Max-Age<=0, or past Expires) are removed from the jar.
// Returns the updated canonical Cookie header and true if any value changed.
func AbsorbSetCookie(current string, setCookies []string) (string, bool) {
	if len(setCookies) == 0 {
		return current, false
	}
	m := ParseHeader(current)
	changed := false
	now := time.Now()
	for _, raw := range setCookies {
		head := raw
		attrs := ""
		if semi := strings.IndexByte(raw, ';'); semi >= 0 {
			head = raw[:semi]
			attrs = raw[semi+1:]
		}
		i := strings.IndexByte(head, '=')
		if i <= 0 {
			continue
		}
		name := strings.TrimSpace(head[:i])
		val := strings.TrimSpace(head[i+1:])
		if !essentialSet[name] {
			continue
		}
		if isCookieDeletion(val, attrs, now) {
			if _, exists := m[name]; exists {
				delete(m, name)
				changed = true
			}
			continue
		}
		if m[name] != val {
			m[name] = val
			changed = true
		}
	}
	if !changed {
		return current, false
	}
	return CanonicalHeader(m), true
}

func isCookieDeletion(val, attrs string, now time.Time) bool {
	if val == "" || strings.EqualFold(val, "deleted") || strings.EqualFold(val, `""`) {
		return true
	}
	for _, part := range strings.Split(attrs, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ak, av, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		ak = strings.ToLower(strings.TrimSpace(ak))
		av = strings.TrimSpace(av)
		switch ak {
		case "max-age":
			if ma, err := strconv.Atoi(av); err == nil && ma <= 0 {
				return true
			}
		case "expires":
			av = strings.Trim(av, `"`)
			if exp, err := http.ParseTime(av); err == nil {
				if exp.Before(now) {
					return true
				}
				continue
			}
			for _, layout := range []string{
				"Mon, 02-Jan-2006 15:04:05 MST",
				"Monday, 02-Jan-2006 15:04:05 MST",
				"Mon, 02-Jan-06 15:04:05 MST",
			} {
				if exp, err := time.Parse(layout, av); err == nil {
					if exp.Before(now) {
						return true
					}
					break
				}
			}
		}
	}
	return false
}

// SAPISIDHash computes the Authorization: SAPISIDHASH header required by
// Google's batchexecute and StreamGenerate endpoints.
func SAPISIDHash(sapisidOrHeader, origin string) string {
	sapisid := strings.TrimSpace(sapisidOrHeader)
	if strings.Contains(sapisid, "=") {
		sapisid = ExtractSAPISID(sapisid)
	}
	if sapisid == "" {
		return ""
	}
	if origin == "" {
		origin = "https://gemini.google.com"
	}
	ts := time.Now().Unix()
	sum := sha1.Sum([]byte(fmt.Sprintf("%d %s %s", ts, sapisid, origin)))
	return fmt.Sprintf("SAPISIDHASH %d_%s", ts, hex.EncodeToString(sum[:]))
}

// ParseImportPayload parses a raw Cookie header string, F12 cURL command,
// Netscape cookies.txt / DevTools Application Cookies TSV, browser extension
// JSON array of cookie objects, or a CredentialFile JSON object.
func ParseImportPayload(raw string) (header string, cred *CredentialFile, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil, fmt.Errorf("empty cookie payload")
	}
	if strings.HasPrefix(trimmed, "{") {
		var cf CredentialFile
		if err := json.Unmarshal([]byte(trimmed), &cf); err == nil && cf.Cookies != "" {
			norm := CanonicalHeader(ParseHeader(cf.Cookies))
			if norm == "" {
				norm = cf.Cookies
			}
			cf.Cookies = norm
			return norm, &cf, nil
		}
		var wrapper struct {
			Cookies []domainCookie `json:"cookies"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapper); err == nil && len(wrapper.Cookies) > 0 {
			h := selectBestDomainCookies(wrapper.Cookies)
			return h, nil, nil
		}
	}
	if strings.HasPrefix(trimmed, "[") {
		var list []domainCookie
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			return "", nil, fmt.Errorf("invalid cookie JSON array: %w", err)
		}
		return selectBestDomainCookies(list), nil, nil
	}
	if curlCookies := extractCurlCookies(trimmed); curlCookies != "" {
		m := ParseHeader(curlCookies)
		return CanonicalHeader(m), nil, nil
	}
	if tsvList := parseTSVCookies(trimmed); len(tsvList) > 0 {
		return selectBestDomainCookies(tsvList), nil, nil
	}
	m := ParseHeader(trimmed)
	return CanonicalHeader(m), nil, nil
}

type domainCookie struct {
	Name           string  `json:"name"`
	Value          string  `json:"value"`
	Domain         string  `json:"domain"`
	Host           string  `json:"host"`
	URL            string  `json:"url"`
	Path           string  `json:"path"`
	ExpirationDate float64 `json:"expirationDate"`
	Expiry         float64 `json:"expiry"`
}

func isExpiredEpoch(exp float64, now time.Time) bool {
	if exp <= 0 {
		return false
	}
	if exp > 1e12 {
		exp = exp / 1000
	}
	return exp < float64(now.Unix())
}

func resolveCookieDomain(c domainCookie) string {
	if d := strings.TrimSpace(c.Domain); d != "" {
		return d
	}
	if h := strings.TrimSpace(c.Host); h != "" {
		return h
	}
	if uStr := strings.TrimSpace(c.URL); uStr != "" {
		if !strings.Contains(uStr, "://") {
			uStr = "https://" + uStr
		}
		if u, err := url.Parse(uStr); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return ""
}

func tokenizeShellArgs(s string) []string {
	var args []string
	var cur strings.Builder
	inTok := false
	var quote byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
				continue
			}
			if quote == '"' && ch == '\\' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				continue
			}
			cur.WriteByte(ch)
			continue
		}
		switch ch {
		case ' ', '\t', '\r', '\n':
			if inTok {
				args = append(args, cur.String())
				cur.Reset()
				inTok = false
			}
		case '\'', '"':
			quote = ch
			inTok = true
		case '$':
			if i+1 < len(s) && s[i+1] == '\'' {
				quote = '\''
				inTok = true
				i++
			} else {
				cur.WriteByte(ch)
				inTok = true
			}
		case '\\':
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				inTok = true
			}
		default:
			cur.WriteByte(ch)
			inTok = true
		}
	}
	if inTok {
		args = append(args, cur.String())
	}
	return args
}

func extractCurlCookies(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	isCurl := strings.HasPrefix(lower, "curl ") || strings.HasPrefix(lower, "curl\t") ||
		strings.HasPrefix(lower, "curl\n") || strings.HasPrefix(lower, "curl\r") ||
		strings.HasPrefix(lower, "curl.exe ")
	if !isCurl {
		isCurl = strings.Contains(lower, "-h ") || strings.Contains(lower, "--header ") ||
			strings.Contains(lower, "-b ") || strings.Contains(lower, "--cookie ")
	}
	if !isCurl {
		return ""
	}
	norm := raw
	norm = strings.ReplaceAll(norm, "\\\r\n", " ")
	norm = strings.ReplaceAll(norm, "\\\n", " ")
	norm = strings.ReplaceAll(norm, "^\r\n", " ")
	norm = strings.ReplaceAll(norm, "^\n", " ")
	norm = strings.ReplaceAll(norm, "^\"", "\"")

	args := tokenizeShellArgs(norm)
	var cookies []string
	appendHeaderIfCookie := func(h string) {
		t := strings.TrimSpace(h)
		if len(t) >= 7 && strings.EqualFold(t[:7], "cookie:") {
			if val := strings.TrimSpace(t[7:]); val != "" {
				cookies = append(cookies, val)
			}
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-H" || a == "--header":
			if i+1 < len(args) {
				i++
				appendHeaderIfCookie(args[i])
			}
		case strings.HasPrefix(a, "--header="):
			appendHeaderIfCookie(strings.TrimPrefix(a, "--header="))
		case strings.HasPrefix(a, "-H") && len(a) > 2:
			appendHeaderIfCookie(a[2:])
		case a == "-b" || a == "--cookie":
			if i+1 < len(args) {
				i++
				if val := strings.TrimSpace(args[i]); val != "" {
					cookies = append(cookies, val)
				}
			}
		case strings.HasPrefix(a, "--cookie="):
			if val := strings.TrimSpace(strings.TrimPrefix(a, "--cookie=")); val != "" {
				cookies = append(cookies, val)
			}
		case strings.HasPrefix(a, "-b") && len(a) > 2:
			if val := strings.TrimSpace(a[2:]); val != "" {
				cookies = append(cookies, val)
			}
		}
	}
	return strings.Join(cookies, "; ")
}

func parseTSVCookies(raw string) []domainCookie {
	var out []domainCookie
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) >= 10 && strings.EqualFold(line[:10], "#HttpOnly_") {
			line = line[10:]
		} else if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.Contains(line, "\t") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) >= 7 && essentialSet[strings.TrimSpace(fields[5])] {
			exp, _ := strconv.ParseFloat(strings.TrimSpace(fields[4]), 64)
			out = append(out, domainCookie{
				Name:           strings.TrimSpace(fields[5]),
				Value:          strings.TrimSpace(fields[6]),
				Domain:         strings.TrimSpace(fields[0]),
				Path:           strings.TrimSpace(fields[2]),
				ExpirationDate: exp,
			})
			continue
		}
		if len(fields) >= 2 && essentialSet[strings.TrimSpace(fields[0])] {
			dom := ""
			if len(fields) >= 3 {
				dom = strings.TrimSpace(fields[2])
			}
			path := ""
			if len(fields) >= 4 {
				path = strings.TrimSpace(fields[3])
			}
			var exp float64
			if len(fields) >= 5 {
				expStr := strings.TrimSpace(fields[4])
				if expStr != "" && !strings.EqualFold(expStr, "Session") {
					if t, err := time.Parse(time.RFC3339Nano, expStr); err == nil {
						exp = float64(t.Unix())
					} else if t, err := time.Parse(time.RFC3339, expStr); err == nil {
						exp = float64(t.Unix())
					}
				}
			}
			out = append(out, domainCookie{
				Name:           strings.TrimSpace(fields[0]),
				Value:          strings.TrimSpace(fields[1]),
				Domain:         dom,
				Path:           path,
				ExpirationDate: exp,
			})
		}
	}
	return out
}

func selectBestDomainCookies(list []domainCookie) string {
	_, hdr := selectBestDomainCookieMap(list)
	return hdr
}

func isGoogleCookieDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" {
		return false
	}
	if strings.HasPrefix(d, ".") {
		d = d[1:]
	}
	return d == "google.com" || (strings.HasSuffix(d, ".google.com") && !strings.HasPrefix(d, ".") && !strings.Contains(d, ".."))
}

func cookieDomainScore(name, domain string) int {
	if !isGoogleCookieDomain(domain) {
		return 0
	}
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	switch name {
	case "OSID", "__Secure-OSID":
		if d == "gemini.google.com" {
			return 4
		}
		if d == "google.com" {
			return 3
		}
		return 2
	case "LSID", "__Host-1PLSID", "__Host-3PLSID", "__Host-GAPS", "ACCOUNT_CHOOSER":
		if d == "accounts.google.com" {
			return 4
		}
		if d == "google.com" {
			return 3
		}
		return 2
	default:
		if d == "google.com" {
			return 3
		}
		return 2
	}
}

func hasEssentialCookieUpdate(setCookies []string) bool {
	if len(setCookies) == 0 {
		return false
	}
	now := time.Now()
	for _, raw := range setCookies {
		head := raw
		attrs := ""
		if semi := strings.IndexByte(raw, ';'); semi >= 0 {
			head = raw[:semi]
			attrs = raw[semi+1:]
		}
		i := strings.IndexByte(head, '=')
		if i <= 0 {
			continue
		}
		name := strings.TrimSpace(head[:i])
		val := strings.TrimSpace(head[i+1:])
		if !essentialSet[name] {
			continue
		}
		if !isCookieDeletion(val, attrs, now) {
			return true
		}
	}
	return false
}

func selectBestDomainCookieMap(list []domainCookie) (map[string]string, string) {
	best := make(map[string]string)
	bestScore := make(map[string]int)
	bestPathScore := make(map[string]int)
	now := time.Now()
	for _, c := range list {
		name := strings.TrimSpace(c.Name)
		val := strings.TrimSpace(c.Value)
		if !essentialSet[name] || val == "" || strings.EqualFold(val, "deleted") || strings.EqualFold(val, `""`) {
			continue
		}
		exp := c.ExpirationDate
		if exp <= 0 {
			exp = c.Expiry
		}
		if isExpiredEpoch(exp, now) {
			continue
		}
		dom := resolveCookieDomain(c)
		s := 1
		if dom != "" {
			s = cookieDomainScore(name, dom)
			if s == 0 {
				continue
			}
		}
		ps := 0
		if p := strings.TrimSpace(c.Path); p == "" || p == "/" {
			ps = 1
		}
		if s > bestScore[name] || (s == bestScore[name] && ps >= bestPathScore[name]) {
			best[name] = val
			bestScore[name] = s
			bestPathScore[name] = ps
		}
	}
	return best, CanonicalHeader(best)
}

// SaveCredentialFile atomically writes a CredentialFile to path with mode 0600.
func SaveCredentialFile(path string, cred CredentialFile) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	if prev, _, err := LoadAccountFile(path); err == nil && prev != nil {
		if cred.AT == "" {
			cred.AT = prev.AT
		}
		if cred.BL == "" {
			cred.BL = prev.BL
		}
		if cred.FSid == "" {
			cred.FSid = prev.FSid
		}
		if cred.AuthUser == 0 && prev.AuthUser > 0 {
			cred.AuthUser = prev.AuthUser
		}
		if cred.Email == "" {
			cred.Email = prev.Email
		}
		if cred.CapturedAt == "" && prev.CapturedAt != "" {
			cred.CapturedAt = prev.CapturedAt
		}
		if cred.CookieDB == "" {
			cred.CookieDB = prev.CookieDB
		}
		if cred.ImportedFrom == "" {
			cred.ImportedFrom = prev.ImportedFrom
		}
		if cred.ProfileID == "" {
			cred.ProfileID = prev.ProfileID
		}
		if cred.TokenFetchedAt == "" {
			cred.TokenFetchedAt = prev.TokenFetchedAt
		}
		if cred.TokenSource == "" {
			cred.TokenSource = prev.TokenSource
		}
		if cred.LastReqID == 0 {
			cred.LastReqID = prev.LastReqID
		}
		if !cred.Quarantined && prev.Quarantined && cred.QuarantineReason == "" {
			// Preserve quarantine unless caller explicitly cleared or updated Surfaces["geminiWeb"] == "ok"
			if cred.Surfaces == nil || cred.Surfaces["geminiWeb"] != "ok" {
				cred.Quarantined = prev.Quarantined
				cred.QuarantineReason = prev.QuarantineReason
			}
		}
		if cred.AccountGroup == "" {
			cred.AccountGroup = prev.AccountGroup
		}
		if cred.ProxyURL == "" {
			cred.ProxyURL = prev.ProxyURL
		}
		if len(prev.Surfaces) > 0 {
			if cred.Surfaces == nil {
				cred.Surfaces = make(map[string]string, len(prev.Surfaces))
			}
			for k, v := range prev.Surfaces {
				if _, ok := cred.Surfaces[k]; !ok {
					cred.Surfaces[k] = v
				}
			}
		}
	}
	if cred.Kind == "" {
		cred.Kind = "gemini-web"
	}
	nowISO := time.Now().UTC().Format(time.RFC3339)
	if cred.CapturedAt == "" {
		cred.CapturedAt = nowISO
	}
	if cred.RotatedAt == "" {
		cred.RotatedAt = nowISO
	}
	data, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return 0, err
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	_ = os.Chmod(path, 0o600)
	if st, err := os.Stat(path); err == nil {
		return st.ModTime().UnixMilli(), nil
	}
	return time.Now().UnixMilli(), nil
}

// LoadAccountFile reads and validates a CredentialFile from disk, returning the
// parsed record and file modification time in milliseconds.
func LoadAccountFile(path string) (*CredentialFile, int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	var cf CredentialFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		return nil, 0, err
	}
	if cf.Name == "" {
		base := filepath.Base(path)
		cf.Name = strings.TrimSuffix(base, filepath.Ext(base))
	}
	return &cf, st.ModTime().UnixMilli(), nil
}
