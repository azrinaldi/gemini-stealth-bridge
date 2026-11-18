package cookies

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// IsolatedLoginResult holds the cookie header and User-Agent captured from an
// isolated temporary browser login session.
type IsolatedLoginResult struct {
	CookieHeader string
	UserAgent    string
}

// FindBrowserBinary locates an installed Chromium-family browser executable on
// Windows, macOS, or Linux. When browser is empty, it defaults to Chrome/Chromium.
func FindBrowserBinary(browser string) string {
	b := strings.ToLower(strings.TrimSpace(browser))
	if b == "" {
		b = "chrome"
	}
	switch runtime.GOOS {
	case "windows":
		roots := []string{
			os.Getenv("ProgramFiles"),
			os.Getenv("ProgramFiles(x86)"),
			os.Getenv("LOCALAPPDATA"),
		}
		var rels []string
		switch b {
		case "brave":
			rels = []string{`BraveSoftware\Brave-Browser\Application\brave.exe`}
		case "edge":
			rels = []string{`Microsoft\Edge\Application\msedge.exe`}
		case "vivaldi":
			rels = []string{`Vivaldi\Application\vivaldi.exe`}
		case "chromium":
			rels = []string{`Chromium\Application\chrome.exe`}
		case "arc":
			for _, root := range roots {
				if root == "" {
					continue
				}
				matches, _ := filepath.Glob(filepath.Join(root, "Packages", "TheBrowserCompany.Arc_*", "LocalCache", "Local", "Arc", "Arc.exe"))
				if len(matches) > 0 {
					return matches[0]
				}
			}
		default:
			rels = []string{`Google\Chrome\Application\chrome.exe`}
		}
		for _, r := range roots {
			if r == "" {
				continue
			}
			for _, rel := range rels {
				cand := filepath.Join(r, rel)
				if st, err := os.Stat(cand); err == nil && !st.IsDir() {
					return cand
				}
			}
		}
	case "darwin":
		var appBins []string
		switch b {
		case "brave":
			appBins = []string{"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser"}
		case "edge":
			appBins = []string{"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"}
		case "vivaldi":
			appBins = []string{"/Applications/Vivaldi.app/Contents/MacOS/Vivaldi"}
		case "chromium":
			appBins = []string{"/Applications/Chromium.app/Contents/MacOS/Chromium"}
		case "arc":
			appBins = []string{"/Applications/Arc.app/Contents/MacOS/Arc"}
		default:
			appBins = []string{
				"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
				"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
				"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
				"/Applications/Chromium.app/Contents/MacOS/Chromium",
			}
		}
		for _, c := range appBins {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return c
			}
		}
	}
	var names []string
	switch b {
	case "brave":
		names = []string{"brave-browser", "brave"}
	case "edge":
		names = []string{"microsoft-edge", "microsoft-edge-stable"}
	case "vivaldi":
		names = []string{"vivaldi", "vivaldi-stable"}
	case "chromium":
		names = []string{"chromium", "chromium-browser"}
	case "arc":
		names = []string{"arc", "arc-browser"}
	default:
		names = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "brave-browser", "microsoft-edge"}
	}
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// isolatedLoginArgs builds the disposable-profile browser command line.
//
// Device Bound Session Credentials are disabled on purpose. When Chrome
// registers a DBSC binding, Google only refreshes the short-lived session
// cookies for a client that can prove possession of the browser's key. gsb
// holds cookies, not that key, so /RotateCookies is rejected and the imported
// session dies within hours. A browser that never registers DBSC leaves the
// session renewable, which is what makes the captured cookies durable.
func isolatedLoginArgs(profileDir string, port int) []string {
	return []string{
		fmt.Sprintf("--user-data-dir=%s", profileDir),
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--remote-allow-origins=*",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=DeviceBoundSessionCredentials",
		"--new-window",
		"https://gemini.google.com/app",
	}
}

// RunIsolatedBrowserLogin launches a clean, temporary Chromium/Chrome instance
// with its own disposable --user-data-dir and CDP debugging port, waits for the
// user to complete Google sign-in at https://gemini.google.com/app, extracts
// the resulting session cookies via CDP, and immediately terminates the browser
// and deletes the temporary profile directory so no browser process ever
// competes with gsb for __Secure-1PSIDTS cookie rotation.
func RunIsolatedBrowserLogin(ctx context.Context, browserBin string) (*IsolatedLoginResult, error) {
	if browserBin == "" {
		browserBin = FindBrowserBinary("")
	}
	if browserBin == "" {
		return nil, fmt.Errorf("no Chromium/Chrome browser binary found on this machine")
	}
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return nil, fmt.Errorf("this gsb server has no GUI display ($DISPLAY); either run `gsb --login <name>` on your laptop or use Option 1 below (open Incognito Ctrl+Shift+N -> sign in -> copy cookies -> close the Incognito window)")
	}

	tmpProfile, err := os.MkdirTemp("", "gsb-isolated-login-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary browser profile: %w", err)
	}
	defer os.RemoveAll(tmpProfile)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("allocate local CDP port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	cmd := exec.CommandContext(ctx, browserBin, isolatedLoginArgs(tmpProfile, port)...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launch browser %s: %w", browserBin, err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	procDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(procDone)
	}()

	ticker := time.NewTicker(1200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-procDone:
			return nil, fmt.Errorf("browser window was closed before Google sign-in completed")
		case <-ticker.C:
			hdr, ua, ok := pollIsolatedCDPCookies(ctx, port)
			if ok {
				return &IsolatedLoginResult{
					CookieHeader: hdr,
					UserAgent:    ua,
				}, nil
			}
		}
	}
}

