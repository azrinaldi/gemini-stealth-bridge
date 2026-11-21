package cookies

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
	fhttp "github.com/bogdanfinn/fhttp"
)

type stubCookieDoer struct {
	fn func(req *fhttp.Request) (*fhttp.Response, error)
}

func (d *stubCookieDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	return d.fn(req)
}

func (d *stubCookieDoer) CloseIdleConnections() {}

func TestAbsorbSetCookieAndSAPISIDHash(t *testing.T) {
	initial := "SID=s1; SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=old_ts"
	merged, changed := AbsorbSetCookie(initial, []string{
		"__Secure-1PSIDTS=new_ts; Path=/; Secure; HttpOnly",
		"UNRELATED_COOKIE=ignore_me; Path=/",
	})
	if !changed {
		t.Fatal("expected changed=true when __Secure-1PSIDTS rotated")
	}
	if !strings.Contains(merged, "__Secure-1PSIDTS=new_ts") || strings.Contains(merged, "UNRELATED_COOKIE") {
		t.Fatalf("unexpected merged header: %s", merged)
	}

	hash := SAPISIDHash(merged, "https://gemini.google.com")
	if !strings.HasPrefix(hash, "SAPISIDHASH ") || len(hash) < 50 {
		t.Fatalf("unexpected SAPISIDHASH: %s", hash)
	}
}

func TestDualPathCookieRotator(t *testing.T) {
	var seenBodies []string
	var seenURLs []string
	doer := &stubCookieDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			seenURLs = append(seenURLs, req.URL.String())
			if req.Body != nil {
				b, _ := io.ReadAll(req.Body)
				seenBodies = append(seenBodies, string(b))
			}
			if strings.Contains(req.URL.Path, "RotateCookiesPage") {
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader(`<script>init('rot_id_999', 1, 540)</script>`)),
				}, nil
			}
			// RotateCookies POST
			lastBody := ""
			if len(seenBodies) > 0 {
				lastBody = seenBodies[len(seenBodies)-1]
			}
			if strings.HasPrefix(lastBody, "[000,") {
				return &fhttp.Response{
					StatusCode: 200,
					Header: fhttp.Header{
						"Set-Cookie": []string{"__Secure-1PSIDTS=fresh_psidts; Path=/; Secure; HttpOnly"},
					},
					Body: io.NopCloser(strings.NewReader("[]")),
				}, nil
			}
			return &fhttp.Response{
				StatusCode: 200,
				Header: fhttp.Header{
					"Set-Cookie": []string{"__Secure-1PSIDCC=fresh_psidcc; Path=/; Secure; HttpOnly"},
				},
				Body: io.NopCloser(strings.NewReader("[]")),
			}, nil
		},
	}

	sc, err := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
	if err != nil {
		t.Fatalf("NewStealthClient: %v", err)
	}

	rot := NewRotator()
	initial := "SAPISID=sap; __Secure-1PSID=psid; __Secure-1PSIDTS=old_ts; __Secure-1PSIDCC=old_cc"
	// Call 1 uses primary [000,"-0000000000000000000"] path
	res, err := rot.RotateAccount(context.Background(), "acct1", initial, sc)
	if err != nil {
		t.Fatalf("RotateAccount primary failed: %v", err)
	}
	if !res.Rotated || res.Status != 200 || !strings.Contains(res.CookieHeader, "__Secure-1PSIDTS=fresh_psidts") {
		t.Fatalf("expected primary PSIDTS rotation, got: %+v", res)
	}
	// Call 2 within 60s returns early without hitting /RotateCookies or fallback 658.
	res2, err := rot.RotateAccount(context.Background(), "acct1", res.CookieHeader, sc)
	if err != nil {
		t.Fatalf("RotateAccount cooldown call failed: %v", err)
	}
	if res2.Rotated || res2.Status != 0 || len(seenBodies) != 1 {
		t.Fatalf("expected cooldown early return with no second POST, got res2=%+v seenBodies=%v", res2, seenBodies)
	}
	// After Forget(), if primary subset is empty, rotation falls back to the 658 iframe path.
	rot.Forget("acct1")
	res3, err := rot.RotateAccount(context.Background(), "acct1", "", sc)
	if err != nil {
		t.Fatalf("RotateAccount fallback failed: %v", err)
	}
	if !res3.Rotated || !strings.Contains(res3.CookieHeader, "__Secure-1PSIDCC=fresh_psidcc") {
		t.Fatalf("expected fallback PSIDCC rotation when primary skipped, got: %+v", res3)
	}
	if len(seenBodies) != 2 || seenBodies[0] != `[000,"-0000000000000000000"]` || seenBodies[1] != `[658,"rot_id_999"]` {
		t.Fatalf("unexpected RotateCookies POST bodies: %v", seenBodies)
	}
}

func TestSaveAndLoadAccountFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "acct.json")
	rec := CredentialFile{
		Name:    "acct",
		Email:   "acct@gmail.com",
		Cookies: "SAPISID=123",
		AT:      "SNlM0e_tok",
		BL:      "boq_123",
	}
	mtime, err := SaveCredentialFile(path, rec)
	if err != nil || mtime <= 0 {
		t.Fatalf("SaveCredentialFile failed: mtime=%d err=%v", mtime, err)
	}
	loaded, mtime2, err := LoadAccountFile(path)
	if err != nil || mtime2 <= 0 {
		t.Fatalf("LoadAccountFile failed: %v", err)
	}
	if loaded.Email != "acct@gmail.com" || loaded.AT != "SNlM0e_tok" {
		t.Fatalf("unexpected loaded record: %+v", loaded)
	}
	st, _ := os.Stat(path)
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600, got %o", st.Mode().Perm())
	}
}

