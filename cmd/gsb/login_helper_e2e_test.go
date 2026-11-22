//go:build e2e

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/webui"
	"golang.org/x/crypto/pbkdf2"
	_ "modernc.org/sqlite"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ephemeral port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func resolveGSBPackageDir(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("main.go"); err == nil {
		if abs, err := filepath.Abs("."); err == nil {
			return abs
		}
		return "."
	}
	if _, err := os.Stat(filepath.Join("cmd", "gsb", "main.go")); err == nil {
		if abs, err := filepath.Abs(filepath.Join("cmd", "gsb")); err == nil {
			return abs
		}
		return filepath.Join("cmd", "gsb")
	}
	if _, thisFile, _, ok := runtime.Caller(0); ok {
		cand := filepath.Dir(thisFile)
		if _, err := os.Stat(filepath.Join(cand, "main.go")); err == nil {
			return cand
		}
	}
	return ""
}

func buildGSBBinary(t *testing.T) string {
	t.Helper()
	binName := "gsb"
	if runtime.GOOS == "windows" {
		binName = "gsb.exe"
	}
	out := filepath.Join(t.TempDir(), binName)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pkgDir := resolveGSBPackageDir(t)
	var cmd *exec.Cmd
	if pkgDir != "" {
		cmd = exec.CommandContext(ctx, "go", "build", "-o", out, ".")
		cmd.Dir = pkgDir
	} else {
		cmd = exec.CommandContext(ctx, "go", "build", "-o", out, "github.com/azrinaldi/gemini-stealth-bridge/cmd/gsb")
	}
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build gsb failed (dir=%q): %v\n%s", cmd.Dir, err, string(raw))
	}
	return out
}

