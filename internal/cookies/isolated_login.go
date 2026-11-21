package cookies

import (
	"bufio"
	"bytes"
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
// Native CDP requests use loopback and send no browser Origin header, so the
// browser does not need a remote-allow-origins override.
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
		"--remote-debugging-address=127.0.0.1",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=DeviceBoundSessionCredentials,BoundSessionCredentials,EnableBoundSessionCredentials,DeviceBoundSessions",
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
	procDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(procDone)
	}()
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-procDone
	}()

	const (
		pollInterval       = 1 * time.Second
		cookieStableWindow = 1200 * time.Millisecond
		cookieGraceCap     = 5 * time.Second
	)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var firstReadyAt, lastChangeAt time.Time
	var lastResult *IsolatedLoginResult

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-procDone:
			if lastResult != nil {
				return lastResult, nil
			}
			return nil, fmt.Errorf("browser window was closed before Google sign-in completed")
		case <-ticker.C:
			hdr, ua, baseReady, rotReady := pollIsolatedCDPCookies(ctx, port)
			if !baseReady {
				continue
			}
			now := time.Now()
			if firstReadyAt.IsZero() {
				firstReadyAt = now
			}
			if lastResult == nil || hdr != lastResult.CookieHeader {
				lastResult = &IsolatedLoginResult{
					CookieHeader: hdr,
					UserAgent:    ua,
				}
				lastChangeAt = now
			}
			if (rotReady && now.Sub(lastChangeAt) >= cookieStableWindow) || now.Sub(firstReadyAt) >= cookieGraceCap {
				return lastResult, nil
			}
		}
	}
}

func isGoogleCookieDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" {
		return false
	}
	if strings.HasPrefix(d, ".") {
		d = d[1:]
	}
	return d == "google.com" || (strings.HasSuffix(d, ".google.com") && !strings.HasPrefix(d, "."))
}

func extractIsolatedCookies(rawCookies []domainCookie) (cookieHeader string, baseReady, rotReady bool) {
	filtered := make([]domainCookie, 0, len(rawCookies))
	for _, c := range rawCookies {
		if !isGoogleCookieDomain(c.Domain) || c.Name == "" || c.Value == "" {
			continue
		}
		filtered = append(filtered, c)
	}
	best, hdr := selectBestDomainCookieMap(filtered)
	baseReady = (best["SAPISID"] != "" || best["__Secure-3PAPISID"] != "") &&
		best["__Secure-1PSID"] != "" &&
		best["__Secure-1PSIDTS"] != ""
	if !baseReady {
		return "", false, false
	}
	rotReady = best["SIDCC"] != "" && best["__Secure-1PSIDCC"] != "" && best["__Secure-3PSIDTS"] != ""
	return hdr, true, rotReady
}