func TestFindBrowserCookieDB_WindowsAndProfileDirs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)

	userData := filepath.Join(tmp, "Google", "Chrome", "User Data")
	prof4 := filepath.Join(userData, "Profile 4")
	netDir := filepath.Join(prof4, "Network")
	if err := os.MkdirAll(netDir, 0o700); err != nil {
		t.Fatal(err)
	}
	netCookies := filepath.Join(netDir, "Cookies")
	if err := os.WriteFile(netCookies, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Profile directory path with Windows backslashes -> resolves to <profile>/Network/Cookies
	winStylePath := strings.ReplaceAll(prof4, "/", `\`)
	got, err := FindBrowserCookieDB(winStylePath)
	if err != nil || got != netCookies {
		t.Fatalf("Windows backslash profile dir: got=%q err=%v, want=%q", got, err, netCookies)
	}

	// 2. Profile name shorthand ("Profile 4" and "chrome:Profile 4")
	got, err = FindBrowserCookieDB("Profile 4")
	if err != nil || got != netCookies {
		t.Fatalf("Profile name shorthand: got=%q err=%v, want=%q", got, err, netCookies)
	}
	got, err = FindBrowserCookieDB("chrome:Profile 4")
	if err != nil || got != netCookies {
		t.Fatalf("Browser:Profile shorthand: got=%q err=%v, want=%q", got, err, netCookies)
	}

	// 3. Fallback to legacy <profile>/Cookies when Network/Cookies is absent
	legacyProf := filepath.Join(userData, "Profile 2")
	if err := os.MkdirAll(legacyProf, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyCookies := filepath.Join(legacyProf, "Cookies")
	if err := os.WriteFile(legacyCookies, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = FindBrowserCookieDB(legacyProf)
	if err != nil || got != legacyCookies {
		t.Fatalf("Legacy profile Cookies fallback: got=%q err=%v, want=%q", got, err, legacyCookies)
	}

	// 4. %LOCALAPPDATA% expansion in path
	got, err = FindBrowserCookieDB(`%LOCALAPPDATA%\Google\Chrome\User Data\Profile 4`)
	if err != nil || got != netCookies {
		t.Fatalf("Percent env expansion: got=%q err=%v, want=%q", got, err, netCookies)
	}
}

func TestReadBrowserCookies_WindowsLocalStateDPAPI(t *testing.T) {
	tmp := t.TempDir()
	userData := filepath.Join(tmp, "Google", "Chrome", "User Data")
	netDir := filepath.Join(userData, "Profile 4", "Network")
	if err := os.MkdirAll(netDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Create a 32-byte master key and wrap it in a simulated DPAPI Local State file.
	masterKey := bytes.Repeat([]byte{0x42}, 32)
	fakeDPAPIBlob := []byte("simulated-dpapi-wrapped-key")
	rawEncryptedKey := append([]byte("DPAPI"), fakeDPAPIBlob...)
	localStateJSON := fmt.Sprintf(`{"os_crypt":{"encrypted_key":%q}}`, base64.StdEncoding.EncodeToString(rawEncryptedKey))
	if err := os.WriteFile(filepath.Join(userData, "Local State"), []byte(localStateJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	origUnprotect := dpapiUnprotect
	dpapiUnprotect = func(blob []byte) ([]byte, error) {
		if !bytes.Equal(blob, fakeDPAPIBlob) {
			return nil, fmt.Errorf("unexpected DPAPI blob: %x", blob)
		}
		return masterKey, nil
	}
	defer func() { dpapiUnprotect = origUnprotect }()

	// Create SQLite Cookies DB with v10 AES-256-GCM + Chrome 130+ SHA-256(host_key) domain prefix.
	dbPath := filepath.Join(netDir, "Cookies")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE cookies (host_key TEXT, name TEXT, encrypted_value BLOB, value TEXT)`); err != nil {
		db.Close()
		t.Fatal(err)
	}

	encryptV10 := func(host, val string) []byte {
		block, err := aes.NewCipher(masterKey)
		if err != nil {
			t.Fatal(err)
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			t.Fatal(err)
		}
		nonce := bytes.Repeat([]byte{0x07}, 12)
		hostHash := sha256.Sum256([]byte(host))
		plain := append(hostHash[:], []byte(val)...)
		ct := gcm.Seal(nil, nonce, plain, nil)
		out := append([]byte("v10"), nonce...)
		return append(out, ct...)
	}

	for _, row := range []struct{ host, name, val string }{
		{".google.com", "SAPISID", "sap_win_123"},
		{".google.com", "__Secure-1PSID", "psid_win_456"},
		{".google.com", "__Secure-1PSIDTS", "psidts_win_789"},
	} {
		if _, err := db.Exec(`INSERT INTO cookies (host_key, name, encrypted_value, value) VALUES (?, ?, ?, '')`,
			row.host, row.name, encryptV10(row.host, row.val)); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	db.Close()

	hdr, err := ReadChromeCookies(dbPath)
	if err != nil {
		t.Fatalf("ReadChromeCookies failed: %v", err)
	}
	if ExtractSAPISID(hdr) != "sap_win_123" || !strings.Contains(hdr, "__Secure-1PSID=psid_win_456") {
		t.Fatalf("unexpected decrypted header: %q", hdr)
	}
}

func TestParseProfileHint(t *testing.T) {
	cases := []struct {
		in          string
		wantBrowser string
		wantProfile string
	}{
		{`C:\Users\alice\AppData\Local\Google\Chrome\User Data\Profile 4`, "chrome", "Profile 4"},
		{`C:\Users\alice\AppData\Local\Google\Chrome\User Data\Profile 4\Network\Cookies`, "chrome", "Profile 4"},
		{`/home/alice/.config/BraveSoftware/Brave-Browser/Profile 2`, "brave", "Profile 2"},
		{`Profile 4`, "", "Profile 4"},
		{`chrome:Profile 4`, "chrome", "Profile 4"},
		{`edge`, "edge", "Default"},
	}
	for _, tc := range cases {
		b, p := ParseProfileHint(tc.in)
		if b != tc.wantBrowser || p != tc.wantProfile {
			t.Errorf("ParseProfileHint(%q) = (%q, %q), want (%q, %q)", tc.in, b, p, tc.wantBrowser, tc.wantProfile)
		}
	}
}

func TestAbsorbSetCookieDeletion(t *testing.T) {
	initial := "SID=s1; SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=old_ts; SIDCC=old_cc; __Secure-3PSIDCC=old_3cc"
	merged, changed := AbsorbSetCookie(initial, []string{
		"__Secure-1PSIDTS=; Max-Age=0; Path=/; Secure; HttpOnly",
		"SIDCC=deleted; Path=/",
		"__Secure-3PSIDCC=stale_val; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Path=/",
	})
	if !changed {
		t.Fatal("expected changed=true on cookie deletion")
	}
	if strings.Contains(merged, "__Secure-1PSIDTS=") || strings.Contains(merged, "SIDCC=") || strings.Contains(merged, "__Secure-3PSIDCC=") {
		t.Fatalf("expected deleted cookies removed from canonical header, got: %s", merged)
	}
	if !strings.Contains(merged, "SID=s1") || !strings.Contains(merged, "SAPISID=sap1") {
		t.Fatalf("expected remaining cookies preserved, got: %s", merged)
	}
}

func TestSQLiteImportFiltersExpired(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "Cookies")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE cookies (host_key TEXT, name TEXT, encrypted_value BLOB, value TEXT, has_expires INTEGER, expires_utc INTEGER, is_persistent INTEGER)`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}

	pastUnix := time.Now().Add(-2 * time.Hour).Unix()
	futureUnix := time.Now().Add(24 * time.Hour).Unix()
	pastChromium := (pastUnix + chromiumEpochOffsetSec) * 1_000_000
	futureChromium := (futureUnix + chromiumEpochOffsetSec) * 1_000_000

	// Insert expired SID and valid SID + valid SAPISID + valid __Secure-1PSID
	_, err = db.Exec(`INSERT INTO cookies VALUES
		('.google.com', 'SID', X'', 'expired_sid', 1, ?, 1),
		('.google.com', 'SID', X'', 'valid_sid', 1, ?, 1),
		('.google.com', 'SAPISID', X'', 'valid_sap', 1, ?, 1),
		('.google.com', '__Secure-1PSID', X'', 'valid_psid', 0, 0, 0)
	`, pastChromium, futureChromium, futureChromium)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	res, err := ReadBrowserCookies(dbPath)
	if err != nil {
		t.Fatalf("ReadBrowserCookies failed: %v", err)
	}
	if strings.Contains(res.Header, "expired_sid") {
		t.Fatalf("expected expired_sid to be filtered out, got header: %s", res.Header)
	}
	if !strings.Contains(res.Header, "SID=valid_sid") || !strings.Contains(res.Header, "SAPISID=valid_sap") {
		t.Fatalf("expected valid cookies in header, got: %s", res.Header)
	}
}

func TestDetectActiveProfileLock(t *testing.T) {
	tmp := t.TempDir()
	profDir := filepath.Join(tmp, "Default")
	netDir := filepath.Join(profDir, "Network")
	if err := os.MkdirAll(netDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(netDir, "Cookies")
	if err := os.WriteFile(dbPath, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DetectActiveProfileLock(dbPath); got != "" {
		t.Fatalf("expected no lock initially, got %q", got)
	}
	lockPath := filepath.Join(tmp, "SingletonLock")
	if err := os.WriteFile(lockPath, []byte("host-1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DetectActiveProfileLock(dbPath); got != lockPath {
		t.Fatalf("expected lock %q, got %q", lockPath, got)
	}
}

func TestSessionCookieHint(t *testing.T) {
	now := time.Now()
	mk := func(name string, exp time.Time) CookieRow {
		return CookieRow{Name: name, ExpiresAt: exp.Unix()}
	}

	t.Run("healthy session has no hint", func(t *testing.T) {
		res := &ReadResult{Cookies: []CookieRow{
			mk("SID", now.Add(365*24*time.Hour)),
			mk("SAPISID", now.Add(365*24*time.Hour)),
			// A rotating cookie near expiry must not trigger a warning.
			mk("__Secure-1PSIDTS", now.Add(time.Minute)),
		}}
		if hint := SessionCookieHint(res, 7*24*time.Hour); hint != "" {
			t.Fatalf("expected no hint, got %q", hint)
		}
	})

	t.Run("expired long-lived cookie is reported", func(t *testing.T) {
		res := &ReadResult{Cookies: []CookieRow{
			mk("SID", now.Add(-time.Hour)),
			mk("SAPISID", now.Add(365*24*time.Hour)),
		}}
		hint := SessionCookieHint(res, 7*24*time.Hour)
		if !strings.Contains(hint, "SID") || !strings.Contains(hint, "expired") {
			t.Fatalf("expected expired SID hint, got %q", hint)
		}
	})

	t.Run("soon-to-expire cookie is reported", func(t *testing.T) {
		res := &ReadResult{Cookies: []CookieRow{
			mk("__Secure-1PSID", now.Add(24*time.Hour)),
		}}
		hint := SessionCookieHint(res, 7*24*time.Hour)
		if !strings.Contains(hint, "expire soon") {
			t.Fatalf("expected expire-soon hint, got %q", hint)
		}
	})

	t.Run("cookies without expiry are ignored", func(t *testing.T) {
		res := &ReadResult{Cookies: []CookieRow{{Name: "SID", ExpiresAt: 0}}}
		if hint := SessionCookieHint(res, 7*24*time.Hour); hint != "" {
			t.Fatalf("expected no hint for a session cookie, got %q", hint)
		}
	})
}

func TestParseProfileHint_Arc(t *testing.T) {
	cases := []struct {
		in          string
		wantBrowser string
		wantProfile string
	}{
		{"/Users/alice/Library/Application Support/Arc/User Data/Default", "arc", "Default"},
		{"/Users/alice/Library/Application Support/Arc/User Data/Default/Network/Cookies", "arc", "Default"},
		{`C:\Users\alice\AppData\Local\Packages\TheBrowserCompany.Arc_abc123\LocalCache\Local\Arc\User Data\Default`, "arc", "Default"},
		{`C:\Users\alice\AppData\Local\Packages\TheBrowserCompany.Arc_abc123\LocalCache\Local\Arc\User Data\Profile 2\Network\Cookies`, "arc", "Profile 2"},
		{"arc", "arc", "Default"},
		{"arc:Profile 3", "arc", "Profile 3"},
	}
	for _, tc := range cases {
		b, p := ParseProfileHint(tc.in)
		if b != tc.wantBrowser || p != tc.wantProfile {
			t.Errorf("ParseProfileHint(%q) = (%q, %q), want (%q, %q)", tc.in, b, p, tc.wantBrowser, tc.wantProfile)
		}
	}
}

func TestBrowserFromDBPath(t *testing.T) {
	cases := map[string]string{
		"/Users/a/Library/Application Support/Arc/User Data/Default/Network/Cookies":                                       "arc",
		`C:\Users\a\AppData\Local\Packages\TheBrowserCompany.Arc_x\LocalCache\Local\Arc\User Data\Default\Network\Cookies`: "arc",
		"/home/a/.config/google-chrome/Default/Cookies":                                                                    "chrome",
		"/home/a/.config/BraveSoftware/Brave-Browser/Default/Cookies":                                                      "brave",
		"/home/a/.config/chromium/Default/Cookies":                                                                         "chromium",
		"/tmp/unknown/Cookies": "",
	}
	for in, want := range cases {
		if got := browserFromDBPath(in); got != want {
			t.Errorf("browserFromDBPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeychainServicesFor(t *testing.T) {
	arcPath := "/Users/a/Library/Application Support/Arc/User Data/Default/Network/Cookies"
	got := keychainServicesFor(arcPath)
	if len(got) != 1 || got[0] != "Arc Safe Storage" {
		t.Fatalf("Arc keychain services = %v, want [Arc Safe Storage]", got)
	}
	chromePath := "/Users/a/Library/Application Support/Google/Chrome/Default/Network/Cookies"
	got = keychainServicesFor(chromePath)
	if len(got) != 1 || got[0] != "Chrome Safe Storage" {
		t.Fatalf("Chrome keychain services = %v, want [Chrome Safe Storage]", got)
	}
	// Unknown path falls back to every known Chromium fork.
	if got := keychainServicesFor("/tmp/unknown/Cookies"); len(got) < 5 {
		t.Fatalf("expected fallback to list all Safe Storage services, got %v", got)
	}
}

func TestFindBrowserCookieDB_Arc(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "AppData", "Local"))

	arcProfile := filepath.Join(tmp, "Library", "Application Support", "Arc", "User Data", "Default")
	if err := os.MkdirAll(filepath.Join(arcProfile, "Network"), 0o700); err != nil {
		t.Fatal(err)
	}
	arcCookies := filepath.Join(arcProfile, "Network", "Cookies")
	if err := os.WriteFile(arcCookies, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := FindBrowserCookieDB("arc")
	if err != nil || got != arcCookies {
		t.Fatalf("FindBrowserCookieDB(arc) = %q, %v; want %q", got, err, arcCookies)
	}

	// Windows MSIX layout must be discovered too.
	winArc := filepath.Join(tmp, "AppData", "Local", "Packages", "TheBrowserCompany.Arc_xyz", "LocalCache", "Local", "Arc", "User Data", "Default", "Network")
	if err := os.MkdirAll(winArc, 0o700); err != nil {
		t.Fatal(err)
	}
	winCookies := filepath.Join(winArc, "Cookies")
	if err := os.WriteFile(winCookies, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = FindBrowserCookieDB(`%LOCALAPPDATA%\Packages\TheBrowserCompany.Arc_xyz\LocalCache\Local\Arc\User Data\Default`)
	if err != nil || got != winCookies {
		t.Fatalf("Windows Arc profile dir = %q, %v; want %q", got, err, winCookies)
	}
}

func TestFindChromeCookieDB_PrefersSignedInProfile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "AppData", "Local"))

	mk := func(rel string, withSAPISID bool) string {
		dir := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Join(dir, "Network"), 0o700); err != nil {
			t.Fatal(err)
		}
		dbPath := filepath.Join(dir, "Network", "Cookies")
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE cookies (host_key TEXT, name TEXT, value TEXT)`); err != nil {
			t.Fatal(err)
		}
		if withSAPISID {
			if _, err := db.Exec(`INSERT INTO cookies (host_key, name, value) VALUES ('.google.com', 'SAPISID', 'x')`); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		return dbPath
	}

	// Chrome exists but was never signed in; Arc is signed in.
	chromeDB := mk(filepath.Join("Library", "Application Support", "Google", "Chrome", "Default"), false)
	arcDB := mk(filepath.Join("Library", "Application Support", "Arc", "User Data", "Default"), true)

	if !hasSAPISIDCookieRow(arcDB) {
		t.Fatal("expected Arc DB to report a SAPISID row")
	}
	if hasSAPISIDCookieRow(chromeDB) {
		t.Fatal("expected Chrome DB to report no SAPISID row")
	}
	if got := FindChromeCookieDB(""); got != arcDB {
		t.Fatalf("FindChromeCookieDB auto-detect = %q, want signed-in Arc profile %q", got, arcDB)
	}
}