func buildFakeCDPBrowser(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	goMod := "module fakebrowser\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write fakebrowser go.mod: %v", err)
	}

	const fakeSrc = `package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func writeWSFrame(w io.Writer, fin bool, opcode byte, payload []byte) error {
	b0 := opcode & 0x0f
	if fin {
		b0 |= 0x80
	}
	hdr := []byte{b0}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 65535:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		hdr = append(hdr, 127)
		hdr = append(hdr, ext[:]...)
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readWSFrame(r *bufio.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	masked := (hdr[1] & 0x80) != 0
	length := uint64(hdr[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return nil, err
		}
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	if masked {
		for i := range buf {
			buf[i] ^= mask[i%4]
		}
	}
	return buf, nil
}

func main() {
	if argsFile := os.Getenv("FAKE_CDP_ARGS_FILE"); argsFile != "" {
		b, _ := json.Marshal(os.Args[1:])
		_ = os.WriteFile(argsFile, b, 0o600)
	}
	mode := os.Getenv("FAKE_CDP_MODE")
	if mode == "exit_immediately" {
		os.Exit(1)
	}
	if mode == "hang" {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		select {
		case <-ch:
		case <-time.After(30 * time.Second):
		}
		return
	}

	port := ""
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, "--remote-debugging-port=") {
			port = strings.TrimPrefix(a, "--remote-debugging-port=")
		}
	}
	if port == "" {
		os.Exit(2)
	}

	var getCookiesCalls atomic.Int64
	traceFile := os.Getenv("FAKE_CDP_TRACE_FILE")
	var traceMu sync.Mutex
	var traceState struct {
		ListCalls  int    ` + "`json:\"list_calls\"`" + `
		WSCalls    int    ` + "`json:\"ws_calls\"`" + `
		LastOrigin string ` + "`json:\"last_origin\"`" + `
		OriginSeen bool   ` + "`json:\"origin_seen\"`" + `
	}
	recordTrace := func(fn func()) {
		if traceFile == "" {
			return
		}
		traceMu.Lock()
		defer traceMu.Unlock()
		fn()
		b, _ := json.Marshal(traceState)
		_ = os.WriteFile(traceFile, b, 0o600)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		wsURL := fmt.Sprintf("ws://127.0.0.1:%s/devtools/browser/1", port)
		switch mode {
		case "bad_ws_host":
			wsURL = fmt.Sprintf("ws://evil.example:%s/devtools/browser/1", port)
		case "bad_ws_localhost":
			wsURL = fmt.Sprintf("ws://localhost:%s/devtools/browser/1", port)
		case "bad_ws_scheme":
			wsURL = fmt.Sprintf("wss://127.0.0.1:%s/devtools/browser/1", port)
		case "bad_ws_query_frag_user":
			wsURL = fmt.Sprintf("ws://user@127.0.0.1:%s/devtools/browser/1?tok=1#frag", port)
		case "bad_ws_path":
			wsURL = fmt.Sprintf("ws://127.0.0.1:%s/evil/browser/1", port)
		}
		_, _ = fmt.Fprintf(w, "{\"User-Agent\":\"FakeChrome/150\",\"webSocketDebuggerUrl\":%q}", wsURL)
	})
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		recordTrace(func() { traceState.ListCalls++ })
		w.Header().Set("Content-Type", "application/json")
		wsURL := fmt.Sprintf("ws://127.0.0.1:%s/devtools/page/1", port)
		switch mode {
		case "browser_ws_fallback":
			wsURL = ""
		case "bad_ws_host":
			wsURL = fmt.Sprintf("ws://evil.example:%s/devtools/page/1", port)
		case "bad_ws_localhost":
			wsURL = fmt.Sprintf("ws://localhost:%s/devtools/page/1", port)
		case "bad_ws_scheme":
			wsURL = fmt.Sprintf("wss://127.0.0.1:%s/devtools/page/1", port)
		case "bad_ws_query_frag_user":
			wsURL = fmt.Sprintf("ws://user@127.0.0.1:%s/devtools/page/1?tok=1#frag", port)
		case "bad_ws_path":
			wsURL = fmt.Sprintf("ws://127.0.0.1:%s/evil/page/1", port)
		}
		_, _ = fmt.Fprintf(w, "[{\"type\":\"page\",\"url\":\"https://gemini.google.com/app\",\"webSocketDebuggerUrl\":%q}]", wsURL)
	})
	wsHandler := func(w http.ResponseWriter, r *http.Request) {
		orig := r.Header.Get("Origin")
		_, hasOrigin := r.Header["Origin"]
		recordTrace(func() {
			traceState.WSCalls++
			traceState.LastOrigin = orig
			traceState.OriginSeen = hasOrigin
		})
		if orig != "" {
			http.Error(w, "unexpected origin", http.StatusForbidden)
			return
		}
		if mode == "ws_non_101" {
			http.Error(w, "ws upgrade refused", http.StatusForbidden)
			return
		}
		key := r.Header.Get("Sec-WebSocket-Key")
		sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		accept := base64.StdEncoding.EncodeToString(sum[:])

		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", 500)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n")
		_ = rw.Flush()

		reqPayload, err := readWSFrame(rw.Reader)
		if err != nil {
			return
		}
		var reqObj struct {
			ID int ` + "`json:\"id\"`" + `
		}
		_ = json.Unmarshal(reqPayload, &reqObj)
		if reqObj.ID == 0 {
			reqObj.ID = 1
		}

		type cookieEntry struct {
			Name   string ` + "`json:\"name\"`" + `
			Value  string ` + "`json:\"value\"`" + `
			Domain string ` + "`json:\"domain\"`" + `
		}
		var cList []cookieEntry
		switch mode {
		case "3papisid_fallback":
			cList = []cookieEntry{
				{Name: "SAPISID", Value: "sapisid_ok", Domain: ".google.com"},
				{Name: "__Secure-3PAPISID", Value: "3papisid_ok", Domain: ".google.com"},
				{Name: "__Secure-1PSID", Value: "psid_3p", Domain: ".google.com"},
				{Name: "__Secure-1PSIDTS", Value: "ts_3p", Domain: ".google.com"},
				{Name: "SIDCC", Value: "sidcc_3p", Domain: ".google.com"},
				{Name: "__Secure-1PSIDCC", Value: "psidcc_3p", Domain: ".google.com"},
				{Name: "__Secure-3PSIDTS", Value: "ts3_3p", Domain: ".google.com"},
			}
		case "exit_after_base_ready":
			cList = []cookieEntry{
				{Name: "SAPISID", Value: "sap_exit_base", Domain: ".google.com"},
				{Name: "__Secure-1PSID", Value: "psid_exit_base", Domain: ".google.com"},
				{Name: "__Secure-1PSIDTS", Value: "ts_exit_base", Domain: ".google.com"},
			}
		case "filter_non_google":
			cList = []cookieEntry{
				{Name: "SAPISID", Value: "evil_sap", Domain: ".evilgoogle.com"},
				{Name: "__Secure-1PSID", Value: "evil_psid", Domain: "google.com.attacker.test"},
				{Name: "_ga", Value: "junk_tracking", Domain: ".google.com"},
				{Name: "SAPISID", Value: "good_google_sap", Domain: ".google.com"},
				{Name: "__Secure-1PSID", Value: "good_google_psid", Domain: ".google.com"},
				{Name: "__Secure-1PSIDTS", Value: "good_google_ts", Domain: ".google.com"},
				{Name: "SIDCC", Value: "good_sidcc", Domain: ".google.com"},
				{Name: "__Secure-1PSIDCC", Value: "good_psidcc", Domain: ".google.com"},
				{Name: "__Secure-3PSIDTS", Value: "good_ts3", Domain: ".google.com"},
			}
		case "domain_scoring":
			cList = []cookieEntry{
				{Name: "SAPISID", Value: "stale_sub", Domain: "accounts.google.com"},
				{Name: "__Secure-1PSID", Value: "psid_sub", Domain: "accounts.google.com"},
				{Name: "__Secure-1PSIDTS", Value: "ts_sub", Domain: "accounts.google.com"},
				{Name: "SAPISID", Value: "best_root", Domain: ".google.com"},
				{Name: "__Secure-1PSID", Value: "psid_root", Domain: ".google.com"},
				{Name: "__Secure-1PSIDTS", Value: "ts_root", Domain: ".google.com"},
				{Name: "SIDCC", Value: "sidcc_root", Domain: ".google.com"},
				{Name: "__Secure-1PSIDCC", Value: "psidcc_root", Domain: ".google.com"},
				{Name: "__Secure-3PSIDTS", Value: "ts3_root", Domain: ".google.com"},
			}
		case "stability_wait":
			call := getCookiesCalls.Add(1)
			if call == 1 {
				cList = []cookieEntry{
					{Name: "SAPISID", Value: "sap_s1", Domain: ".google.com"},
					{Name: "__Secure-1PSID", Value: "psid_s1", Domain: ".google.com"},
					{Name: "__Secure-1PSIDTS", Value: "ts_s1", Domain: ".google.com"},
				}
			} else {
				cList = []cookieEntry{
					{Name: "SAPISID", Value: "sap_s2", Domain: ".google.com"},
					{Name: "__Secure-1PSID", Value: "psid_s2", Domain: ".google.com"},
					{Name: "__Secure-1PSIDTS", Value: "ts_s2", Domain: ".google.com"},
					{Name: "SIDCC", Value: "sidcc_s2", Domain: ".google.com"},
					{Name: "__Secure-1PSIDCC", Value: "psidcc_s2", Domain: ".google.com"},
					{Name: "__Secure-3PSIDTS", Value: "ts3_s2", Domain: ".google.com"},
					{Name: "OSID", Value: "osid_s2", Domain: ".google.com"},
					{Name: "__Secure-OSID", Value: "secosid_s2", Domain: ".google.com"},
					{Name: "LSID", Value: "lsid_s2", Domain: ".google.com"},
					{Name: "__Host-1PLSID", Value: "h1plsid_s2", Domain: ".google.com"},
					{Name: "__Host-3PLSID", Value: "h3plsid_s2", Domain: ".google.com"},
				}
			}
		default:
			cList = []cookieEntry{
				{Name: "SAPISID", Value: "sap_happy", Domain: ".google.com"},
				{Name: "__Secure-1PSID", Value: "psid_happy", Domain: ".google.com"},
				{Name: "__Secure-1PSIDTS", Value: "ts_happy", Domain: ".google.com"},
				{Name: "SIDCC", Value: "sidcc_happy", Domain: ".google.com"},
				{Name: "__Secure-1PSIDCC", Value: "psidcc_happy", Domain: ".google.com"},
				{Name: "__Secure-3PSIDTS", Value: "ts3_happy", Domain: ".google.com"},
			}
		}

		respBytes, _ := json.Marshal(map[string]any{
			"id": reqObj.ID,
			"result": map[string]any{
				"cookies": cList,
			},
		})

		if mode == "fragmented_and_event" {
			_ = writeWSFrame(rw, true, 0x01, []byte("{\"method\":\"Network.requestWillBeSent\",\"params\":{\"requestId\":\"1\"}}"))
			mid := len(respBytes) / 2
			_ = writeWSFrame(rw, false, 0x01, respBytes[:mid])
			_ = writeWSFrame(rw, true, 0x00, respBytes[mid:])
		} else {
			_ = writeWSFrame(rw, true, 0x01, respBytes)
		}
		_ = rw.Flush()
		if mode == "exit_after_base_ready" {
			time.Sleep(150 * time.Millisecond)
			os.Exit(0)
		}
		time.Sleep(150 * time.Millisecond)
	}
	mux.HandleFunc("/devtools/page/1", wsHandler)
	mux.HandleFunc("/devtools/browser/1", wsHandler)

	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		os.Exit(3)
	}
	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(ln)
	}()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
	}
	_ = srv.Close()
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(fakeSrc), 0o600); err != nil {
		t.Fatalf("write fakebrowser main.go: %v", err)
	}
	binName := "fakebrowser"
	if runtime.GOOS == "windows" {
		binName = "fakebrowser.exe"
	}
	binPath := filepath.Join(dir, binName)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binPath, ".")
	cmd.Dir = dir
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fakebrowser failed: %v\n%s", err, string(raw))
	}
	return binPath
}

func assertIsolatedLoginArgsHardened(t *testing.T, argsFile string) {
	t.Helper()
	rawArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	var args []string
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	joined := strings.Join(args, " ")
	var userDataDir string
	var disableFeaturesArg string
	for _, a := range args {
		if strings.HasPrefix(a, "--remote-allow-origins") {
			t.Fatal("native CDP login must not override browser origin policy")
		}
		if strings.HasPrefix(a, "--user-data-dir=") {
			userDataDir = strings.TrimPrefix(a, "--user-data-dir=")
		}
		if strings.HasPrefix(a, "--disable-features=") {
			disableFeaturesArg = a
		}
	}
	for _, want := range []string{
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir=",
		"--remote-debugging-port=",
		"--disable-features=",
		"DeviceBoundSessionCredentials",
		"BoundSessionCredentials",
		"EnableBoundSessionCredentials",
		"DeviceBoundSessions",
		"https://gemini.google.com/app",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("isolated login args missing %q: %v", want, args)
		}
	}
	for _, dbscFeat := range []string{
		"DeviceBoundSessionCredentials",
		"BoundSessionCredentials",
		"EnableBoundSessionCredentials",
		"DeviceBoundSessions",
	} {
		if !strings.Contains(disableFeaturesArg, dbscFeat) {
			t.Fatalf("--disable-features missing anti-DBSC flag %q: %q", dbscFeat, disableFeaturesArg)
		}
	}
	if userDataDir == "" {
		t.Fatalf("expected --user-data-dir in args: %v", args)
	}
	if _, statErr := os.Stat(userDataDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected temporary profile dir %s to be deleted after return, statErr=%v", userDataDir, statErr)
	}
}

func TestE2EIsolatedBrowserLogin_Tier1_HappyAndArgsHardening(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Setenv("DISPLAY", ":99")
	}
	fakeBin := buildFakeCDPBrowser(t)

	t.Run("page_ws_capture_and_args_hardening", func(t *testing.T) {
		argsFile := filepath.Join(t.TempDir(), "args.json")
		t.Setenv("FAKE_CDP_ARGS_FILE", argsFile)
		t.Setenv("FAKE_CDP_MODE", "happy")

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := cookies.RunIsolatedBrowserLogin(ctx, fakeBin)
		if err != nil {
			t.Fatalf("RunIsolatedBrowserLogin failed: %v", err)
		}
		if res == nil || res.CookieHeader == "" {
			t.Fatalf("expected non-empty CookieHeader, got %+v", res)
		}
		if res.UserAgent != "FakeChrome/150" {
			t.Fatalf("expected UserAgent FakeChrome/150, got %q", res.UserAgent)
		}
		assertIsolatedLoginArgsHardened(t, argsFile)
	})

	t.Run("json_version_browser_ws_fallback", func(t *testing.T) {
		argsFile := filepath.Join(t.TempDir(), "args.json")
		t.Setenv("FAKE_CDP_ARGS_FILE", argsFile)
		t.Setenv("FAKE_CDP_MODE", "browser_ws_fallback")

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := cookies.RunIsolatedBrowserLogin(ctx, fakeBin)
		if err != nil {
			t.Fatalf("RunIsolatedBrowserLogin(browser_ws_fallback) failed: %v", err)
		}
		parsed := cookies.ParseHeader(res.CookieHeader)
		if parsed["SAPISID"] != "sap_happy" || parsed["__Secure-1PSID"] != "psid_happy" {
			t.Fatalf("expected fallback browser WS cookies captured, got %+v", parsed)
		}
		assertIsolatedLoginArgsHardened(t, argsFile)
	})

	t.Run("secure_3papisid_fallback", func(t *testing.T) {
		argsFile := filepath.Join(t.TempDir(), "args.json")
		t.Setenv("FAKE_CDP_ARGS_FILE", argsFile)
		t.Setenv("FAKE_CDP_MODE", "3papisid_fallback")

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := cookies.RunIsolatedBrowserLogin(ctx, fakeBin)
		if err != nil {
			t.Fatalf("RunIsolatedBrowserLogin(3papisid_fallback) failed: %v", err)
		}
		parsed := cookies.ParseHeader(res.CookieHeader)
		if parsed["__Secure-3PAPISID"] != "3papisid_ok" || parsed["__Secure-1PSID"] != "psid_3p" {
			t.Fatalf("expected __Secure-3PAPISID fallback cookies captured, got %+v", parsed)
		}
		assertIsolatedLoginArgsHardened(t, argsFile)
	})

	t.Run("clean_exit_after_base_ready_returns_last_result", func(t *testing.T) {
		argsFile := filepath.Join(t.TempDir(), "args.json")
		t.Setenv("FAKE_CDP_ARGS_FILE", argsFile)
		t.Setenv("FAKE_CDP_MODE", "exit_after_base_ready")

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := cookies.RunIsolatedBrowserLogin(ctx, fakeBin)
		if err != nil {
			t.Fatalf("expected clean exit after baseReady to return lastResult, got err=%v", err)
		}
		parsed := cookies.ParseHeader(res.CookieHeader)
		if parsed["SAPISID"] != "sap_exit_base" || parsed["__Secure-1PSID"] != "psid_exit_base" || parsed["__Secure-1PSIDTS"] != "ts_exit_base" {
			t.Fatalf("expected lastResult cookies preserved, got %+v", parsed)
		}
		assertIsolatedLoginArgsHardened(t, argsFile)
	})

	t.Run("non_google_cookie_filtering", func(t *testing.T) {
		argsFile := filepath.Join(t.TempDir(), "args.json")
		t.Setenv("FAKE_CDP_ARGS_FILE", argsFile)
		t.Setenv("FAKE_CDP_MODE", "filter_non_google")

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := cookies.RunIsolatedBrowserLogin(ctx, fakeBin)
		if err != nil {
			t.Fatalf("RunIsolatedBrowserLogin(filter_non_google) failed: %v", err)
		}
		parsed := cookies.ParseHeader(res.CookieHeader)
		if parsed["SAPISID"] != "good_google_sap" || parsed["__Secure-1PSID"] != "good_google_psid" {
			t.Fatalf("expected .google.com cookies captured, got %+v", parsed)
		}
		if parsed["_ga"] != "" || strings.Contains(res.CookieHeader, "evil_") || strings.Contains(res.CookieHeader, "junk_tracking") {
			t.Fatalf("expected non-Google and tracking cookies excluded, got %q", res.CookieHeader)
		}
		assertIsolatedLoginArgsHardened(t, argsFile)
	})
}

func TestE2EIsolatedBrowserLogin_Tier2_CDPLoopbackAndOriginHardening(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Setenv("DISPLAY", ":99")
	}
	fakeBin := buildFakeCDPBrowser(t)

	type tracePayload struct {
		ListCalls  int    `json:"list_calls"`
		WSCalls    int    `json:"ws_calls"`
		LastOrigin string `json:"last_origin"`
		OriginSeen bool   `json:"origin_seen"`
	}

	cases := []struct {
		name         string
		mode         string
		expectWSHit  bool
		verifyOrigin bool
	}{
		{"rejects_non_127_0_0_1_ws_host", "bad_ws_host", false, false},
		{"rejects_localhost_hostname_instead_of_127_0_0_1", "bad_ws_localhost", false, false},
		{"rejects_wss_scheme", "bad_ws_scheme", false, false},
		{"rejects_userinfo_query_and_fragment_in_ws_url", "bad_ws_query_frag_user", false, false},
		{"rejects_non_devtools_ws_path", "bad_ws_path", false, false},
		{"omits_origin_header_and_rejects_non_101_handshake", "ws_non_101", true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			traceFile := filepath.Join(t.TempDir(), "trace.json")
			t.Setenv("FAKE_CDP_TRACE_FILE", traceFile)
			t.Setenv("FAKE_CDP_MODE", tc.mode)

			ctx, cancel := context.WithTimeout(context.Background(), 2400*time.Millisecond)
			defer cancel()
			res, err := cookies.RunIsolatedBrowserLogin(ctx, fakeBin)
			if res != nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s: expected nil result and DeadlineExceeded (CDP rejection), got res=%+v err=%v", tc.name, res, err)
			}

			rawTrace, rErr := os.ReadFile(traceFile)
			if rErr != nil {
				t.Fatalf("%s: expected CDP /json/list to be polled and trace file written: %v", tc.name, rErr)
			}
			var tr tracePayload
			if jErr := json.Unmarshal(rawTrace, &tr); jErr != nil {
				t.Fatalf("%s: unmarshal trace: %v", tc.name, jErr)
			}
			if tr.ListCalls < 1 {
				t.Fatalf("%s: expected >=1 /json/list poll before timeout, got %d", tc.name, tr.ListCalls)
			}
			if tc.expectWSHit {
				if tr.WSCalls < 1 {
					t.Fatalf("%s: expected >=1 WS handshake attempt, got %d", tc.name, tr.WSCalls)
				}
				if tc.verifyOrigin && (tr.OriginSeen || tr.LastOrigin != "") {
					t.Fatalf("%s: expected native CDP WS client to omit Origin header, got origin_seen=%v last_origin=%q", tc.name, tr.OriginSeen, tr.LastOrigin)
				}
			} else if tr.WSCalls != 0 {
				t.Fatalf("%s: expected invalid webSocketDebuggerUrl rejected before WS dial, got ws_calls=%d", tc.name, tr.WSCalls)
			}
		})
	}
}

func TestE2EIsolatedBrowserLogin_Tier2_CDPFrameLoopDomainScoreAndRotationCookies(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Setenv("DISPLAY", ":99")
	}
	fakeBin := buildFakeCDPBrowser(t)

	t.Run("fragmented_and_event", func(t *testing.T) {
		t.Setenv("FAKE_CDP_MODE", "fragmented_and_event")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := cookies.RunIsolatedBrowserLogin(ctx, fakeBin)
		if err != nil {
			t.Fatalf("RunIsolatedBrowserLogin(fragmented_and_event) failed: %v", err)
		}
		parsed := cookies.ParseHeader(res.CookieHeader)
		if parsed["SAPISID"] != "sap_happy" || parsed["__Secure-1PSIDTS"] != "ts_happy" {
			t.Fatalf("expected fragmented CDP response cookies captured, got %+v", parsed)
		}
	})

	t.Run("domain_scoring", func(t *testing.T) {
		t.Setenv("FAKE_CDP_MODE", "domain_scoring")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := cookies.RunIsolatedBrowserLogin(ctx, fakeBin)
		if err != nil {
			t.Fatalf("RunIsolatedBrowserLogin(domain_scoring) failed: %v", err)
		}
		parsed := cookies.ParseHeader(res.CookieHeader)
		if parsed["SAPISID"] != "best_root" {
			t.Fatalf("expected .google.com SAPISID=best_root over accounts.google.com stale_sub, got %q (all=%+v)", parsed["SAPISID"], parsed)
		}
	})

	t.Run("stability_wait_and_rotation_cookies", func(t *testing.T) {
		t.Setenv("FAKE_CDP_MODE", "stability_wait")
		ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
		defer cancel()
		res, err := cookies.RunIsolatedBrowserLogin(ctx, fakeBin)
		if err != nil {
			t.Fatalf("RunIsolatedBrowserLogin(stability_wait) failed: %v", err)
		}
		captured := cookies.ParseHeader(res.CookieHeader)
		for _, reqName := range []string{"SIDCC", "__Secure-1PSIDCC", "__Secure-3PSIDTS"} {
			if captured[reqName] == "" {
				t.Fatalf("expected RunIsolatedBrowserLogin to wait for rotation cookie %s, got %+v", reqName, captured)
			}
		}

		hdr, _, err := cookies.ParseImportPayload(res.CookieHeader)
		if err != nil {
			t.Fatalf("ParseImportPayload failed: %v", err)
		}
		parsed := cookies.ParseHeader(hdr)
		for _, rotName := range []string{"OSID", "__Secure-OSID", "LSID", "__Host-1PLSID", "__Host-3PLSID"} {
			if parsed[rotName] == "" {
				t.Fatalf("expected ParseImportPayload to preserve Google rotation cookie %s, got %+v", rotName, parsed)
			}
		}
		merged, changed := cookies.AbsorbSetCookie(hdr, []string{
			"OSID=osid_updated; Path=/; Domain=.google.com",
			"__Host-1PLSID=h1_updated; Path=/; Secure",
		})
		if !changed {
			t.Fatal("expected AbsorbSetCookie to accept rotation cookie updates")
		}
		mergedMap := cookies.ParseHeader(merged)
		if mergedMap["OSID"] != "osid_updated" || mergedMap["__Host-1PLSID"] != "h1_updated" {
			t.Fatalf("expected AbsorbSetCookie to preserve updated rotation cookies, got %+v", mergedMap)
		}
	})
}

func TestE2EIsolatedBrowserLogin_Tier2_ReaperCancelAndNoDisplay(t *testing.T) {
	fakeBin := buildFakeCDPBrowser(t)

	t.Run("exit_immediately", func(t *testing.T) {
		if runtime.GOOS == "linux" {
			t.Setenv("DISPLAY", ":99")
		}
		argsFile := filepath.Join(t.TempDir(), "args.json")
		t.Setenv("FAKE_CDP_ARGS_FILE", argsFile)
		t.Setenv("FAKE_CDP_MODE", "exit_immediately")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := cookies.RunIsolatedBrowserLogin(ctx, fakeBin)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "closed") {
			t.Fatalf("expected browser-closed error, got %v", err)
		}
		if rawArgs, rErr := os.ReadFile(argsFile); rErr == nil {
			var args []string
			if json.Unmarshal(rawArgs, &args) == nil {
				for _, a := range args {
					if strings.HasPrefix(a, "--user-data-dir=") {
						dir := strings.TrimPrefix(a, "--user-data-dir=")
						if _, stErr := os.Stat(dir); !os.IsNotExist(stErr) {
							t.Fatalf("expected temp dir %s cleaned up, statErr=%v", dir, stErr)
						}
					}
				}
			}
		}
	})

	t.Run("hang_and_cancel", func(t *testing.T) {
		if runtime.GOOS == "linux" {
			t.Setenv("DISPLAY", ":99")
		}
		argsFile := filepath.Join(t.TempDir(), "args.json")
		t.Setenv("FAKE_CDP_ARGS_FILE", argsFile)
		t.Setenv("FAKE_CDP_MODE", "hang")
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, err := cookies.RunIsolatedBrowserLogin(ctx, fakeBin)
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context timeout/cancel error, got %v", err)
		}
		if rawArgs, rErr := os.ReadFile(argsFile); rErr == nil {
			var args []string
			if json.Unmarshal(rawArgs, &args) == nil {
				for _, a := range args {
					if strings.HasPrefix(a, "--user-data-dir=") {
						dir := strings.TrimPrefix(a, "--user-data-dir=")
						if _, stErr := os.Stat(dir); !os.IsNotExist(stErr) {
							t.Fatalf("expected temp dir %s cleaned up on cancel, statErr=%v", dir, stErr)
						}
					}
				}
			}
		}
	})

	if runtime.GOOS == "linux" {
		t.Run("no_display_linux", func(t *testing.T) {
			t.Setenv("DISPLAY", "")
			t.Setenv("WAYLAND_DISPLAY", "")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := cookies.RunIsolatedBrowserLogin(ctx, fakeBin)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "display") {
				t.Fatalf("expected GUI display error when DISPLAY/WAYLAND_DISPLAY empty, got %v", err)
			}
		})
	}
}

// Child process uses the production listener with a test-only token display.
func TestLoginHelperFixtureProcess(t *testing.T) {
	if os.Getenv("GSB_TEST_HELPER_PROCESS") != "1" {
		return
	}
	mode := os.Getenv("GSB_TEST_HELPER_MODE")
	if err := serveLocalLoginHelper(func(token string) error {
		_, err := fmt.Fprintln(os.Stdout, token)
		return err
	}, func(context.Context) (*cookies.IsolatedLoginResult, error) {
		if mode == "ok" {
			return &cookies.IsolatedLoginResult{
				CookieHeader: "SAPISID=helper_ok_sap; __Secure-1PSID=helper_ok_psid",
				UserAgent:    "FakeHelperChrome/150",
			}, nil
		}
		return nil, errors.New("fixture browser unavailable")
	}); err != nil {
		t.Fatalf("serve helper: %v", err)
	}
}

type loginHelperInstance struct {
	addr    string
	baseURL string
	token   string
	logs    *syncBuffer
	stop    func() error
}

func startLoginHelperProcess(t *testing.T, mode string) *loginHelperInstance {
	t.Helper()
	var logs syncBuffer

	// Normal path: 127.0.0.1:20134 is free, so spawn the full TestLoginHelperFixtureProcess subprocess.
	if conn, err := net.DialTimeout("tcp", loginHelperAddr, 150*time.Millisecond); err != nil {
		bin, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, "-test.run=^TestLoginHelperFixtureProcess$")
		env := append(cmd.Environ(), "GSB_TEST_HELPER_PROCESS=1")
		if mode != "" {
			env = append(env, "GSB_TEST_HELPER_MODE="+mode)
		}
		cmd.Env = env
		display, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = &logs
		if err := cmd.Start(); err != nil {
			t.Fatalf("start gsb --login-helper: %v", err)
		}

		pipeDone := make(chan struct{})
		readyToken := make(chan string, 1)
		go func() {
			defer close(pipeDone)
			reader := bufio.NewReader(display)
			line, _ := reader.ReadString('\n')
			readyToken <- strings.TrimSpace(line)
			_, _ = io.Copy(io.Discard, reader)
		}()
		done := make(chan error, 1)
		waitDone := make(chan struct{})
		go func() {
			<-pipeDone
			done <- cmd.Wait()
			close(done)
			close(waitDone)
		}()

		var stopOnce sync.Once
		var stopErr error
		stopFn := func() error {
			stopOnce.Do(func() {
				select {
				case <-waitDone:
					return
				default:
				}
				if cmd.Process != nil {
					_ = cmd.Process.Signal(syscall.SIGTERM)
				}
				select {
				case err := <-done:
					stopErr = err
				case <-time.After(5 * time.Second):
					if cmd.Process != nil {
						_ = cmd.Process.Kill()
					}
					<-waitDone
					stopErr = fmt.Errorf("login helper subprocess did not exit within 5s after SIGTERM")
				}
			})
			return stopErr
		}
		t.Cleanup(func() {
			select {
			case <-waitDone:
				return
			default:
				_ = stopFn()
			}
		})

		var token string
		select {
		case token = <-readyToken:
		case <-time.After(5 * time.Second):
			t.Fatalf("fixture token display timed out; logs:\n%s", logs.String())
		}
		decoded, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(decoded) != 32 {
			t.Fatalf("fixture display did not emit a 256-bit helper token (token=%q); logs:\n%s", token, logs.String())
		}

		deadline := time.Now().Add(5 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			if c, err := net.DialTimeout("tcp", loginHelperAddr, 100*time.Millisecond); err == nil {
				_ = c.Close()
				ready = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !ready {
			t.Fatalf("login helper fixture failed to listen on %s; logs:\n%s", loginHelperAddr, logs.String())
		}
		return &loginHelperInstance{
			addr:    loginHelperAddr,
			baseURL: "http://" + loginHelperAddr,
			token:   token,
			logs:    &logs,
			stop:    stopFn,
		}
	} else {
		_ = conn.Close()
	}

	// Fallback path when an external process already occupies 127.0.0.1:20134:
	// run an in-process http.Server on 127.0.0.1:0 wrapping loginHelperHandler with identical timeouts & shutdown lifecycle.
	token, err := helperToken()
	if err != nil {
		t.Fatalf("helperToken fallback: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fallback helper on 127.0.0.1:0: %v", err)
	}
	loginFn := func(context.Context) (*cookies.IsolatedLoginResult, error) {
		if mode == "ok" {
			return &cookies.IsolatedLoginResult{
				CookieHeader: "SAPISID=helper_ok_sap; __Secure-1PSID=helper_ok_psid",
				UserAgent:    "FakeHelperChrome/150",
			}, nil
		}
		return nil, errors.New("fixture browser unavailable")
	}
	httpSrv := &http.Server{
		Handler:           loginHelperHandler(token, loginFn),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      loginHelperDeadline + 15*time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          log.New(&logs, "", log.LstdFlags),
	}
	serveDone := make(chan error, 1)
	go func() {
		err := httpSrv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveDone <- err
		close(serveDone)
	}()

	var stopOnce sync.Once
	var stopErr error
	stopFn := func() error {
		stopOnce.Do(func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			if err := httpSrv.Shutdown(shutdownCtx); err != nil {
				_ = httpSrv.Close()
				stopErr = err
				<-serveDone
				return
			}
			stopErr = <-serveDone
		})
		return stopErr
	}
	t.Cleanup(func() {
		_ = stopFn()
	})
	addr := ln.Addr().String()
	return &loginHelperInstance{
		addr:    addr,
		baseURL: "http://" + addr,
		token:   token,
		logs:    &logs,
		stop:    stopFn,
	}
}

func TestE2ELoginHelper_Tier1_AuthenticatedHappyPaths(t *testing.T) {
	helper := startLoginHelperProcess(t, "")
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	defer client.CloseIdleConnections()
	if strings.Contains(helper.logs.String(), helper.token) {
		t.Fatal("helper token leaked into process logs")
	}

	t.Run("options_preflight_localhost_origin", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodOptions, helper.baseURL+"/browser-login", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Host = "localhost:20134"
		req.Header.Set("Origin", "http://localhost:20133")
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		req.Header.Set("Access-Control-Request-Headers", "content-type, x-gsb-helper-token")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("OPTIONS failed: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 204/200 on valid localhost OPTIONS, got %d", resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:20133" {
			t.Fatalf("expected ACAO http://localhost:20133, got %q", got)
		}
		if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "Content-Type, "+loginHelperHeader {
			t.Fatalf("unexpected preflight allow headers: %q", got)
		}
	})

	postValid := func(t *testing.T, hostOverride, origin string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, helper.baseURL+"/browser-login", strings.NewReader(`{"id":"acc1"}`))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if hostOverride != "" {
			req.Host = hostOverride
		} else {
			req.Host = loginHelperAddr
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		req.Header.Set(loginHelperHeader, helper.token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /browser-login failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			t.Fatalf("authenticated POST /browser-login rejected with %d: %s", resp.StatusCode, string(body))
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 (fast headless browser-unavailable JSON error) when authenticated, got %d: %s", resp.StatusCode, string(body))
		}
	}

	t.Run("authenticated_post_127_origin", func(t *testing.T) {
		postValid(t, "127.0.0.1:20134", "http://127.0.0.1:20133")
	})
	t.Run("authenticated_post_localhost_origin", func(t *testing.T) {
		postValid(t, "localhost:20134", "http://localhost:20133")
	})
	t.Run("authenticated_post_ipv6_ui_origin", func(t *testing.T) {
		postValid(t, "[::1]:20134", "http://[::1]:20133")
	})

	client.CloseIdleConnections()
	if err := helper.stop(); err != nil {
		t.Fatalf("stop helper before 200 OK subtest: %v\nlogs:\n%s", err, helper.logs.String())
	}

	t.Run("authenticated_post_200_ok_json_and_no_store", func(t *testing.T) {
		okHelper := startLoginHelperProcess(t, "ok")
		okClient := &http.Client{
			Timeout:   3 * time.Second,
			Transport: &http.Transport{DisableKeepAlives: true},
		}
		defer okClient.CloseIdleConnections()

		req, err := http.NewRequest(http.MethodPost, okHelper.baseURL+"/browser-login", strings.NewReader(`{"id":"acc_ok"}`))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Host = loginHelperAddr
		req.Header.Set("Origin", "http://127.0.0.1:20133")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(loginHelperHeader, okHelper.token)

		resp, err := okClient.Do(req)
		if err != nil {
			t.Fatalf("POST /browser-login (ok mode) failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK on GSB_TEST_HELPER_MODE=ok, got %d: %s", resp.StatusCode, string(body))
		}
		if gotCC := resp.Header.Get("Cache-Control"); gotCC != "no-store" {
			t.Fatalf("expected Cache-Control: no-store, got %q", gotCC)
		}
		if gotACAO := resp.Header.Get("Access-Control-Allow-Origin"); gotACAO != "http://127.0.0.1:20133" {
			t.Fatalf("expected Access-Control-Allow-Origin http://127.0.0.1:20133, got %q", gotACAO)
		}
		var payload struct {
			Cookie    string `json:"cookie"`
			UserAgent string `json:"userAgent"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal 200 OK JSON body %q: %v", string(body), err)
		}
		if payload.Cookie != "SAPISID=helper_ok_sap; __Secure-1PSID=helper_ok_psid" || payload.UserAgent != "FakeHelperChrome/150" {
			t.Fatalf("unexpected 200 OK JSON payload: %+v", payload)
		}
		if strings.Contains(okHelper.logs.String(), okHelper.token) {
			t.Fatal("helper token leaked into process logs")
		}
		okClient.CloseIdleConnections()
		if err := okHelper.stop(); err != nil {
			t.Fatalf("stop ok helper: %v\nlogs:\n%s", err, okHelper.logs.String())
		}
	})
}