// CDP discovery must not consult environment proxies or follow redirects.
var isolatedCDPClient = &http.Client{
	Transport: &http.Transport{DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func pollIsolatedCDPCookies(ctx context.Context, port int) (cookieHeader, userAgent string, baseReady, rotReady bool) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	verURL := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	reqVer, err := http.NewRequestWithContext(cctx, http.MethodGet, verURL, nil)
	if err != nil {
		return "", "", false, false
	}
	respVer, err := isolatedCDPClient.Do(reqVer)
	if err != nil {
		return "", "", false, false
	}
	var verInfo struct {
		UserAgent            string `json:"User-Agent"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	verErr := json.NewDecoder(io.LimitReader(respVer.Body, 1<<20)).Decode(&verInfo)
	_ = respVer.Body.Close()
	if respVer.StatusCode != http.StatusOK || verErr != nil {
		return "", "", false, false
	}

	listURL := fmt.Sprintf("http://127.0.0.1:%d/json/list", port)
	reqList, err := http.NewRequestWithContext(cctx, http.MethodGet, listURL, nil)
	if err != nil {
		return "", "", false, false
	}
	respList, err := isolatedCDPClient.Do(reqList)
	if err != nil {
		return "", "", false, false
	}
	var targets []struct {
		Type                 string `json:"type"`
		URL                  string `json:"url"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	listErr := json.NewDecoder(io.LimitReader(respList.Body, 1<<20)).Decode(&targets)
	_ = respList.Body.Close()
	if respList.StatusCode != http.StatusOK || listErr != nil {
		return "", "", false, false
	}

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
		return "", "", false, false
	}
	if wsURL == "" {
		wsURL = verInfo.WebSocketDebuggerURL
	}
	endpoint, err := url.Parse(wsURL)
	if err != nil || endpoint.Scheme != "ws" || endpoint.Host != net.JoinHostPort("127.0.0.1", fmt.Sprint(port)) ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || !strings.HasPrefix(endpoint.Path, "/devtools/") {
		return "", "", false, false
	}

	rawJSON, err := cdpCallStdlibWS(cctx, wsURL, `{"id":1,"method":"Network.getAllCookies"}`)
	if err != nil {
		return "", "", false, false
	}

	var reply struct {
		ID     int `json:"id"`
		Result struct {
			Cookies []domainCookie `json:"cookies"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rawJSON, &reply); err != nil {
		return "", "", false, false
	}

	hdr, baseReady, rotReady := extractIsolatedCookies(reply.Result.Cookies)
	if !baseReady {
		return "", "", false, false
	}
	return hdr, verInfo.UserAgent, true, rotReady
}

func writeMaskedWSFrame(w io.Writer, firstByte byte, msg []byte) error {
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	header := []byte{firstByte}
	switch {
	case len(msg) < 126:
		header = append(header, byte(0x80|len(msg)))
	case len(msg) <= 65535:
		header = append(header, 0x80|126, byte(len(msg)>>8), byte(len(msg)))
	default:
		return fmt.Errorf("payload too large")
	}
	header = append(header, mask[:]...)
	masked := make([]byte, len(msg))
	for i := range msg {
		masked[i] = msg[i] ^ mask[i%4]
	}
	_, err := w.Write(append(header, masked...))
	return err
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
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	dl := time.Now().Add(2 * time.Second)
	if ctxDL, ok := ctx.Deadline(); ok && ctxDL.Before(dl) {
		dl = ctxDL
	}
	_ = conn.SetDeadline(dl)

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

	wantID := 1
	var reqProbe struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal([]byte(payload), &reqProbe); err == nil && reqProbe.ID != 0 {
		wantID = reqProbe.ID
	}

	// Send masked text frame (opcode 0x81)
	if err := writeMaskedWSFrame(conn, 0x81, []byte(payload)); err != nil {
		return nil, err
	}

	const maxWSMessageBytes = 16 * 1024 * 1024
	var assembled bytes.Buffer
	inFragmented := false

	for {
		var hdr [2]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return nil, err
		}
		fin := (hdr[0] & 0x80) != 0
		opcode := hdr[0] & 0x0f
		masked := (hdr[1] & 0x80) != 0
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
		if length > maxWSMessageBytes {
			return nil, fmt.Errorf("ws frame too large: %d", length)
		}
		var frameMask [4]byte
		if masked {
			if _, err := io.ReadFull(br, frameMask[:]); err != nil {
				return nil, err
			}
		}
		frameData := make([]byte, length)
		if _, err := io.ReadFull(br, frameData); err != nil {
			return nil, err
		}
		if masked {
			for i := range frameData {
				frameData[i] ^= frameMask[i%4]
			}
		}

		switch opcode {
		case 0x08: // Close
			return nil, io.EOF
		case 0x09: // Ping
			if err := writeMaskedWSFrame(conn, 0x8A, frameData); err != nil {
				return nil, err
			}
			continue
		case 0x0A: // Pong
			continue
		case 0x01, 0x02: // Text / Binary start
			assembled.Reset()
			assembled.Write(frameData)
			inFragmented = !fin
		case 0x00: // Continuation
			if !inFragmented {
				return nil, fmt.Errorf("unexpected ws continuation frame")
			}
			if uint64(assembled.Len())+length > maxWSMessageBytes {
				return nil, fmt.Errorf("ws assembled message too large")
			}
			assembled.Write(frameData)
			if fin {
				inFragmented = false
			}
		default:
			continue
		}

		if fin && !inFragmented {
			msg := append([]byte(nil), assembled.Bytes()...)
			assembled.Reset()
			var probe struct {
				ID int `json:"id"`
			}
			if err := json.Unmarshal(msg, &probe); err == nil && probe.ID == wantID {
				return msg, nil
			}
		}
	}
}