func TestRotateAccount_NoDoublePOSTOnTerminalStatus(t *testing.T) {
	for _, status := range []int{200, 401, 403, 429} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			var postCount, pageCount atomic.Int32
			doer := &stubCookieDoer{
				fn: func(req *fhttp.Request) (*fhttp.Response, error) {
					if strings.Contains(req.URL.Path, "RotateCookiesPage") {
						pageCount.Add(1)
						return &fhttp.Response{
							StatusCode: 200,
							Header:     fhttp.Header{},
							Body:       io.NopCloser(strings.NewReader(`<script>init('rot_id', 1, 540)</script>`)),
						}, nil
					}
					postCount.Add(1)
					return &fhttp.Response{
						StatusCode: status,
						Header:     fhttp.Header{},
						Body:       io.NopCloser(strings.NewReader("[]")),
					}, nil
				},
			}
			sc, err := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
			if err != nil {
				t.Fatalf("NewStealthClient: %v", err)
			}
			rot := NewRotator()
			res, err := rot.RotateAccount(context.Background(), "acct-term", "SAPISID=sap; __Secure-1PSID=psid; __Secure-1PSIDTS=ts", sc)
			if err != nil {
				t.Fatalf("RotateAccount: %v", err)
			}
			if res.Status != status {
				t.Fatalf("res.Status = %d, want %d (must not be masked by second POST)", res.Status, status)
			}
			if postCount.Load() != 1 || pageCount.Load() != 0 {
				t.Fatalf("expected exactly 1 POST and 0 RotateCookiesPage GETs, got post=%d page=%d", postCount.Load(), pageCount.Load())
			}
		})
	}
}