func TestE2ELoginHelper_Tier2_AuthHostOriginCORSAndWebUI(t *testing.T) {
	helper := startLoginHelperProcess(t, "")

	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	defer client.CloseIdleConnections()
	doReq := func(t *testing.T, method, path, hostOverride, origin string, headers map[string]string, body string) *http.Response {
		t.Helper()
		var bodyReader io.Reader
		if body != "" {
			bodyReader = strings.NewReader(body)
		}
		urlStr := helper.baseURL + path
		req, err := http.NewRequest(method, urlStr, bodyReader)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if hostOverride != "" {
			req.Host = hostOverride
		} else {
			req.Host = loginHelperAddr
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("HTTP %s %s failed: %v", method, urlStr, err)
		}
		return resp
	}

	t.Run("missing_token_rejected", func(t *testing.T) {
		resp := doReq(t, http.MethodPost, "/browser-login", "", "http://127.0.0.1:20133", map[string]string{"Content-Type": "application/json"}, `{"id":"acc1"}`)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 401 or 403, got %d", resp.StatusCode)
		}
	})

	t.Run("wrong_token_rejected", func(t *testing.T) {
		resp := doReq(t, http.MethodPost, "/browser-login", "", "http://127.0.0.1:20133", map[string]string{
			"Content-Type":         "application/json",
			"Authorization":        "Bearer wrong-token",
			"X-GSB-Helper-Token":   "wrong-token",
			"X-Login-Helper-Token": "wrong-token",
			"X-Helper-Token":       "wrong-token",
		}, `{"id":"acc1"}`)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 401 or 403, got %d", resp.StatusCode)
		}
	})

	t.Run("evil_origin_rejected", func(t *testing.T) {
		resp := doReq(t, http.MethodPost, "/browser-login", "", "https://evil.example", map[string]string{"Content-Type": "application/json"}, `{}`)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", resp.StatusCode)
		}
		if acao := resp.Header.Get("Access-Control-Allow-Origin"); acao != "" {
			t.Fatalf("expected empty Access-Control-Allow-Origin, got %q", acao)
		}
	})

	t.Run("evil_host_dns_rebinding_rejected", func(t *testing.T) {
		resp := doReq(t, http.MethodPost, "/browser-login", "evil.example:20134", "http://127.0.0.1:20133", map[string]string{"Content-Type": "application/json"}, `{}`)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", resp.StatusCode)
		}
		if acao := resp.Header.Get("Access-Control-Allow-Origin"); acao != "" {
			t.Fatalf("expected empty Access-Control-Allow-Origin, got %q", acao)
		}
	})

	t.Run("options_evil_origin_rejected", func(t *testing.T) {
		resp := doReq(t, http.MethodOptions, "/browser-login", "", "https://evil.example", nil, "")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", resp.StatusCode)
		}
		if acao := resp.Header.Get("Access-Control-Allow-Origin"); acao != "" {
			t.Fatalf("expected empty Access-Control-Allow-Origin, got %q", acao)
		}
	})

	t.Run("options_valid_origin_cors_headers", func(t *testing.T) {
		resp := doReq(t, http.MethodOptions, "/browser-login", "", "http://127.0.0.1:20133", map[string]string{
			"Access-Control-Request-Method":  http.MethodPost,
			"Access-Control-Request-Headers": "x-gsb-helper-token",
		}, "")
		_ = resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:20133" {
			t.Fatalf("expected ACAO http://127.0.0.1:20133, got %q", got)
		}
		if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "Content-Type, "+loginHelperHeader {
			t.Fatalf("unexpected preflight allow headers: %q", got)
		}
	})

	t.Run("get_method_rejected", func(t *testing.T) {
		resp := doReq(t, http.MethodGet, "/browser-login", "", "http://127.0.0.1:20133", nil, "")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 405/403/401, got %d", resp.StatusCode)
		}
	})

	t.Run("token_never_logged", func(t *testing.T) {
		if strings.Contains(helper.logs.String(), helper.token) {
			t.Fatal("helper token leaked into process logs")
		}
	})

	t.Run("authenticated_post_with_displayed_token_passes_auth", func(t *testing.T) {
		resp := doReq(t, http.MethodPost, "/browser-login", "127.0.0.1:20134", "http://127.0.0.1:20133", map[string]string{loginHelperHeader: helper.token}, "")
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			t.Fatalf("expected valid helper token to pass auth/Origin/Host middleware, got %d: %s", resp.StatusCode, string(body))
		}
	})

	t.Run("slowloris_http_server_timeout", func(t *testing.T) {
		conn, err := net.DialTimeout("tcp", helper.addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial %s: %v", helper.addr, err)
		}
		defer conn.Close()
		// Send incomplete HTTP headers (missing final \r\n) to verify http.Server ReadHeaderTimeout/ReadTimeout.
		if _, err := io.WriteString(conn, "POST /browser-login HTTP/1.1\r\nHost: 127.0.0.1:20134\r\n"); err != nil {
			t.Fatalf("write partial HTTP request: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(32 * time.Second))
		var buf [64]byte
		_, readErr := conn.Read(buf[:])
		if readErr == nil {
			t.Fatal("expected slowloris connection to be closed by server timeout, got data")
		}
		var netErr net.Error
		if errors.As(readErr, &netErr) && netErr.Timeout() {
			t.Fatalf("expected --login-helper http.Server to enforce ReadHeaderTimeout/ReadTimeout within 32s, but connection stayed open: %v", readErr)
		}
	})

	t.Run("webui_btn_isolated_login_handler", func(t *testing.T) {
		htmlBytes, ok := webui.PageHTML()
		if !ok {
			t.Fatal("webui.PageHTML() returned ok=false")
		}
		htmlStr := string(htmlBytes)
		startAnchor := "document.getElementById('btn-isolated-login').addEventListener"
		idx := strings.Index(htmlStr, startAnchor)
		if idx < 0 {
			idx = strings.LastIndex(htmlStr, "btn-isolated-login")
		}
		if idx < 0 {
			t.Fatal("expected btn-isolated-login click handler in webui/index.html")
		}
		endRel := strings.Index(htmlStr[idx:], "document.getElementById('form-import')")
		end := len(htmlStr)
		if endRel > 0 {
			end = idx + endRel
		} else if idx+3500 < end {
			end = idx + 3500
		}
		handlerSnippet := htmlStr[idx:end]
		if !strings.Contains(handlerSnippet, "proxyUrl") || !strings.Contains(handlerSnippet, "accountGroup") {
			t.Fatalf("expected btn-isolated-login click handler to forward proxyUrl and accountGroup (audit 6.2), snippet:\n%s", handlerSnippet)
		}
		helperIdx := strings.Index(handlerSnippet, "127.0.0.1:20134/browser-login")
		if helperIdx < 0 {
			t.Fatalf("expected btn-isolated-login click handler to call 127.0.0.1:20134/browser-login, snippet:\n%s", handlerSnippet)
		}
		helperCallEnd := helperIdx + 500
		if helperCallEnd > len(handlerSnippet) {
			helperCallEnd = len(handlerSnippet)
		}
		helperFetchSnippet := strings.ToLower(handlerSnippet[helperIdx:helperCallEnd])
		if !strings.Contains(helperFetchSnippet, strings.ToLower(loginHelperHeader)) || !strings.Contains(helperFetchSnippet, "redirect: 'error'") {
			t.Fatal("helper fetch must send the dedicated token header and reject redirects")
		}
	})
}

func TestE2E_T3_IsolatedLoginArgsAndHelperTokenWithChromeFallback(t *testing.T) {
	// Cross-feature pairwise Tier 3 test combining 1.4 + 1.5 + 3.4 + 3.7:
	// 1) Isolated browser login captures best-scored cookies without --remote-allow-origins=*
	//    and with full anti-DBSC flags;
	// 2) Chrome SQLite fallback decrypts & validates cookies via single-pass %VAR%;
	// 3) --login-helper enforces per-session token & Host/Origin checks.
	if runtime.GOOS == "linux" {
		t.Setenv("DISPLAY", ":99")
	}
	fakeBin := buildFakeCDPBrowser(t)
	argsFile := filepath.Join(t.TempDir(), "args.json")
	t.Setenv("FAKE_CDP_ARGS_FILE", argsFile)
	t.Setenv("FAKE_CDP_MODE", "domain_scoring")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	isoRes, err := cookies.RunIsolatedBrowserLogin(ctx, fakeBin)
	if err != nil {
		t.Fatalf("RunIsolatedBrowserLogin: %v", err)
	}
	rawArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	var args []string
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	joinedArgs := strings.Join(args, " ")
	if strings.Contains(joinedArgs, "--remote-allow-origins=*") || !strings.Contains(joinedArgs, "--remote-debugging-address=127.0.0.1") {
		t.Fatalf("isolated login args failed 1.4 hardening: %v", args)
	}
	if gotSap := cookies.ExtractSAPISID(isoRes.CookieHeader); gotSap != "best_root" {
		t.Fatalf("expected .google.com SAPISID=best_root, got %q", gotSap)
	}

	// Pair with 3.7 Chrome SQLite fallback using %VAR% single-pass path & AES-128-CBC.
	t.Setenv("CHROME_PASS", "t3_pair_pass")
	profDir := t.TempDir()
	dbPath := filepath.Join(profDir, "Cookies")
	encSap, err := encryptAES128CBCV10("t3_pair_pass", []byte("chrome_t3_sap"))
	if err != nil {
		t.Fatalf("encrypt sap: %v", err)
	}
	encBad, err := encryptAES128CBCV10("t3_pair_pass", []byte{0x00, 0x80, 0xff, 0x01, 0x7f, 0x1b, 0xfe, 0x99, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08})
	if err != nil {
		t.Fatalf("encrypt bad: %v", err)
	}
	createSyntheticChromeCookiesDB(t, dbPath, []struct {
		host      string
		name      string
		encrypted []byte
	}{
		{".google.com", "SAPISID", encSap},
		{".google.com", "__Secure-1PSID", encBad},
	})
	t.Setenv("GSB_T3_PROFILE", profDir)
	resolvedDB, err := cookies.FindBrowserCookieDB("%GSB_T3_PROFILE%/Cookies")
	if err != nil {
		t.Fatalf("FindBrowserCookieDB: %v", err)
	}
	dbRes, err := cookies.ReadBrowserCookies(resolvedDB)
	if err != nil {
		t.Fatalf("ReadBrowserCookies: %v", err)
	}
	if dbRes.Decrypted != 1 || dbRes.Failed != 1 || cookies.ExtractSAPISID(dbRes.Header) != "chrome_t3_sap" {
		t.Fatalf("unexpected Chrome SQLite fallback result: %+v", dbRes)
	}

	// Pair with 1.5 --login-helper token + Origin validation.
	helper := startLoginHelperProcess(t, "")
	hc := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	defer hc.CloseIdleConnections()
	unauthReq, _ := http.NewRequest(http.MethodPost, helper.baseURL+"/browser-login", strings.NewReader(`{"id":"u"}`))
	unauthReq.Host = loginHelperAddr
	unauthReq.Header.Set("Content-Type", "application/json")
	unauthReq.Header.Set("Origin", "http://127.0.0.1:20133")
	unauthResp, err := hc.Do(unauthReq)
	if err != nil {
		t.Fatalf("unauth POST failed: %v", err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized && unauthResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 401/403 without helper token, got %d", unauthResp.StatusCode)
	}

	authReq, _ := http.NewRequest(http.MethodPost, helper.baseURL+"/browser-login", strings.NewReader(`{"id":"u"}`))
	authReq.Host = loginHelperAddr
	authReq.Header.Set("Origin", "http://127.0.0.1:20133")
	authReq.Header.Set(loginHelperHeader, helper.token)
	authResp, err := hc.Do(authReq)
	if err != nil {
		t.Fatalf("auth POST failed: %v", err)
	}
	_ = authResp.Body.Close()
	if authResp.StatusCode == http.StatusUnauthorized || authResp.StatusCode == http.StatusForbidden {
		t.Fatalf("expected valid helper token to pass auth, got %d", authResp.StatusCode)
	}
}

func TestE2ELoginHelper_Tier4_FullOnboardingAndDaemonLifecycle(t *testing.T) {
	// Tier 4 Scenario #9 (TEST_INFRA.md):
	// Full Isolated Browser Login + Chrome SQLite Fallback + --login-helper + Web UI + Daemon Lifecycle
	// exercising 1.4, 1.5, 3.4, 3.7, 6.2, and 6.3b end-to-end.
	gsbBin := buildGSBBinary(t)
	fakeBin := buildFakeCDPBrowser(t)

	// Step 1 (1.4 + 3.4): Isolated browser capture with stability wait & rotation cookies.
	if runtime.GOOS == "linux" {
		t.Setenv("DISPLAY", ":99")
	}
	argsFile := filepath.Join(t.TempDir(), "t4_args.json")
	t.Setenv("FAKE_CDP_ARGS_FILE", argsFile)
	t.Setenv("FAKE_CDP_MODE", "stability_wait")
	ctxIso, cancelIso := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancelIso()
	isoRes, err := cookies.RunIsolatedBrowserLogin(ctxIso, fakeBin)
	if err != nil {
		t.Fatalf("Step 1 RunIsolatedBrowserLogin: %v", err)
	}
	hdr, _, err := cookies.ParseImportPayload(isoRes.CookieHeader)
	if err != nil {
		t.Fatalf("Step 1 ParseImportPayload: %v", err)
	}
	if parsed := cookies.ParseHeader(hdr); parsed["OSID"] == "" || parsed["SIDCC"] == "" {
		t.Fatalf("Step 1 expected rotation cookies preserved, got %+v", parsed)
	}

	// Step 2 (3.7): Chrome SQLite fallback decryption with %VAR% env expansion & octet validation.
	t.Setenv("CHROME_PASS", "t4_chrome_pass")
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "Cookies")
	encSap, err := encryptAES128CBCV10("t4_chrome_pass", []byte("t4_sap_ok"))
	if err != nil {
		t.Fatalf("Step 2 encrypt sap: %v", err)
	}
	encPsid, err := encryptAES128CBCV10("t4_chrome_pass", []byte("t4_psid_ok"))
	if err != nil {
		t.Fatalf("Step 2 encrypt psid: %v", err)
	}
	createSyntheticChromeCookiesDB(t, dbPath, []struct {
		host      string
		name      string
		encrypted []byte
	}{
		{".google.com", "SAPISID", encSap},
		{".google.com", "__Secure-1PSID", encPsid},
	})
	t.Setenv("GSB_T4_DB", dbDir)
	resolvedDB, err := cookies.FindBrowserCookieDB("%GSB_T4_DB%/Cookies")
	if err != nil {
		t.Fatalf("Step 2 FindBrowserCookieDB: %v", err)
	}
	dbRes, err := cookies.ReadBrowserCookies(resolvedDB)
	if err != nil || dbRes.Decrypted != 2 || cookies.ExtractSAPISID(dbRes.Header) != "t4_sap_ok" {
		t.Fatalf("Step 2 ReadBrowserCookies failed: res=%+v err=%v", dbRes, err)
	}

	// Step 3 (1.5 + 6.2): --login-helper concurrent unauth/evil rejections, valid-token acceptance, Web UI wiring, and SIGTERM shutdown.
	helper := startLoginHelperProcess(t, "")

	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	defer client.CloseIdleConnections()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := range 12 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, helper.baseURL+"/browser-login", strings.NewReader(`{"id":"u"}`))
			if err != nil {
				errs <- err
				return
			}
			req.Header.Set("Content-Type", "application/json")
			switch idx % 3 {
			case 0:
				req.Host = loginHelperAddr
				req.Header.Set("Origin", "http://127.0.0.1:20133")
			case 1:
				req.Host = loginHelperAddr
				req.Header.Set("Origin", "https://evil.example")
			case 2:
				req.Host = "evil.example:20134"
				req.Header.Set("Origin", "http://127.0.0.1:20133")
			}
			resp, err := client.Do(req)
			if err != nil {
				errs <- err
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
				errs <- fmt.Errorf("req %d expected 401/403, got %d", idx, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}

	if strings.Contains(helper.logs.String(), helper.token) {
		t.Fatal("Step 3 helper token leaked into logs")
	}
	validReq, _ := http.NewRequest(http.MethodPost, helper.baseURL+"/browser-login", strings.NewReader(`{"id":"u"}`))
	validReq.Host = loginHelperAddr
	validReq.Header.Set("Origin", "http://127.0.0.1:20133")
	validReq.Header.Set(loginHelperHeader, helper.token)
	validResp, err := client.Do(validReq)
	if err != nil {
		t.Fatalf("Step 3 valid POST failed: %v", err)
	}
	_ = validResp.Body.Close()
	if validResp.StatusCode == http.StatusUnauthorized || validResp.StatusCode == http.StatusForbidden {
		t.Fatalf("Step 3 valid-token POST rejected with %d", validResp.StatusCode)
	}

	htmlBytes, ok := webui.PageHTML()
	if !ok {
		t.Fatal("Step 3 webui.PageHTML() returned ok=false")
	}
	htmlStr := string(htmlBytes)
	hStart := strings.Index(htmlStr, "document.getElementById('btn-isolated-login').addEventListener")
	if hStart < 0 {
		t.Fatal("Step 3 missing btn-isolated-login addEventListener in webui/index.html")
	}
	hEndRel := strings.Index(htmlStr[hStart:], "document.getElementById('form-import')")
	if hEndRel <= 0 {
		t.Fatal("Step 3 missing form-import anchor after btn-isolated-login")
	}
	hSnippet := htmlStr[hStart : hStart+hEndRel]
	if !strings.Contains(hSnippet, "proxyUrl") || !strings.Contains(hSnippet, "accountGroup") || (!strings.Contains(strings.ToLower(hSnippet), "token") && !strings.Contains(strings.ToLower(hSnippet), "authorization")) {
		t.Fatalf("Step 3 btn-isolated-login handler must forward proxyUrl, accountGroup, and helper token, got:\n%s", hSnippet)
	}

	client.CloseIdleConnections()
	if err := helper.stop(); err != nil {
		t.Fatalf("Step 3 helper shutdown failed: %v\nlogs:\n%s", err, helper.logs.String())
	}

	// Step 4 (6.3b): Save captured credential, start gsb daemon with secret BRIDGE_API_KEY, verify log redaction & session-cache.json flush on SIGTERM.
	home := t.TempDir()
	acctPath := filepath.Join(home, "accounts", "onboarded.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:    "onboarded",
		Email:   "onboarded@example.com",
		Cookies: hdr,
	}); err != nil {
		t.Fatalf("Step 4 SaveCredentialFile: %v", err)
	}

	port := freeTCPPort(t)
	secretKey := "sk-e2e-t4-secret-987654321"
	var daemonLogs syncBuffer
	dCmd := exec.Command(gsbBin)
	dCmd.Stdout = &daemonLogs
	dCmd.Stderr = &daemonLogs
	dCmd.Env = append(os.Environ(),
		"BRIDGE_HOME="+home,
		"BRIDGE_API_KEY="+secretKey,
		"BRIDGE_HOST=127.0.0.1",
		fmt.Sprintf("BRIDGE_PORT=%d", port),
		"BRIDGE_WEB_UI=0",
	)
	if err := dCmd.Start(); err != nil {
		t.Fatalf("Step 4 start gsb daemon: %v", err)
	}
	waitDaemon := make(chan error, 1)
	waitDaemonDone := make(chan struct{})
	go func() {
		waitDaemon <- dCmd.Wait()
		close(waitDaemon)
		close(waitDaemonDone)
	}()
	defer func() {
		select {
		case <-waitDaemonDone:
			return
		default:
		}
		if dCmd.Process != nil {
			_ = dCmd.Process.Kill()
		}
		<-waitDaemonDone
	}()

	hcClient := &http.Client{
		Timeout:   500 * time.Millisecond,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	defer hcClient.CloseIdleConnections()
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	hDeadline := time.Now().Add(10 * time.Second)
	healthy := false
	for time.Now().Before(hDeadline) {
		resp, err := hcClient.Get(healthURL)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !healthy {
		t.Fatalf("Step 4 daemon not healthy at %s; logs:\n%s", healthURL, daemonLogs.String())
	}
	hcClient.CloseIdleConnections()
	if err := dCmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Step 4 daemon SIGTERM: %v", err)
	}
	select {
	case err := <-waitDaemon:
		if err != nil {
			t.Fatalf("Step 4 daemon exited with error: %v\nlogs:\n%s", err, daemonLogs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Step 4 daemon did not exit within 10s after SIGTERM\nlogs:\n%s", daemonLogs.String())
	}

	if strings.Contains(daemonLogs.String(), secretKey) {
		t.Fatal("Step 4 daemon logs leaked plaintext BRIDGE_API_KEY")
	}
	cachePath := filepath.Join(home, "session-cache.json")
	cacheBytes, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("Step 4 expected session-cache.json flushed on graceful shutdown: %v", err)
	}
	var parsedCache any
	if err := json.Unmarshal(cacheBytes, &parsedCache); err != nil {
		t.Fatalf("Step 4 session-cache.json invalid JSON: %v", err)
	}
}

func encryptAES128CBCV10(pass string, plaintext []byte) ([]byte, error) {
	key128 := pbkdf2.Key([]byte(pass), []byte("saltysalt"), 1, 16, sha1.New)
	block, err := aes.NewCipher(key128)
	if err != nil {
		return nil, err
	}
	padLen := aes.BlockSize - (len(plaintext) % aes.BlockSize)
	padded := make([]byte, len(plaintext)+padLen)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	iv := bytes.Repeat([]byte{0x20}, aes.BlockSize)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	return append([]byte("v10"), ct...), nil
}

func createSyntheticChromeCookiesDB(t *testing.T, dbPath string, rows []struct {
	host      string
	name      string
	encrypted []byte
}) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE cookies (host_key TEXT, name TEXT, encrypted_value BLOB, value TEXT, has_expires INTEGER, expires_utc INTEGER, is_persistent INTEGER);`); err != nil {
		t.Fatalf("create cookies table: %v", err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO cookies (host_key, name, encrypted_value, value, has_expires, expires_utc, is_persistent) VALUES (?, ?, ?, '', 0, 0, 1);`, r.host, r.name, r.encrypted); err != nil {
			t.Fatalf("insert cookie row: %v", err)
		}
	}
}

