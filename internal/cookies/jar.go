package cookies

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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
	"__Secure-1PSID", "__Secure-1PAPISID", "__Secure-3PSID", "__Secure-3PAPISID",
	"__Secure-1PSIDTS", "__Secure-3PSIDTS",
	"SIDCC", "__Secure-1PSIDCC", "__Secure-3PSIDCC", "NID", "AEC",
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

// ParseHeader splits a Cookie header string into ordered key-value pairs.
func ParseHeader(header string) map[string]string {
	out := make(map[string]string, len(EssentialNames))
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if i := strings.IndexByte(part, '='); i > 0 {
			k := strings.TrimSpace(part[:i])
			v := strings.TrimSpace(part[i+1:])
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
			if exp, err := http.ParseTime(av); err == nil && exp.Before(now) {
				return true
			}
			if exp, err := time.Parse("Mon, 02-Jan-2006 15:04:05 MST", av); err == nil && exp.Before(now) {
				return true
			}
			if exp, err := time.Parse("Mon, 02-Jan-06 15:04:05 MST", av); err == nil && exp.Before(now) {
				return true
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

// ParseImportPayload parses either a raw Cookie header string, a browser
// extension JSON array of cookie objects, or a CredentialFile JSON object.
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
			Cookies []struct {
				Name   string `json:"name"`
				Value  string `json:"value"`
				Domain string `json:"domain"`
			} `json:"cookies"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapper); err == nil && len(wrapper.Cookies) > 0 {
			h := selectBestDomainCookies(wrapper.Cookies)
			return h, nil, nil
		}
	}
	if strings.HasPrefix(trimmed, "[") {
		var list []struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Domain string `json:"domain"`
		}
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			return "", nil, fmt.Errorf("invalid cookie JSON array: %w", err)
		}
		return selectBestDomainCookies(list), nil, nil
	}
	m := ParseHeader(trimmed)
	return CanonicalHeader(m), nil, nil
}

func selectBestDomainCookies(list []struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
}) string {
	score := func(d string) int {
		if d == ".google.com" || d == "google.com" {
			return 3
		}
		if strings.HasSuffix(d, ".google.com") {
			return 2
		}
		return 1
	}
	best := make(map[string]string)
	bestScore := make(map[string]int)
	for _, c := range list {
		if !essentialSet[c.Name] || c.Value == "" {
			continue
		}
		s := score(c.Domain)
		if s > bestScore[c.Name] {
			best[c.Name] = c.Value
			bestScore[c.Name] = s
		}
	}
	return CanonicalHeader(best)
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