func TestRotateAccount_FallbackOn5xxOrErrorAndConcurrentRaceFree(t *testing.T) {
	t.Run("500_falls_back_to_iframe", func(t *testing.T) {
		var postCount, pageCount atomic.Int32
		doer := &stubCookieDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				if strings.Contains(req.URL.Path, "RotateCookiesPage") {
					pageCount.Add(1)
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{},
						Body:       io.NopCloser(strings.NewReader(`<script>init('rot_id_fb', 1, 540)</script>`)),
					}, nil
				}
				n := postCount.Add(1)
				if n == 1 {
					return &fhttp.Response{StatusCode: 500, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader("err"))}, nil
				}
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"SIDCC=fb_sidcc; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader("[]")),
				}, nil
			},
		}
		sc, _ := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
		res, err := NewRotator().RotateAccount(context.Background(), "acct-500", "SAPISID=sap; __Secure-1PSID=psid", sc)
		if err != nil {
			t.Fatalf("RotateAccount: %v", err)
		}
		if !res.Rotated || res.Status != 200 || !strings.Contains(res.CookieHeader, "SIDCC=fb_sidcc") {
			t.Fatalf("expected fallback rotation to succeed, got %+v", res)
		}
		if postCount.Load() != 2 || pageCount.Load() != 1 {
			t.Fatalf("expected 2 POSTs + 1 page GET, got post=%d page=%d", postCount.Load(), pageCount.Load())
		}
	})

	t.Run("network_error_falls_back_to_iframe", func(t *testing.T) {
		var postCount atomic.Int32
		doer := &stubCookieDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				if strings.Contains(req.URL.Path, "RotateCookiesPage") {
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{},
						Body:       io.NopCloser(strings.NewReader(`<script>init('rot_id_err', 1, 540)</script>`)),
					}, nil
				}
				if postCount.Add(1) == 1 {
					return nil, fmt.Errorf("transient dial error")
				}
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDCC=fb_cc; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader("[]")),
				}, nil
			},
		}
		sc, _ := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
		res, err := NewRotator().RotateAccount(context.Background(), "acct-err", "SAPISID=sap; __Secure-1PSID=psid", sc)
		if err != nil || !res.Rotated {
			t.Fatalf("expected fallback rotation after error, got res=%+v err=%v", res, err)
		}
	})

	t.Run("concurrent_rotate_race_free", func(t *testing.T) {
		doer := &stubCookieDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				if strings.Contains(req.URL.Path, "RotateCookiesPage") {
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{},
						Body:       io.NopCloser(strings.NewReader(`<script>init('rot_race', 1, 540)</script>`)),
					}, nil
				}
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=ts_ok; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader("[]")),
				}, nil
			},
		}
		sc, _ := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
		rot := NewRotator()
		var wg sync.WaitGroup
		for i := range 6 {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for range 10 {
					_, _ = rot.RotateAccount(context.Background(), fmt.Sprintf("acct-%d", id%2), "SAPISID=sap; __Secure-1PSID=psid", sc)
				}
			}(i)
		}
		wg.Wait()
	})
	t.Run("subsecond_fallback_interval_clamps_to_60s", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			sec  string
			want time.Duration
		}{
			{"half_second", "0.5", 60 * time.Second},
			{"tiny_fraction", "0.0000000001", 60 * time.Second},
			{"huge_overflow", "99999999999999999", 3600 * time.Second},
		} {
			t.Run(tc.name, func(t *testing.T) {
				doer := &stubCookieDoer{
					fn: func(req *fhttp.Request) (*fhttp.Response, error) {
						if strings.Contains(req.URL.Path, "RotateCookiesPage") {
							return &fhttp.Response{
								StatusCode: 200,
								Header:     fhttp.Header{},
								Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`<script>init('rot_clamp', 1, %s)</script>`, tc.sec))),
							}, nil
						}
						return &fhttp.Response{
							StatusCode: 502,
							Header:     fhttp.Header{},
							Body:       io.NopCloser(strings.NewReader("bad gateway")),
						}, nil
					},
				}
				sc, err := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
				if err != nil {
					t.Fatalf("NewStealthClient: %v", err)
				}
				res, err := NewRotator().RotateAccount(context.Background(), "acct-clamp-"+tc.name, "SAPISID=sap; __Secure-1PSID=psid", sc)
				if err != nil {
					t.Fatalf("RotateAccount: %v", err)
				}
				if res.NextInterval != tc.want {
					t.Fatalf("init(..., %s): NextInterval = %v, want %v", tc.sec, res.NextInterval, tc.want)
				}
			})
		}
	})
}

func TestSaveCredentialFilePreservesAuthUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secondary.json")
	if _, err := SaveCredentialFile(path, CredentialFile{
		Name:     "secondary",
		Email:    "sec@example.com",
		Cookies:  "SAPISID=s1",
		AuthUser: 2,
	}); err != nil {
		t.Fatalf("initial SaveCredentialFile: %v", err)
	}
	if _, err := SaveCredentialFile(path, CredentialFile{
		Name:    "secondary",
		Email:   "sec@example.com",
		Cookies: "SAPISID=s2",
	}); err != nil {
		t.Fatalf("overwrite SaveCredentialFile: %v", err)
	}
	loaded, _, err := LoadAccountFile(path)
	if err != nil {
		t.Fatalf("LoadAccountFile: %v", err)
	}
	if loaded.AuthUser != 2 || loaded.Cookies != "SAPISID=s2" {
		t.Fatalf("expected AuthUser=2 preserved and Cookies updated, got %+v", loaded)
	}
}

func TestExpandPercentEnv_SinglePass(t *testing.T) {
	t.Setenv("SELF_LOOP", "%SELF_LOOP%")
	t.Setenv("MUT_A", "%MUT_B%")
	t.Setenv("MUT_B", "%MUT_A%")
	t.Setenv("DIR_VAR", "/tmp/%NESTED%")
	t.Setenv("NESTED", "should_not_expand")
	t.Setenv("GOOD_VAR", "ok")

	cases := []struct {
		in   string
		want string
	}{
		{"%SELF_LOOP%/Cookies", "%SELF_LOOP%/Cookies"},
		{"%MUT_A%/Cookies", "%MUT_B%/Cookies"},
		{"%DIR_VAR%/Cookies", "/tmp/%NESTED%/Cookies"},
		{"100%%/%GOOD_VAR%", "100%%/" + "ok"},
		{"%MISSING_VAR%/%GOOD_VAR%", "%MISSING_VAR%/ok"},
		{"%UNCLOSED", "%UNCLOSED"},
	}
	for _, tc := range cases {
		if got := expandPercentEnv(tc.in); got != tc.want {
			t.Errorf("expandPercentEnv(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func encryptAES128CBCForTest(key128, plain []byte) []byte {
	block, err := aes.NewCipher(key128)
	if err != nil {
		panic(err)
	}
	padLen := aes.BlockSize - (len(plain) % aes.BlockSize)
	padded := make([]byte, len(plain)+padLen)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	iv := bytes.Repeat([]byte{0x20}, aes.BlockSize)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	return append([]byte("v10"), ct...)
}

func TestDecryptCookieBytes_AES128CBC_CookieOctetAndChrome130Prefix(t *testing.T) {
	trueKey := bytes.Repeat([]byte{0x11}, 16)
	wrongKey := bytes.Repeat([]byte{0x22}, 16)

	// 1. Valid ASCII cookie passes.
	validCT := encryptAES128CBCForTest(trueKey, []byte("valid_cookie_123"))
	if got := decryptCookieBytesForHost(validCT, nil, trueKey, ".google.com"); string(got) != "valid_cookie_123" {
		t.Fatalf("expected valid cookie decrypted, got %q", got)
	}

	// 2. Binary garbage ending in valid PKCS#7 padding (0x01) under wrongKey must be rejected (nil).
	garbagePlain := append(bytes.Repeat([]byte{0x00, 0x80, 0xff}, 5), 0x01)
	garbagePlain = garbagePlain[:16]
	garbagePlain[15] = 0x01 // valid 1-byte PKCS#7 padding
	// Encrypt garbagePlain with wrongKey so decrypting with wrongKey produces garbagePlain!
	block, _ := aes.NewCipher(wrongKey)
	iv := bytes.Repeat([]byte{0x20}, aes.BlockSize)
	ctBuf := make([]byte, 16)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ctBuf, garbagePlain)
	fakeV10 := append([]byte("v10"), ctBuf...)
	if got := decryptCookieBytesForHost(fakeV10, nil, wrongKey, ".google.com"); got != nil {
		t.Fatalf("expected binary garbage with accidental PKCS#7 padding to be rejected, got %x", got)
	}

	// 3. Chrome 130+ sha256(host) + valid ASCII cookie passes when host is supplied.
	hostHash := sha256.Sum256([]byte(".google.com"))
	prefixedPlain := append(hostHash[:], []byte("sap_cbc_130")...)
	prefixedCT := encryptAES128CBCForTest(trueKey, prefixedPlain)
	dec := decryptCookieBytesForHost(prefixedCT, nil, trueKey, ".google.com")
	if dec == nil {
		t.Fatal("expected Chrome 130+ prefixed AES-128-CBC cookie to decrypt cleanly when host is supplied")
	}
	stripped, bound := stripDomainPrefix(dec, ".google.com")
	if !bound || string(stripped) != "sap_cbc_130" {
		t.Fatalf("unexpected stripped cookie: bound=%v val=%q", bound, stripped)
	}
}

func TestKeyringCommandTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell script test not applicable on Windows")
	}
	binDir := t.TempDir()
	cmdName := "secret-tool"
	if runtime.GOOS == "darwin" {
		cmdName = "security"
	}
	scriptPath := filepath.Join(binDir, cmdName)
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nsleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CHROME_PASS", "")

	origTimeout := keyringCmdTimeout
	keyringCmdTimeout = 100 * time.Millisecond
	defer func() { keyringCmdTimeout = origTimeout }()

	start := time.Now()
	passes := candidatePasswords("")
	elapsed := time.Since(start)
	if elapsed >= time.Second {
		t.Fatalf("candidatePasswords took %v, expected < 1s on hung keyring", elapsed)
	}
	foundPeanuts := false
	for _, p := range passes {
		if p == "peanuts" {
			foundPeanuts = true
		}
	}
	if !foundPeanuts {
		t.Fatalf("expected fallback 'peanuts' in candidatePasswords, got %v", passes)
	}
}