func pollIsolatedCDPCookies(ctx context.Context, port int) (cookieHeader, userAgent string, ready bool) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	verURL := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	reqVer, err := http.NewRequestWithContext(cctx, http.MethodGet, verURL, nil)
	if err != nil {
		return "", "", false
	}
	respVer, err := http.DefaultClient.Do(reqVer)
	if err != nil {
		return "", "", false
	}
	var verInfo struct {
		UserAgent            string `json:"User-Agent"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	_ = json.NewDecoder(respVer.Body).Decode(&verInfo)
	_ = respVer.Body.Close()

	listURL := fmt.Sprintf("http://127.0.0.1:%d/json/list", port)
	reqList, err := http.NewRequestWithContext(cctx, http.MethodGet, listURL, nil)
	if err != nil {
		return "", "", false
	}
	respList, err := http.DefaultClient.Do(reqList)
	if err != nil {
		return "", "", false
	}
	var targets []struct {
		Type                 string `json:"type"`
		URL                  string `json:"url"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	_ = json.NewDecoder(respList.Body).Decode(&targets)
	_ = respList.Body.Close()

	wsURL := ""
	onGeminiApp := false
	for _, t := range targets {
		if t.Type == "page" && strings.HasPrefix(t.URL, "https://gemini.google.com/") && !strings.Contains(t.URL, "accounts.google.com") {
			onGeminiApp = true
			if t.WebSocketDebuggerURL != "" {
				wsURL = t.WebSocketDebuggerURL
				break
			}
		}
	}
	if !onGeminiApp {
		return "", "", false
	}
	if wsURL == "" {
		wsURL = verInfo.WebSocketDebuggerURL
	}
	if wsURL == "" {
		return "", "", false
	}

	rawJSON, err := cdpCallStdlibWS(cctx, wsURL, `{"id":1,"method":"Network.getAllCookies"}`)
	if err != nil {
		return "", "", false
	}

	var reply struct {
		ID     int `json:"id"`
		Result struct {
			Cookies []struct {
				Name   string `json:"name"`
				Value  string `json:"value"`
				Domain string `json:"domain"`
			} `json:"cookies"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rawJSON, &reply); err != nil {
		return "", "", false
	}

	seen := make(map[string]string)
	var parts []string
	for _, c := range reply.Result.Cookies {
		if !strings.HasSuffix(c.Domain, "google.com") || c.Name == "" || c.Value == "" {
			continue
		}
		if _, exists := seen[c.Name]; !exists {
			seen[c.Name] = c.Value
			parts = append(parts, c.Name+"="+c.Value)
		}
	}

	if seen["SAPISID"] == "" && seen["__Secure-3PAPISID"] == "" {
		return "", "", false
	}
	if seen["__Secure-1PSID"] == "" || seen["__Secure-1PSIDTS"] == "" {
		return "", "", false
	}

	return strings.Join(parts, "; "), verInfo.UserAgent, true
}

func cdpCallStdlibWS(ctx context.Context, wsURL string, payload string) ([]byte, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	var keyBytes [16]byte
	_, _ = rand.Read(keyBytes[:])
	wsKey := base64.StdEncoding.EncodeToString(keyBytes[:])

	reqStr := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", u.RequestURI(), u.Host, wsKey)
	if _, err := conn.Write([]byte(reqStr)); err != nil {
		return nil, err
	}

	br := bufio.NewReaderSize(conn, 65536)
	statusLine, err := br.ReadString('\n')
	if err != nil || !strings.Contains(statusLine, "101") {
		return nil, fmt.Errorf("ws handshake failed: %s", strings.TrimSpace(statusLine))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// Send masked text frame (opcode 0x81)
	msg := []byte(payload)
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	header := []byte{0x81}
	switch {
	case len(msg) < 126:
		header = append(header, byte(0x80|len(msg)))
	case len(msg) <= 65535:
		header = append(header, 0x80|126, byte(len(msg)>>8), byte(len(msg)))
	default:
		return nil, fmt.Errorf("payload too large")
	}
	header = append(header, mask[:]...)
	masked := make([]byte, len(msg))
	for i := range msg {
		masked[i] = msg[i] ^ mask[i%4]
	}
	if _, err := conn.Write(append(header, masked...)); err != nil {
		return nil, err
	}

	// Read unmasked server frame
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return nil, err
	}
	length := uint64(hdr[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > 16*1024*1024 {
		return nil, fmt.Errorf("ws frame too large: %d", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(br, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