func encryptAES256GCMV11(pass string, plaintext []byte) ([]byte, error) {
	key256 := pbkdf2.Key([]byte(pass), []byte("saltysalt"), 1, 32, sha1.New)
	block, err := aes.NewCipher(key256)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, 3+len(nonce)+len(sealed))
	out = append(out, "v11"...)
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

func TestE2EChromeSQLite_Tier1_HappyDecryptAndSinglePassEnv(t *testing.T) {
	t.Run("aes128cbc_v10_happy_decrypt", func(t *testing.T) {
		t.Setenv("CHROME_PASS", "happy_pass_128")
		encSap, err := encryptAES128CBCV10("happy_pass_128", []byte("sap_happy_128"))
		if err != nil {
			t.Fatalf("encrypt sap: %v", err)
		}
		encPsid, err := encryptAES128CBCV10("happy_pass_128", []byte("psid_happy_128"))
		if err != nil {
			t.Fatalf("encrypt psid: %v", err)
		}
		encTs, err := encryptAES128CBCV10("happy_pass_128", []byte("ts_happy_128"))
		if err != nil {
			t.Fatalf("encrypt ts: %v", err)
		}
		dbPath := filepath.Join(t.TempDir(), "Cookies")
		createSyntheticChromeCookiesDB(t, dbPath, []struct {
			host      string
			name      string
			encrypted []byte
		}{
			{".google.com", "SAPISID", encSap},
			{".google.com", "__Secure-1PSID", encPsid},
			{".google.com", "__Secure-1PSIDTS", encTs},
		})
		res, err := cookies.ReadBrowserCookies(dbPath)
		if err != nil {
			t.Fatalf("ReadBrowserCookies: %v", err)
		}
		if res.Decrypted != 3 || res.Failed != 0 || !res.KeyFound {
			t.Fatalf("expected Decrypted=3 Failed=0 KeyFound=true, got %+v", res)
		}
		parsed := cookies.ParseHeader(res.Header)
		if parsed["SAPISID"] != "sap_happy_128" || parsed["__Secure-1PSID"] != "psid_happy_128" || parsed["__Secure-1PSIDTS"] != "ts_happy_128" {
			t.Fatalf("unexpected decrypted header: %+v", parsed)
		}
	})

	t.Run("aes256gcm_v11_happy_decrypt", func(t *testing.T) {
		t.Setenv("CHROME_PASS", "happy_pass_256")
		encSap, err := encryptAES256GCMV11("happy_pass_256", []byte("sap_gcm_256"))
		if err != nil {
			t.Fatalf("encrypt gcm sap: %v", err)
		}
		encPsid, err := encryptAES256GCMV11("happy_pass_256", []byte("psid_gcm_256"))
		if err != nil {
			t.Fatalf("encrypt gcm psid: %v", err)
		}
		dbPath := filepath.Join(t.TempDir(), "Cookies")
		createSyntheticChromeCookiesDB(t, dbPath, []struct {
			host      string
			name      string
			encrypted []byte
		}{
			{".google.com", "SAPISID", encSap},
			{".google.com", "__Secure-1PSID", encPsid},
		})
		res, err := cookies.ReadBrowserCookies(dbPath)
		if err != nil {
			t.Fatalf("ReadBrowserCookies(v11): %v", err)
		}
		if res.Decrypted != 2 || res.Failed != 0 {
			t.Fatalf("expected Decrypted=2 Failed=0, got %+v", res)
		}
		if got := cookies.ExtractSAPISID(res.Header); got != "sap_gcm_256" {
			t.Fatalf("expected SAPISID=sap_gcm_256, got %q", got)
		}
	})

	t.Run("chrome130_domain_prefix_stripped", func(t *testing.T) {
		t.Setenv("CHROME_PASS", "chrome130_pass")
		prefix := sha256.Sum256([]byte(".google.com"))
		plainWithPrefix := append(prefix[:], []byte("bound_sapisid_val")...)
		enc, err := encryptAES128CBCV10("chrome130_pass", plainWithPrefix)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		dbPath := filepath.Join(t.TempDir(), "Cookies")
		createSyntheticChromeCookiesDB(t, dbPath, []struct {
			host      string
			name      string
			encrypted []byte
		}{
			{".google.com", "SAPISID", enc},
		})
		res, err := cookies.ReadBrowserCookies(dbPath)
		if err != nil {
			t.Fatalf("ReadBrowserCookies: %v", err)
		}
		if res.DomainBound != 1 || cookies.ExtractSAPISID(res.Header) != "bound_sapisid_val" {
			t.Fatalf("expected DomainBound=1 and stripped SAPISID=bound_sapisid_val, got %+v", res)
		}
	})

	t.Run("firefox_moz_cookies_happy_read", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "cookies.sqlite")
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		if _, err := db.Exec(`CREATE TABLE moz_cookies (host TEXT, name TEXT, value TEXT, expiry INTEGER);`); err != nil {
			_ = db.Close()
			t.Fatalf("create moz_cookies: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO moz_cookies (host, name, value, expiry) VALUES ('.google.com', 'SAPISID', 'ff_sap', 0), ('.google.com', '__Secure-1PSID', 'ff_psid', 0);`); err != nil {
			_ = db.Close()
			t.Fatalf("insert moz_cookies: %v", err)
		}
		_ = db.Close()
		res, err := cookies.ReadBrowserCookies(dbPath)
		if err != nil {
			t.Fatalf("ReadBrowserCookies(firefox): %v", err)
		}
		if res.Plain != 2 || cookies.ExtractSAPISID(res.Header) != "ff_sap" {
			t.Fatalf("unexpected firefox read result: %+v", res)
		}
	})

	t.Run("single_pass_percent_env_and_domain_scoring", func(t *testing.T) {
		t.Setenv("CHROME_PASS", "env_happy_pass")
		rootDir := t.TempDir()
		subDir := filepath.Join(rootDir, "Profile_One")
		if err := os.MkdirAll(subDir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		dbPath := filepath.Join(subDir, "Cookies")
		encSub, err := encryptAES128CBCV10("env_happy_pass", []byte("sub_sap"))
		if err != nil {
			t.Fatalf("encrypt sub: %v", err)
		}
		encRoot, err := encryptAES128CBCV10("env_happy_pass", []byte("root_sap"))
		if err != nil {
			t.Fatalf("encrypt root: %v", err)
		}
		createSyntheticChromeCookiesDB(t, dbPath, []struct {
			host      string
			name      string
			encrypted []byte
		}{
			{"accounts.google.com", "SAPISID", encSub},
			{".google.com", "SAPISID", encRoot},
		})
		t.Setenv("GSB_ROOT", rootDir)
		t.Setenv("GSB_PROF", "Profile_One")
		resolved, err := cookies.FindBrowserCookieDB("%GSB_ROOT%/%GSB_PROF%/Cookies")
		if err != nil || resolved != dbPath {
			t.Fatalf("FindBrowserCookieDB got %q err=%v, want %q", resolved, err, dbPath)
		}
		hdr, err := cookies.ReadChromeCookies(resolved)
		if err != nil {
			t.Fatalf("ReadChromeCookies: %v", err)
		}
		if got := cookies.ExtractSAPISID(hdr); got != "root_sap" {
			t.Fatalf("expected .google.com root_sap over accounts.google.com sub_sap, got %q", got)
		}
	})

	if runtime.GOOS != "windows" {
		t.Run("keyring_command_happy_lookup", func(t *testing.T) {
			t.Setenv("CHROME_PASS", "")
			binDir := t.TempDir()
			scriptBody := "#!/bin/sh\necho 'keyring_happy_secret'\nexit 0\n"
			for _, cmdName := range []string{"secret-tool", "security"} {
				p := filepath.Join(binDir, cmdName)
				if err := os.WriteFile(p, []byte(scriptBody), 0o700); err != nil {
					t.Fatalf("write fake %s: %v", cmdName, err)
				}
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			enc, err := encryptAES128CBCV10("keyring_happy_secret", []byte("kr_sap_ok"))
			if err != nil {
				t.Fatalf("encrypt: %v", err)
			}
			dbPath := filepath.Join(t.TempDir(), "Cookies")
			createSyntheticChromeCookiesDB(t, dbPath, []struct {
				host      string
				name      string
				encrypted []byte
			}{
				{".google.com", "SAPISID", enc},
			})
			res, err := cookies.ReadBrowserCookies(dbPath)
			if err != nil {
				t.Fatalf("ReadBrowserCookies(keyring): %v", err)
			}
			if res.Decrypted != 1 || cookies.ExtractSAPISID(res.Header) != "kr_sap_ok" {
				t.Fatalf("expected keyring-decrypted SAPISID=kr_sap_ok, got %+v", res)
			}
		})
	}
}

func TestE2EChromeSQLite_Tier2_SinglePassEnvOctetCheckAndKeyringTimeout(t *testing.T) {
	t.Run("expandPercentEnv_single_pass_no_infinite_loop", func(t *testing.T) {
		t.Setenv("GSB_LOOP", "%GSB_LOOP%")
		t.Setenv("GSB_A", "%GSB_B%")
		t.Setenv("GSB_B", "%GSB_A%")

		done := make(chan error, 1)
		go func() {
			_, err := cookies.FindBrowserCookieDB("%GSB_LOOP%/Cookies")
			done <- err
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("FindBrowserCookieDB hung on self-referential env expansion for %s", "%GSB_LOOP%")
		}

		done2 := make(chan error, 1)
		go func() {
			_, err := cookies.FindBrowserCookieDB("%GSB_A%/Cookies")
			done2 <- err
		}()
		select {
		case <-done2:
		case <-time.After(2 * time.Second):
			t.Fatalf("FindBrowserCookieDB hung on mutually recursive env expansion for %s/%s", "%GSB_A%", "%GSB_B%")
		}

		tmpDir := t.TempDir()
		literalSub := filepath.Join(tmpDir, "%LITERAL%")
		if err := os.MkdirAll(literalSub, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		cookieFile := filepath.Join(literalSub, "Cookies")
		if err := os.WriteFile(cookieFile, []byte("sqlite"), 0o600); err != nil {
			t.Fatalf("write dummy cookies file: %v", err)
		}
		t.Setenv("GSB_DIR", literalSub)
		t.Setenv("LITERAL", "SHOULD_NOT_EXPAND_TWICE")
		resolved, err := cookies.FindBrowserCookieDB("%GSB_DIR%/Cookies")
		if err != nil {
			t.Fatalf("single-pass env expansion failed to resolve %s: %v", cookieFile, err)
		}
		if resolved != cookieFile {
			t.Fatalf("got resolved=%q, want %q", resolved, cookieFile)
		}
	})

	t.Run("aes128cbc_cookie_octet_validation", func(t *testing.T) {
		t.Setenv("CHROME_PASS", "testpass")
		validEnc, err := encryptAES128CBCV10("testpass", []byte("valid_sapisid_123"))
		if err != nil {
			t.Fatalf("encrypt valid: %v", err)
		}
		// 15 bytes containing control/non-ASCII bytes + 1 byte PKCS#7 pad (0x01)
		garbagePlain := []byte{0x00, 0xff, 0x80, 0x01, 0x7f, 0x1b, 0xfe, 0x99, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
		garbageEnc, err := encryptAES128CBCV10("testpass", garbagePlain)
		if err != nil {
			t.Fatalf("encrypt garbage: %v", err)
		}

		dbPath := filepath.Join(t.TempDir(), "Cookies")
		createSyntheticChromeCookiesDB(t, dbPath, []struct {
			host      string
			name      string
			encrypted []byte
		}{
			{".google.com", "SAPISID", validEnc},
			{".google.com", "__Secure-1PSID", garbageEnc},
		})

		res, err := cookies.ReadBrowserCookies(dbPath)
		if err != nil {
			t.Fatalf("ReadBrowserCookies failed: %v", err)
		}
		if res.Decrypted != 1 || res.Failed != 1 {
			t.Fatalf("expected Decrypted=1, Failed=1 (binary garbage rejected), got Decrypted=%d Failed=%d", res.Decrypted, res.Failed)
		}
		if got := cookies.ExtractSAPISID(res.Header); got != "valid_sapisid_123" {
			t.Fatalf("expected SAPISID=valid_sapisid_123, got %q (header=%q)", got, res.Header)
		}
		if cookies.ParseHeader(res.Header)["__Secure-1PSID"] != "" {
			t.Fatalf("expected binary garbage __Secure-1PSID excluded from header, got %q", res.Header)
		}
	})

	if runtime.GOOS != "windows" {
		t.Run("keyring_command_10s_timeout", func(t *testing.T) {
			t.Setenv("CHROME_PASS", "")
			binDir := t.TempDir()
			marker := filepath.Join(binDir, "slept.once")
			scriptBody := fmt.Sprintf("#!/bin/sh\nif [ ! -f %q ]; then\n  touch %q\n  exec sleep 30\nfi\nexit 1\n", marker, marker)
			for _, cmdName := range []string{"secret-tool", "security"} {
				p := filepath.Join(binDir, cmdName)
				if err := os.WriteFile(p, []byte(scriptBody), 0o700); err != nil {
					t.Fatalf("write fake %s: %v", cmdName, err)
				}
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			validEnc, err := encryptAES128CBCV10("peanuts", []byte("peanuts_sapisid"))
			if err != nil {
				t.Fatalf("encrypt: %v", err)
			}
			dbPath := filepath.Join(t.TempDir(), "Cookies")
			createSyntheticChromeCookiesDB(t, dbPath, []struct {
				host      string
				name      string
				encrypted []byte
			}{
				{".google.com", "SAPISID", validEnc},
			})

			start := time.Now()
			done := make(chan error, 1)
			go func() {
				_, rErr := cookies.ReadBrowserCookies(dbPath)
				done <- rErr
			}()
			select {
			case rErr := <-done:
				elapsed := time.Since(start)
				if rErr != nil {
					t.Fatalf("ReadBrowserCookies returned error after keyring timeout: %v", rErr)
				}
				if elapsed >= 15*time.Second {
					t.Fatalf("expected 10s keyring command timeout to abort 30s sleep (<15s), took %s", elapsed)
				}
			case <-time.After(18 * time.Second):
				t.Fatal("ReadBrowserCookies hung past 18s (missing keyring command timeout)")
			}
		})
	}

	t.Run("corrupt_or_truncated_v11_aes256gcm_rejected", func(t *testing.T) {
		t.Setenv("CHROME_PASS", "v11_testpass")
		validV11, err := encryptAES256GCMV11("v11_testpass", []byte("v11_sap_ok"))
		if err != nil {
			t.Fatalf("encrypt v11 valid: %v", err)
		}
		tamperedV11, err := encryptAES256GCMV11("v11_testpass", []byte("v11_tampered"))
		if err != nil {
			t.Fatalf("encrypt v11 tampered: %v", err)
		}
		tamperedV11[len(tamperedV11)-1] ^= 0xff
		truncatedV11 := []byte("v11short")

		dbPath := filepath.Join(t.TempDir(), "Cookies")
		createSyntheticChromeCookiesDB(t, dbPath, []struct {
			host      string
			name      string
			encrypted []byte
		}{
			{".google.com", "SAPISID", validV11},
			{".google.com", "__Secure-1PSID", tamperedV11},
			{".google.com", "__Secure-1PSIDTS", truncatedV11},
		})

		res, err := cookies.ReadBrowserCookies(dbPath)
		if err != nil {
			t.Fatalf("ReadBrowserCookies(mixed v11): %v", err)
		}
		if res.Decrypted != 1 || res.Failed != 2 {
			t.Fatalf("expected Decrypted=1 Failed=2 for corrupt/truncated v11 rows, got %+v", res)
		}
		if got := cookies.ExtractSAPISID(res.Header); got != "v11_sap_ok" {
			t.Fatalf("expected valid SAPISID=v11_sap_ok, got %q", got)
		}

		// When all encrypted cookies fail v11 GCM authentication, ReadChromeCookies must return an error.
		failOnlyDB := filepath.Join(t.TempDir(), "CookiesFailOnly")
		createSyntheticChromeCookiesDB(t, failOnlyDB, []struct {
			host      string
			name      string
			encrypted []byte
		}{
			{".google.com", "SAPISID", tamperedV11},
			{".google.com", "__Secure-1PSID", truncatedV11},
		})
		if hdr, err := cookies.ReadChromeCookies(failOnlyDB); err == nil || hdr != "" {
			t.Fatalf("expected ReadChromeCookies error when all v11 rows fail decrypt, got hdr=%q err=%v", hdr, err)
		}
	})

	t.Run("nonexistent_or_corrupt_sqlite_db_returns_error", func(t *testing.T) {
		tmp := t.TempDir()
		missingDB := filepath.Join(tmp, "missing", "Cookies")
		if _, err := cookies.FindBrowserCookieDB(missingDB); err == nil {
			t.Fatalf("expected FindBrowserCookieDB to fail for missing path %s", missingDB)
		}
		if _, err := cookies.ReadBrowserCookies(missingDB); err == nil {
			t.Fatalf("expected ReadBrowserCookies to fail for missing DB %s", missingDB)
		}
		corruptDB := filepath.Join(tmp, "CorruptCookies")
		if err := os.WriteFile(corruptDB, []byte("not-a-valid-sqlite-database"), 0o600); err != nil {
			t.Fatalf("write corrupt db: %v", err)
		}
		if _, err := cookies.ReadBrowserCookies(corruptDB); err == nil {
			t.Fatalf("expected ReadBrowserCookies to fail for corrupt non-sqlite file %s", corruptDB)
		}
	})
}

func TestE2ECLIParentDaemon_Tier2_RedactAPIKeyAndFlushSessionCacheOnShutdown(t *testing.T) {
	gsbBin := buildGSBBinary(t)
	home := t.TempDir()
	acctPath := filepath.Join(home, "accounts", "acc1.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:    "acc1",
		Email:   "acc1@example.com",
		Cookies: "SAPISID=123; __Secure-1PSID=psid1; __Secure-1PSIDTS=ts1",
	}); err != nil {
		t.Fatalf("SaveCredentialFile: %v", err)
	}

	port := freeTCPPort(t)
	secretKey := "sk-e2e-secret-987654321"

	var logs syncBuffer
	cmd := exec.Command(gsbBin)
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	cmd.Env = append(os.Environ(),
		"BRIDGE_HOME="+home,
		"BRIDGE_API_KEY="+secretKey,
		"BRIDGE_HOST=127.0.0.1",
		fmt.Sprintf("BRIDGE_PORT=%d", port),
		"BRIDGE_WEB_UI=0",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gsb daemon: %v", err)
	}
	waitCh := make(chan error, 1)
	waitDone := make(chan struct{})
	go func() {
		waitCh <- cmd.Wait()
		close(waitCh)
		close(waitDone)
	}()
	defer func() {
		select {
		case <-waitDone:
			return
		default:
		}
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-waitDone
	}()

	hcClient := &http.Client{
		Timeout:   500 * time.Millisecond,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	defer hcClient.CloseIdleConnections()
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(10 * time.Second)
	healthy := false
	for time.Now().Before(deadline) {
		resp, err := hcClient.Get(healthURL)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !healthy {
		t.Fatalf("gsb daemon did not become healthy on %s; logs:\n%s", healthURL, logs.String())
	}
	hcClient.CloseIdleConnections()

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM daemon: %v", err)
	}
	select {
	case err := <-waitCh:
		if err != nil {
			t.Fatalf("daemon exited with error: %v\nlogs:\n%s", err, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("daemon did not shut down within 10s after SIGTERM\nlogs:\n%s", logs.String())
	}

	logOutput := logs.String()
	if strings.Contains(logOutput, secretKey) {
		t.Fatal("daemon startup/shutdown logs leaked plaintext BRIDGE_API_KEY")
	}

	cachePath := filepath.Join(home, "session-cache.json")
	cacheBytes, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("expected session-cache.json flushed on graceful shutdown at %s: %v\nlogs:\n%s", cachePath, err, logOutput)
	}
	var parsed any
	if err := json.Unmarshal(cacheBytes, &parsed); err != nil {
		t.Fatalf("session-cache.json is not valid JSON: %v (raw=%q)", err, string(cacheBytes))
	}
}