func TestRotateAccount_Non200IgnoresSetCookieAndDeletions(t *testing.T) {
	initial := "SID=s1; SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts1; SIDCC=cc1"
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprintf("primary_%d", status), func(t *testing.T) {
			doer := &stubCookieDoer{
				fn: func(req *fhttp.Request) (*fhttp.Response, error) {
					return &fhttp.Response{
						StatusCode: status,
						Header: fhttp.Header{
							"Set-Cookie": []string{
								"__Secure-1PSIDTS=; Max-Age=0; Path=/; Secure",
								"SIDCC=deleted; Path=/",
							},
						},
						Body: io.NopCloser(strings.NewReader("[]")),
					}, nil
				},
			}
			sc, _ := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
			res, err := NewRotator().RotateAccount(context.Background(), "acct-non200", initial, sc)
			if err != nil {
				t.Fatalf("RotateAccount: %v", err)
			}
			if res.Status != status || res.Rotated || len(res.SetCookies) != 0 {
				t.Fatalf("expected Status=%d Rotated=false SetCookies=nil, got %+v", status, res)
			}
			if res.CookieHeader != initial {
				t.Fatalf("essential cookies modified on %d: got %q", status, res.CookieHeader)
			}
		})

		t.Run(fmt.Sprintf("fallback_post_%d", status), func(t *testing.T) {
			var posts atomic.Int32
			doer := &stubCookieDoer{
				fn: func(req *fhttp.Request) (*fhttp.Response, error) {
					if strings.Contains(req.URL.Path, "RotateCookiesPage") {
						return &fhttp.Response{
							StatusCode: 200,
							Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDCC=page_cc; Path=/; Secure"}},
							Body:       io.NopCloser(strings.NewReader(`<script>init('rot_fb', 1, 540)</script>`)),
						}, nil
					}
					if posts.Add(1) == 1 {
						return &fhttp.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("err"))}, nil
					}
					return &fhttp.Response{
						StatusCode: status,
						Header: fhttp.Header{
							"Set-Cookie": []string{"__Secure-1PSIDTS=; Max-Age=0; Path=/; Secure"},
						},
						Body: io.NopCloser(strings.NewReader("[]")),
					}, nil
				},
			}
			sc, _ := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
			res, err := NewRotator().RotateAccount(context.Background(), "acct-fb-non200", initial, sc)
			if err != nil {
				t.Fatalf("RotateAccount: %v", err)
			}
			if res.Status != status || res.Rotated || len(res.SetCookies) != 0 || res.CookieHeader != initial {
				t.Fatalf("expected fallback non-200 not to absorb/rotate, got %+v", res)
			}
		})
	}
}

