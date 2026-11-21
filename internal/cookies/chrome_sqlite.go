package cookies

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
	_ "modernc.org/sqlite"
)

// BrowserProfiles maps well-known Linux browser names to their default Cookie DB paths.
var BrowserProfiles = map[string]string{
	"chrome":   ".config/google-chrome/Default/Cookies",
	"chromium": ".config/chromium/Default/Cookies",
	"brave":    ".config/BraveSoftware/Brave-Browser/Default/Cookies",
	"edge":     ".config/microsoft-edge/Default/Cookies",
	"vivaldi":  ".config/vivaldi/Default/Cookies",
}

var (
	keyringApps   = []string{"chrome", "brave", "chromium", "microsoft-edge", "vivaldi", "opera", "arc"}
	knownBrowsers = []string{"chrome", "brave", "chromium", "edge", "vivaldi", "arc"}
	// dpapiUnprotect decrypts a Windows DPAPI blob; overridable in tests.
	dpapiUnprotect = platformDPAPIUnprotect
)

// macSafeStorageServices maps a browser identifier to the macOS Keychain
// generic-password service that holds its cookie-encryption secret. Chromium
// forks all store the key under "<Product> Safe Storage"; reading it requires
// the user to approve the Keychain prompt once.
var macSafeStorageServices = map[string]string{
	"chrome":   "Chrome Safe Storage",
	"chromium": "Chromium Safe Storage",
	"brave":    "Brave Safe Storage",
	"edge":     "Microsoft Edge Safe Storage",
	"vivaldi":  "Vivaldi Safe Storage",
	"opera":    "Opera Safe Storage",
	"arc":      "Arc Safe Storage",
}

// safeStorageService returns the macOS Keychain service name for a browser.
func safeStorageService(browser string) (string, bool) {
	svc, ok := macSafeStorageServices[strings.ToLower(strings.TrimSpace(browser))]
	return svc, ok
}

// CookieRow represents a decrypted cookie from a browser SQLite database.
type CookieRow struct {
	Host      string
	Name      string
	Value     string
	ExpiresAt int64
}

// LongLivedSessionCookies are the Google cookies that indicate whether a
// browser profile was actually signed in. Short-lived rotating cookies such as
// __Secure-1PSIDTS are deliberately excluded so routine rotation never triggers
// a freshness warning.
var LongLivedSessionCookies = []string{
	"SID", "HSID", "SSID", "APISID", "SAPISID",
	"__Secure-1PSID", "__Secure-3PSID",
}

// ExpiringCookies returns the long-lived session cookies in res that are already
// expired or expire within the given window. An empty result means the export
// looks usable for at least that long.
func ExpiringCookies(res *ReadResult, within time.Duration) []CookieRow {
	if res == nil {
		return nil
	}
	wanted := make(map[string]bool, len(LongLivedSessionCookies))
	for _, n := range LongLivedSessionCookies {
		wanted[n] = true
	}
	cutoff := time.Now().Add(within).Unix()
	var out []CookieRow
	for _, c := range res.Cookies {
		if !wanted[c.Name] || c.ExpiresAt <= 0 {
			continue
		}
		if c.ExpiresAt <= cutoff {
			out = append(out, c)
		}
	}
	return out
}

// SessionCookieHint summarizes why a cookie export is likely to fail, or returns
// an empty string when the long-lived session cookies look healthy.
func SessionCookieHint(res *ReadResult, within time.Duration) string {
	expiring := ExpiringCookies(res, within)
	if len(expiring) == 0 {
		return ""
	}
	now := time.Now()
	var expired, soon []string
	for _, c := range expiring {
		if c.ExpiresAt <= now.Unix() {
			expired = append(expired, c.Name)
			continue
		}
		soon = append(soon, c.Name)
	}
	switch {
	case len(expired) > 0 && len(soon) > 0:
		return fmt.Sprintf("%s expired and %s expire soon", strings.Join(expired, ", "), strings.Join(soon, ", "))
	case len(expired) > 0:
		return fmt.Sprintf("%s expired", strings.Join(expired, ", "))
	default:
		return fmt.Sprintf("%s expire soon", strings.Join(soon, ", "))
	}
}