func TestRotateAccount_200DeletionOnlyNotRotated(t *testing.T) {
	initial := "SID=s1; SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts1; SIDCC=cc1"
	doer := &stubCookieDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{"Set-Cookie": []string{"SIDCC=; Max-Age=0; Path=/; Secure"}},
				Body:       io.NopCloser(strings.NewReader("[]")),
			}, nil
		},
	}
	sc, _ := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
	res, err := NewRotator().RotateAccount(context.Background(), "acct-del200", initial, sc)
	if err != nil {
		t.Fatalf("RotateAccount: %v", err)
	}
	if res.Rotated {
		t.Fatalf("deletion-only 200 must not report Rotated=true: %+v", res)
	}
}

func TestRotateAccount_FallbackPostErrorAndPage401Status(t *testing.T) {
	t.Run("fallback_post_error_returned", func(t *testing.T) {
		var posts atomic.Int32
		doer := &stubCookieDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				if strings.Contains(req.URL.Path, "RotateCookiesPage") {
					return &fhttp.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(`<script>init('rot_err', 1, 540)</script>`)),
					}, nil
				}
				if posts.Add(1) == 1 {
					return &fhttp.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("err"))}, nil
				}
				return nil, fmt.Errorf("fallback post dial failure")
			},
		}
		sc, _ := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
		_, err := NewRotator().RotateAccount(context.Background(), "acct-posterr", "SAPISID=sap", sc)
		if err == nil || !strings.Contains(err.Error(), "fallback post dial failure") {
			t.Fatalf("expected fallback postErr returned, got %v", err)
		}
	})

	for _, pageStatus := range []int{401, 403} {
		t.Run(fmt.Sprintf("fallback_page_%d_updates_status", pageStatus), func(t *testing.T) {
			doer := &stubCookieDoer{
				fn: func(req *fhttp.Request) (*fhttp.Response, error) {
					if strings.Contains(req.URL.Path, "RotateCookiesPage") {
						return &fhttp.Response{
							StatusCode: pageStatus,
							Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=; Max-Age=0"}},
							Body:       io.NopCloser(strings.NewReader("denied")),
						}, nil
					}
					return &fhttp.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("err"))}, nil
				},
			}
			sc, _ := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
			res, err := NewRotator().RotateAccount(context.Background(), "acct-pageauth", "SAPISID=sap; __Secure-1PSIDTS=ts1", sc)
			if err != nil {
				t.Fatalf("RotateAccount: %v", err)
			}
			if res.Status != pageStatus || res.Rotated {
				t.Fatalf("expected Status=%d Rotated=false, got %+v", pageStatus, res)
			}
		})
	}
}

func TestDomainScoringAndNonGoogleFilter(t *testing.T) {
	payload := `[
		{"name":"SAPISID","value":"evil_sap","domain":".evilgoogle.com"},
		{"name":"SAPISID","value":"sub_sap","domain":"accounts.google.com"},
		{"name":"SAPISID","value":"root_sap","domain":".google.com"},
		{"name":"OSID","value":"root_osid","domain":".google.com"},
		{"name":"OSID","value":"gemini_osid","domain":"gemini.google.com"},
		{"name":"__Secure-OSID","value":"gemini_sosid","domain":"gemini.google.com"},
		{"name":"__Secure-OSID","value":"root_sosid","domain":".google.com"},
		{"name":"LSID","value":"root_lsid","domain":".google.com"},
		{"name":"LSID","value":"acct_lsid","domain":"accounts.google.com"},
		{"name":"__Host-1PLSID","value":"acct_1plsid","domain":"accounts.google.com"},
		{"name":"ACCOUNT_CHOOSER","value":"acct_chooser","domain":"accounts.google.com"},
		{"name":"SID","value":"evil_sid","domain":"notgoogle.com"}
	]`
	hdr, _, err := ParseImportPayload(payload)
	if err != nil {
		t.Fatalf("ParseImportPayload: %v", err)
	}
	m := ParseHeader(hdr)
	if m["SAPISID"] != "root_sap" {
		t.Fatalf("expected .google.com SAPISID, got %q", m["SAPISID"])
	}
	if m["OSID"] != "gemini_osid" || m["__Secure-OSID"] != "gemini_sosid" {
		t.Fatalf("expected gemini.google.com OSID/__Secure-OSID, got OSID=%q __Secure-OSID=%q", m["OSID"], m["__Secure-OSID"])
	}
	if m["LSID"] != "acct_lsid" || m["__Host-1PLSID"] != "acct_1plsid" || m["ACCOUNT_CHOOSER"] != "acct_chooser" {
		t.Fatalf("expected accounts.google.com LSID/__Host-1PLSID/ACCOUNT_CHOOSER, got %+v", m)
	}
	if m["SID"] != "" {
		t.Fatalf("expected non-.google.com SID skipped, got %q", m["SID"])
	}
}

func TestIsCookieDeletion_DateLayouts(t *testing.T) {
	now := time.Now()
	for _, exp := range []string{
		"Thu, 01-Jan-1970 00:00:00 GMT",
		"Thursday, 01-Jan-1970 00:00:00 GMT",
		"Thu, 01-Jan-70 00:00:00 GMT",
		`"Thu, 01-Jan-1970 00:00:00 GMT"`,
	} {
		if !isCookieDeletion("val", "Expires="+exp+"; Path=/", now) {
			t.Errorf("expected Expires=%s to be detected as deletion", exp)
		}
	}
	future := now.Add(24 * time.Hour).UTC().Format("Mon, 02-Jan-2006 15:04:05 MST")
	if isCookieDeletion("val", "Expires="+future+"; Path=/", now) {
		t.Errorf("expected future Expires=%s NOT to be deletion", future)
	}
}

func TestSQLiteWALAndURISpecialChars(t *testing.T) {
	base := filepath.Join(t.TempDir(), "dir#with?special%chars and spaces")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(base, "Cookies")
	db, err := sql.Open("sqlite", sqliteFileURI(dbPath, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE cookies (host_key TEXT, name TEXT, encrypted_value BLOB, value TEXT, has_expires INTEGER, expires_utc INTEGER, is_persistent INTEGER)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	// Checkpoint schema, then keep SAPISID row in the uncheckpointed WAL.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE);`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cookies VALUES ('.google.com', 'SAPISID', X'', 'wal_sap_123', 0, 0, 0)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	// Hold db open so WAL is not auto-checkpointed on close.
	defer db.Close()

	if !hasSAPISIDCookieRow(dbPath) {
		t.Fatal("expected hasSAPISIDCookieRow to detect uncheckpointed WAL row in special-char path")
	}
	res, err := ReadBrowserCookies(dbPath)
	if err != nil {
		t.Fatalf("ReadBrowserCookies: %v", err)
	}
	if ExtractSAPISID(res.Header) != "wal_sap_123" {
		t.Fatalf("expected wal_sap_123 from uncheckpointed WAL, got %q", res.Header)
	}
}