// ReadResult summarizes a browser cookie DB extraction.
type ReadResult struct {
	Cookies     []CookieRow
	Header      string
	Decrypted   int
	Plain       int
	Failed      int
	DomainBound int
	KeyFound    bool
}

func isKnownBrowser(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "chrome", "chromium", "brave", "edge", "vivaldi", "arc":
		return true
	default:
		return false
	}
}

func expandPercentEnv(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		relStart := strings.IndexByte(s[i:], '%')
		if relStart < 0 {
			b.WriteString(s[i:])
			break
		}
		start := i + relStart
		b.WriteString(s[i:start])
		relEnd := strings.IndexByte(s[start+1:], '%')
		if relEnd < 0 {
			b.WriteString(s[start:])
			break
		}
		end := start + 1 + relEnd
		key := s[start+1 : end]
		if key != "" {
			if val := os.Getenv(key); val != "" {
				b.WriteString(val)
				i = end + 1
				continue
			}
		}
		b.WriteString(s[start : end+1])
		i = end + 1
	}
	return b.String()
}

func normalizeInputPath(raw string) string {
	p := strings.TrimSpace(raw)
	p = strings.Trim(p, `"'`)
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = expandPercentEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}
	for len(p) > 1 && (strings.HasSuffix(p, "/") || strings.HasSuffix(p, `\`)) {
		if len(p) == 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
			break
		}
		p = p[:len(p)-1]
	}
	return p
}

func resolveCookiePath(rawPath string) (string, bool) {
	candidates := []string{rawPath}
	if os.PathSeparator == '/' && strings.Contains(rawPath, `\`) {
		candidates = append(candidates, strings.ReplaceAll(rawPath, `\`, "/"))
	} else if os.PathSeparator == '\\' && strings.Contains(rawPath, "/") {
		candidates = append(candidates, strings.ReplaceAll(rawPath, "/", `\`))
	}
	for _, p := range candidates {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !st.IsDir() {
			return p, true
		}
		relCandidates := []string{
			filepath.Join(p, "Network", "Cookies"),
			filepath.Join(p, "Cookies"),
			filepath.Join(p, "cookies.sqlite"),
			filepath.Join(p, "Default", "Network", "Cookies"),
			filepath.Join(p, "Default", "Cookies"),
		}
		for _, c := range relCandidates {
			if cst, err := os.Stat(c); err == nil && !cst.IsDir() {
				return c, true
			}
		}
	}
	return "", false
}

func isPathLike(s string) bool {
	if strings.ContainsAny(s, `/\`) || strings.HasSuffix(s, "Cookies") || strings.HasSuffix(strings.ToLower(s), ".sqlite") {
		return true
	}
	if len(s) >= 2 && s[1] == ':' && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) {
		return true
	}
	return false
}

func browserUserDataDirs(browser string) []string {
	b := strings.ToLower(strings.TrimSpace(browser))
	home, _ := os.UserHomeDir()
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" && home != "" {
		localAppData = filepath.Join(home, "AppData", "Local")
	}
	var dirs []string
	addWin := func() {
		if localAppData == "" {
			return
		}
		switch b {
		case "chrome":
			dirs = append(dirs, filepath.Join(localAppData, "Google", "Chrome", "User Data"))
		case "chromium":
			dirs = append(dirs, filepath.Join(localAppData, "Chromium", "User Data"))
		case "brave":
			dirs = append(dirs, filepath.Join(localAppData, "BraveSoftware", "Brave-Browser", "User Data"))
		case "edge":
			dirs = append(dirs, filepath.Join(localAppData, "Microsoft", "Edge", "User Data"))
		case "vivaldi":
			dirs = append(dirs, filepath.Join(localAppData, "Vivaldi", "User Data"))
		case "arc":
			// Arc ships as an MSIX package, so the profile lives under a
			// versioned package directory that has to be globbed.
			matches, _ := filepath.Glob(filepath.Join(localAppData, "Packages", "TheBrowserCompany.Arc_*", "LocalCache", "Local", "Arc", "User Data"))
			dirs = append(dirs, matches...)
			dirs = append(dirs, filepath.Join(localAppData, "Arc", "User Data"))
		}
	}
	addMac := func() {
		if home == "" {
			return
		}
		appSupport := filepath.Join(home, "Library", "Application Support")
		switch b {
		case "chrome":
			dirs = append(dirs, filepath.Join(appSupport, "Google", "Chrome"))
		case "chromium":
			dirs = append(dirs, filepath.Join(appSupport, "Chromium"))
		case "brave":
			dirs = append(dirs, filepath.Join(appSupport, "BraveSoftware", "Brave-Browser"))
		case "edge":
			dirs = append(dirs, filepath.Join(appSupport, "Microsoft Edge"))
		case "vivaldi":
			dirs = append(dirs, filepath.Join(appSupport, "Vivaldi"))
		case "arc":
			dirs = append(dirs, filepath.Join(appSupport, "Arc", "User Data"))
		}
	}
	addLinux := func() {
		if home == "" {
			return
		}
		cfgDir := filepath.Join(home, ".config")
		switch b {
		case "chrome":
			dirs = append(dirs, filepath.Join(cfgDir, "google-chrome"))
		case "chromium":
			dirs = append(dirs, filepath.Join(cfgDir, "chromium"))
		case "brave":
			dirs = append(dirs, filepath.Join(cfgDir, "BraveSoftware", "Brave-Browser"))
		case "edge":
			dirs = append(dirs, filepath.Join(cfgDir, "microsoft-edge"))
		case "vivaldi":
			dirs = append(dirs, filepath.Join(cfgDir, "vivaldi"))
		}
	}

	switch runtime.GOOS {
	case "windows":
		addWin()
		addLinux()
		addMac()
	case "darwin":
		addMac()
		addLinux()
		addWin()
	default:
		addLinux()
		addWin()
		addMac()
	}
	return dirs
}

// ParseProfileHint extracts a target browser name and profile directory name (e.g. "Profile 4")
// from a --chrome-profile / --cookie-db argument so interactive login can launch the right profile.
func ParseProfileHint(nameOrPath string) (browser, profileDir string) {
	p := normalizeInputPath(nameOrPath)
	if p == "" {
		return "", ""
	}
	lower := strings.ToLower(p)
	if isKnownBrowser(lower) {
		return lower, "Default"
	}
	if idx := strings.IndexByte(p, ':'); idx > 1 && isKnownBrowser(p[:idx]) {
		return strings.ToLower(strings.TrimSpace(p[:idx])), strings.TrimSpace(p[idx+1:])
	}
	switch {
	case strings.Contains(lower, "brave"):
		browser = "brave"
	case strings.Contains(lower, "edge"):
		browser = "edge"
	case strings.Contains(lower, "vivaldi"):
		browser = "vivaldi"
	case strings.Contains(lower, "chromium"):
		browser = "chromium"
	case strings.Contains(lower, "chrome"):
		browser = "chrome"
	}
	if browser == "" {
		browser = browserFromDBPath(p)
	}
	if !strings.ContainsAny(p, `/\`) {
		return browser, p
	}
	norm := strings.ReplaceAll(p, `\`, "/")
	parts := strings.Split(strings.TrimRight(norm, "/"), "/")
	for len(parts) > 0 {
		last := strings.ToLower(parts[len(parts)-1])
		if last == "cookies" || last == "network" || strings.HasSuffix(last, ".sqlite") {
			parts = parts[:len(parts)-1]
			continue
		}
		break
	}
	if len(parts) == 0 {
		return browser, ""
	}
	last := parts[len(parts)-1]
	lastLower := strings.ToLower(last)
	if lastLower == "default" || strings.HasPrefix(lastLower, "profile ") || strings.HasPrefix(lastLower, "profile_") {
		return browser, last
	}
	if len(parts) >= 2 {
		parentLower := strings.ToLower(parts[len(parts)-2])
		if parentLower == "user data" || parentLower == "google-chrome" || parentLower == "chromium" || parentLower == "brave-browser" || parentLower == "microsoft-edge" || parentLower == "vivaldi" {
			return browser, last
		}
	}
	return browser, ""
}

// FindBrowserCookieDB resolves a browser name, profile name, profile directory, or file path
// to an existing SQLite Cookie DB.
func FindBrowserCookieDB(nameOrPath string) (string, error) {
	p := normalizeInputPath(nameOrPath)
	if p == "" {
		return "", fmt.Errorf("empty browser or cookie database path")
	}
	if resolved, ok := resolveCookiePath(p); ok {
		return resolved, nil
	}
	if isPathLike(p) {
		return "", fmt.Errorf("cookie database not found at %s", nameOrPath)
	}
	if idx := strings.IndexByte(p, ':'); idx > 1 && isKnownBrowser(p[:idx]) {
		bName := strings.ToLower(strings.TrimSpace(p[:idx]))
		profName := strings.TrimSpace(p[idx+1:])
		if profName != "" {
			for _, ud := range browserUserDataDirs(bName) {
				if resolved, ok := resolveCookiePath(filepath.Join(ud, profName)); ok {
					return resolved, nil
				}
			}
			return "", fmt.Errorf("no cookie database found for %s profile %q", bName, profName)
		}
	}
	lower := strings.ToLower(p)
	home, _ := os.UserHomeDir()
	rel, inMap := BrowserProfiles[lower]
	if isKnownBrowser(lower) || inMap {
		for _, ud := range browserUserDataDirs(lower) {
			if resolved, ok := resolveCookiePath(filepath.Join(ud, "Default")); ok {
				return resolved, nil
			}
			if resolved, ok := resolveCookiePath(ud); ok {
				return resolved, nil
			}
		}
		if inMap && home != "" {
			full := filepath.Join(home, rel)
			if resolved, ok := resolveCookiePath(full); ok {
				return resolved, nil
			}
			return "", fmt.Errorf("no cookie database found for %s at %s", nameOrPath, full)
		}
		return "", fmt.Errorf("no cookie database found for %s", nameOrPath)
	}
	// Check if nameOrPath is a profile directory name (e.g. "Profile 4", "Default") under any known browser.
	for _, bName := range knownBrowsers {
		for _, ud := range browserUserDataDirs(bName) {
			if resolved, ok := resolveCookiePath(filepath.Join(ud, p)); ok {
				return resolved, nil
			}
		}
	}
	return "", fmt.Errorf("unknown browser %q (known: chrome, chromium, brave, edge, vivaldi, arc; or pass a profile directory/path)", nameOrPath)
}

// ReadBrowserCookies reads and decrypts Google session cookies from a Chrome/Chromium
// or Firefox SQLite database without locking the browser's live file.
func ReadBrowserCookies(dbPath string) (*ReadResult, error) {
	tmpDir := filepath.Join(os.TempDir(), fmt.Sprintf("gsb-ck-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	tmpDB := filepath.Join(tmpDir, "Cookies.db")
	if err := copyFile(dbPath, tmpDB); err != nil {
		return nil, fmt.Errorf("copy cookie db: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suffix); err == nil {
			_ = copyFile(dbPath+suffix, tmpDB+suffix)
		}
	}

	db, err := sql.Open("sqlite", tmpDB)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if strings.HasSuffix(strings.ToLower(dbPath), ".sqlite") || strings.Contains(strings.ToLower(dbPath), "firefox") {
		return readFirefoxCookies(db)
	}
	return readChromiumCookies(db, dbPath)
}

func readFirefoxCookies(db *sql.DB) (*ReadResult, error) {
	rows, err := db.Query(`SELECT host, name, value, COALESCE(expiry, 0) FROM moz_cookies WHERE host LIKE '%google.com' OR host LIKE '%gemini.google%'`)
	if err != nil {
		// Fallback if table has minimal columns
		rows, err = db.Query(`SELECT host, name, value, 0 FROM moz_cookies WHERE host LIKE '%google.com' OR host LIKE '%gemini.google%'`)
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()

	nowUnix := time.Now().Unix()
	res := &ReadResult{KeyFound: true}
	for rows.Next() {
		var host, name, value string
		var expiry int64
		if err := rows.Scan(&host, &name, &value, &expiry); err != nil {
			continue
		}
		if expiry > 0 && expiry <= nowUnix {
			continue
		}
		res.Plain++
		res.Cookies = append(res.Cookies, CookieRow{Host: host, Name: name, Value: value, ExpiresAt: expiry})
	}
	res.Header = bestHeaderFromRows(res.Cookies)
	return res, nil
}

type rawChromiumRow struct {
	host         string
	name         string
	encrypted    []byte
	plain        string
	hasExpires   int64
	expiresUTC   int64
	isPersistent int64
	expiresUnix  int64
}

const chromiumEpochOffsetSec int64 = 11644473600

func readChromiumCookies(db *sql.DB, dbPath string) (*ReadResult, error) {
	hasExpiryCols := true
	rows, err := db.Query(`SELECT host_key, name, encrypted_value, value, COALESCE(has_expires, 0), COALESCE(expires_utc, 0), COALESCE(is_persistent, 1) FROM cookies WHERE host_key LIKE '%google%'`)
	if err != nil {
		hasExpiryCols = false
		rows, err = db.Query(`SELECT host_key, name, encrypted_value, value FROM cookies WHERE host_key LIKE '%google%'`)
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()

	nowUnix := time.Now().Unix()
	var rawRows []rawChromiumRow
	for rows.Next() {
		var r rawChromiumRow
		if hasExpiryCols {
			if err := rows.Scan(&r.host, &r.name, &r.encrypted, &r.plain, &r.hasExpires, &r.expiresUTC, &r.isPersistent); err != nil {
				continue
			}
			if r.hasExpires != 0 && r.expiresUTC > 0 {
				unixSec := (r.expiresUTC / 1_000_000) - chromiumEpochOffsetSec
				if unixSec <= nowUnix {
					continue
				}
				r.expiresUnix = unixSec
			}
		} else {
			if err := rows.Scan(&r.host, &r.name, &r.encrypted, &r.plain); err != nil {
				continue
			}
		}
		rawRows = append(rawRows, r)
	}

	key256, key128, keyFound := detectBestKey(rawRows, dbPath)
	res := &ReadResult{KeyFound: keyFound}
	for _, r := range rawRows {
		val := r.plain
		if len(r.encrypted) > 0 {
			if !keyFound {
				res.Failed++
				continue
			}
			dec := decryptCookieBytesForHost(r.encrypted, key256, key128, r.host)
			if dec == nil {
				res.Failed++
				continue
			}
			stripped, bound := stripDomainPrefix(dec, r.host)
			if bound {
				res.DomainBound++
			}
			val = string(stripped)
			res.Decrypted++
		} else {
			res.Plain++
		}
		res.Cookies = append(res.Cookies, CookieRow{Host: r.host, Name: r.name, Value: val, ExpiresAt: r.expiresUnix})
	}
	res.Header = bestHeaderFromRows(res.Cookies)
	return res, nil
}

type localStateFile struct {
	OSCrypt struct {
		EncryptedKey string `json:"encrypted_key"`
	} `json:"os_crypt"`
}

func extractLocalStateDPAPIBlob(localStatePath string) ([]byte, error) {
	data, err := os.ReadFile(localStatePath)
	if err != nil {
		return nil, err
	}
	var ls localStateFile
	if err := json.Unmarshal(data, &ls); err != nil {
		return nil, err
	}
	if ls.OSCrypt.EncryptedKey == "" {
		return nil, fmt.Errorf("missing os_crypt.encrypted_key in %s", localStatePath)
	}
	raw, err := base64.StdEncoding.DecodeString(ls.OSCrypt.EncryptedKey)
	if err != nil {
		return nil, err
	}
	if len(raw) <= 5 || string(raw[:5]) != "DPAPI" {
		return nil, fmt.Errorf("unexpected encrypted_key prefix in %s", localStatePath)
	}
	return raw[5:], nil
}

func readLocalStateMasterKey(localStatePath string) ([]byte, error) {
	blob, err := extractLocalStateDPAPIBlob(localStatePath)
	if err != nil {
		return nil, err
	}
	return dpapiUnprotect(blob)
}

func findLocalStatePaths(dbPath string) []string {
	var out []string
	seen := make(map[string]bool)
	addIfFile := func(p string) {
		if p == "" || seen[p] {
			return
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			seen[p] = true
			out = append(out, p)
		}
	}
	if dbPath != "" {
		dir := filepath.Dir(dbPath)
		for i := 0; i < 4 && dir != ""; i++ {
			addIfFile(filepath.Join(dir, "Local State"))
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	for _, bName := range knownBrowsers {
		for _, ud := range browserUserDataDirs(bName) {
			addIfFile(filepath.Join(ud, "Local State"))
		}
	}
	return out
}

// browserFromDBPath infers which browser owns a cookie database from its path.
// It is used to pick the right macOS Keychain service without prompting the
// user for every installed browser.
func browserFromDBPath(dbPath string) string {
	// Backslashes are normalised explicitly: filepath.ToSlash is a no-op for
	// Windows-style paths on non-Windows hosts, and callers may pass either.
	s := strings.ReplaceAll(strings.ToLower(dbPath), `\`, "/")
	s = "/" + strings.TrimPrefix(s, "/")
	has := func(marker string) bool { return strings.Contains(s, marker) }
	switch {
	// Arc ships as "TheBrowserCompany.Arc_*" on Windows and stores its profile
	// under "Application Support/Arc/User Data" on macOS.
	case has("/thebrowsercompany.arc_"), has("/application support/arc/"), has("/local/arc/"):
		return "arc"
	case has("/bravesoftware/"), has("/brave-browser/"):
		return "brave"
	case has("/microsoft edge/"), has("/microsoft-edge/"):
		return "edge"
	case has("/vivaldi/"):
		return "vivaldi"
	case has("/opera software/"), has("/opera/"):
		return "opera"
	case has("/chromium/"):
		return "chromium"
	case has("/google/chrome/"), has("/google-chrome/"):
		return "chrome"
	}
	return ""
}

// keychainServicesFor returns the macOS Keychain services worth trying for a
// cookie database. A recognised profile only queries its own browser; an
// unrecognised path falls back to every known Chromium fork.
func keychainServicesFor(dbPath string) []string {
	if b := browserFromDBPath(dbPath); b != "" {
		if svc, ok := safeStorageService(b); ok {
			return []string{svc}
		}
	}
	out := make([]string, 0, len(knownBrowsers))
	for _, b := range knownBrowsers {
		if svc, ok := safeStorageService(b); ok {
			out = append(out, svc)
		}
	}
	return out
}

var keyringCmdTimeout = 10 * time.Second

func runKeyringCmd(name string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), keyringCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 500 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		return "", errors.Is(ctx.Err(), context.DeadlineExceeded)
	}
	return strings.TrimSpace(string(out)), false
}

// keychainPassword reads a macOS Keychain generic password. An empty result
// means the item is absent or the user declined the access prompt.
func keychainPassword(service string) string {
	out, _ := runKeyringCmd("security", "find-generic-password", "-s", service, "-w")
	return out
}

func candidatePasswords(dbPath string) []string {
	var passes []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		passes = append(passes, p)
	}
	if envPass := os.Getenv("CHROME_PASS"); envPass != "" {
		add(envPass)
	} else {
		switch runtime.GOOS {
		case "darwin":
			// Chromium forks keep their cookie key in the login Keychain under
			// "<Product> Safe Storage"; macOS prompts once per item.
			for _, svc := range keychainServicesFor(dbPath) {
				out, timedOut := runKeyringCmd("security", "find-generic-password", "-s", svc, "-w")
				add(out)
				if timedOut {
					break
				}
			}
		case "windows":
			// The key is DPAPI-wrapped inside "Local State"; see detectBestKey.
		default:
			for _, app := range keyringApps {
				out, timedOut := runKeyringCmd("secret-tool", "lookup", "application", app)
				add(out)
				if timedOut {
					break
				}
			}
		}
	}
	add("peanuts")
	passes = append(passes, "")
	return passes
}

type keyCandidate struct {
	k256 []byte
	k128 []byte
}

func detectBestKey(rows []rawChromiumRow, dbPath string) (best256, best128 []byte, ok bool) {
	var candidates []keyCandidate
	localStateKeyFound := false
	for _, lsPath := range findLocalStatePaths(dbPath) {
		if k, err := readLocalStateMasterKey(lsPath); err == nil && len(k) == 32 {
			candidates = append(candidates, keyCandidate{k256: k})
			localStateKeyFound = true
		}
	}
	passes := candidatePasswords(dbPath)
	for _, p := range passes {
		k256 := pbkdf2.Key([]byte(p), []byte("saltysalt"), 1, 32, sha1.New)
		k128 := pbkdf2.Key([]byte(p), []byte("saltysalt"), 1, 16, sha1.New)
		candidates = append(candidates, keyCandidate{k256: k256, k128: k128})
		if runtime.GOOS == "darwin" && p != "" {
			k128Mac := pbkdf2.Key([]byte(p), []byte("saltysalt"), 1003, 16, sha1.New)
			candidates = append(candidates, keyCandidate{k128: k128Mac})
		}
	}

	encryptedRows := 0
	for _, r := range rows {
		if len(r.encrypted) > 0 {
			encryptedRows++
		}
	}
	if encryptedRows == 0 {
		return nil, nil, true
	}

	bestScore := 0
	for _, cand := range candidates {
		score := 0
		for _, r := range rows {
			if len(r.encrypted) == 0 {
				continue
			}
			if decryptCookieBytesForHost(r.encrypted, cand.k256, cand.k128, r.host) != nil {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			best256 = cand.k256
			best128 = cand.k128
			ok = true
			if score == encryptedRows {
				break
			}
		}
	}
	if !ok && localStateKeyFound && len(candidates) > 0 {
		return candidates[0].k256, nil, true
	}
	return best256, best128, ok
}

func isValidCookieOctets(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		b = b[1 : len(b)-1]
		if len(b) == 0 {
			return false
		}
	}
	for _, ch := range b {
		if ch == 0x21 ||
			(ch >= 0x23 && ch <= 0x2b) ||
			(ch >= 0x2d && ch <= 0x3a) ||
			(ch >= 0x3c && ch <= 0x5b) ||
			(ch >= 0x5d && ch <= 0x7e) {
			continue
		}
		return false
	}
	return true
}

func decryptCookieBytes(buf, key256, key128 []byte) []byte {
	return decryptCookieBytesForHost(buf, key256, key128, "")
}

func decryptCookieBytesForHost(buf, key256, key128 []byte, host string) []byte {
	if len(buf) < 4 {
		return nil
	}
	prefix := string(buf[:3])
	if prefix != "v10" && prefix != "v11" {
		if plain, err := dpapiUnprotect(buf); err == nil && len(plain) > 0 {
			return plain
		}
		return nil
	}
	rest := buf[3:]
	// Try AES-256-GCM (Chrome / Brave modern Linux keyring)
	if len(rest) >= 12+16 && len(key256) == 32 {
		if block, err := aes.NewCipher(key256); err == nil {
			if gcm, err := cipher.NewGCM(block); err == nil {
				nonce := rest[:12]
				ctAndTag := rest[12:]
				if plain, err := gcm.Open(nil, nonce, ctAndTag, nil); err == nil {
					return plain
				}
			}
		}
	}
	// Try AES-128-CBC (Chromium / v10 legacy)
	if len(rest) >= aes.BlockSize && len(rest)%aes.BlockSize == 0 && len(key128) == 16 {
		if block, err := aes.NewCipher(key128); err == nil {
			iv := bytes.Repeat([]byte{0x20}, aes.BlockSize)
			out := make([]byte, len(rest))
			cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, rest)
			pad := int(out[len(out)-1])
			if pad > 0 && pad <= aes.BlockSize && pad <= len(out) {
				validPad := true
				for i := len(out) - pad; i < len(out); i++ {
					if int(out[i]) != pad {
						validPad = false
						break
					}
				}
				if validPad {
					plain := out[:len(out)-pad]
					check := plain
					if host != "" {
						check, _ = stripDomainPrefix(plain, host)
					}
					if isValidCookieOctets(check) {
						return plain
					}
				}
			}
		}
	}
	return nil
}

// Chrome 130+ prepends SHA-256(host_key) to the decrypted cookie value.
func stripDomainPrefix(buf []byte, host string) ([]byte, bool) {
	if len(buf) >= 32 {
		sum := sha256.Sum256([]byte(host))
		if bytes.Equal(buf[:32], sum[:]) {
			return buf[32:], true
		}
	}
	return buf, false
}

func bestHeaderFromRows(rows []CookieRow) string {
	score := func(h string) int {
		if h == ".google.com" || h == "google.com" {
			return 3
		}
		if strings.HasSuffix(h, ".google.com") {
			return 2
		}
		return 1
	}
	best := make(map[string]string)
	bestScore := make(map[string]int)
	bestExpiry := make(map[string]int64)
	for _, r := range rows {
		if !essentialSet[r.Name] || r.Value == "" {
			continue
		}
		s := score(r.Host)
		if s > bestScore[r.Name] || (s == bestScore[r.Name] && r.ExpiresAt > bestExpiry[r.Name]) {
			best[r.Name] = r.Value
			bestScore[r.Name] = s
			bestExpiry[r.Name] = r.ExpiresAt
		}
	}
	return CanonicalHeader(best)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// hasSAPISIDCookieRow reports whether the cookie database contains a SAPISID
// row, without decrypting anything. Auto-detection uses it to prefer a browser
// profile that is actually signed in to Google over one that merely exists.
func hasSAPISIDCookieRow(dbPath string) bool {
	if strings.TrimSpace(dbPath) == "" {
		return false
	}
	// immutable=1 keeps the probe lock-free while the browser is running; a
	// stale snapshot is fine for a yes/no presence check.
	dsn := "file:" + filepath.ToSlash(dbPath) + "?mode=ro&immutable=1"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cookies WHERE name = 'SAPISID'`).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// FindChromeCookieDB locates an existing Chromium-family SQLite Cookie DB.
// With no explicit path it scans every known browser and prefers the first
// profile that holds a SAPISID cookie, so a stray unsigned-in Chrome profile
// does not shadow a signed-in Arc/Brave/Edge profile.
func FindChromeCookieDB(explicitPath string) string {
	if strings.TrimSpace(explicitPath) != "" {
		if p, err := FindBrowserCookieDB(explicitPath); err == nil {
			return p
		}
		return ""
	}
	var found []string
	for _, browser := range knownBrowsers {
		if p, err := FindBrowserCookieDB(browser); err == nil {
			if hasSAPISIDCookieRow(p) {
				return p
			}
			found = append(found, p)
		}
	}
	if len(found) > 0 {
		return found[0]
	}
	return ""
}

// DetectActiveProfileLock checks whether the browser profile or User Data directory
// containing dbPath has an active browser lock indicator (e.g. SingletonLock, lockfile,
// parent.lock, .parentlock). Strictly read-only and informational.
func DetectActiveProfileLock(dbPath string) string {
	if strings.TrimSpace(dbPath) == "" {
		return ""
	}
	profDir := filepath.Dir(dbPath)
	if strings.EqualFold(filepath.Base(profDir), "Network") {
		profDir = filepath.Dir(profDir)
	}
	userDataDir := filepath.Dir(profDir)
	for _, dir := range []string{profDir, userDataDir} {
		if dir == "" || dir == "." || dir == "/" {
			continue
		}
		for _, name := range []string{"SingletonLock", "lockfile", "parent.lock", ".parentlock"} {
			cand := filepath.Join(dir, name)
			if _, err := os.Lstat(cand); err == nil {
				return cand
			}
		}
	}
	return ""
}

// ReadChromeCookies reads and decrypts Google cookies from dbPath and returns the canonical Cookie header.
func ReadChromeCookies(dbPath string) (string, error) {
	res, err := ReadBrowserCookies(dbPath)
	if err != nil {
		return "", err
	}
	if res.Header == "" && res.Failed > 0 && res.Decrypted == 0 && res.Plain == 0 {
		hint := "if Chrome uses App-Bound (v20) encryption, export cookies via Cookie-Editor and use --import-cookie"
		if runtime.GOOS == "darwin" {
			svc := "Chrome Safe Storage"
			if b := browserFromDBPath(dbPath); b != "" {
				if s, ok := safeStorageService(b); ok {
					svc = s
				}
			}
			hint = fmt.Sprintf("approve the macOS Keychain prompt for %q (or set CHROME_PASS), otherwise export cookies via Cookie-Editor and use --import-cookie", svc)
		}
		return "", fmt.Errorf("failed to decrypt %d Google cookie(s) in %s (key_found=%v) — %s", res.Failed, dbPath, res.KeyFound, hint)
	}
	return res.Header, nil
}
