package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
)

const (
	desktopUIAddr     = "127.0.0.1:20133"
	desktopBridgeAddr = "127.0.0.1:20133"
	desktopLoginPath  = "/v1/admin/accounts/browser-login"
	desktopImportPath = "/v1/admin/accounts/import"
	desktopConfigPath = "/v1/admin/config"
)

func runDesktopUI() error {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if !filepath.IsAbs(runtimeDir) {
		return errors.New("desktop UI requires an absolute XDG_RUNTIME_DIR")
	}
	socket := filepath.Join(runtimeDir, "gsb-desktop", "bridge.sock")
	dialer := net.Dialer{Timeout: 4 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	target := url.URL{Scheme: "http", Host: desktopBridgeAddr}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
		IdleConnTimeout: 30 * time.Second,
	}
	defer transport.CloseIdleConnections()
	ln, err := net.Listen("tcp4", desktopUIAddr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler: newDesktopUIHandler(ln.Addr().String(), target, transport, func(ctx context.Context) (*cookies.IsolatedLoginResult, error) {
			return cookies.RunIsolatedBrowserLogin(ctx, "")
		}),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      loginHelperDeadline + 85*time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	log.Printf("[desktop] proxying daemon UI and API at http://%s/ui", desktopUIAddr)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			_ = srv.Close()
			return err
		}
		return nil
	}
}

type desktopLoginOptions struct {
	ID           string `json:"id"`
	ProxyURL     string `json:"proxyUrl"`
	AccountGroup string `json:"accountGroup"`
}

func newDesktopUIHandler(authority string, target url.URL, transport http.RoundTripper, login func(context.Context) (*cookies.IsolatedLoginResult, error)) http.Handler {
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(&target)
			p.Out.Header = make(http.Header, 8)
			copyDesktopHeaders(p.Out.Header, p.In.Header)
		},
		ModifyResponse: func(res *http.Response) error {
			if res.Request == nil || res.Request.URL.Path != desktopConfigPath {
				return nil
			}
			contentType, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
			if contentType != "application/json" || res.StatusCode >= 300 && res.StatusCode < 400 {
				return errors.New("unexpected response from bridge")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			desktopError(w, http.StatusBadGateway, "Cannot reach the bridge over the SSH tunnel.")
		},
	}
	loginMu := make(chan struct{}, 1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, _, _ := net.SplitHostPort(r.RemoteAddr)
		if net.ParseIP(peer) == nil || !net.ParseIP(peer).IsLoopback() || r.Host != authority || r.URL.IsAbs() || r.URL.RawPath != "" {
			desktopError(w, http.StatusForbidden, "Open the GSB UI at its local address.")
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) > 1 || len(origins) == 1 && origins[0] != "http://"+authority {
			desktopError(w, http.StatusForbidden, "Open the GSB UI at its local address.")
			return
		}
		for _, site := range r.Header.Values("Sec-Fetch-Site") {
			s := strings.ToLower(strings.TrimSpace(site))
			if s == "cross-site" || s == "same-site" {
				desktopError(w, http.StatusForbidden, "Open the GSB UI at its local address.")
				return
			}
		}
		if r.Method != http.MethodPost || r.URL.Path != desktopLoginPath || r.URL.RawQuery != "" || r.URL.ForceQuery {
			proxy.ServeHTTP(w, r)
			return
		}
		contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if contentType != "application/json" {
			desktopError(w, http.StatusUnsupportedMediaType, "Send login options as JSON.")
			return
		}
		var opts *desktopLoginOptions
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
		if err != nil || json.Unmarshal(data, &opts) != nil || opts == nil || opts.ID == "." || opts.ID == ".." || opts.ID != "" && !reSafeAccountName.MatchString(opts.ID) {
			desktopError(w, http.StatusBadRequest, "Invalid login options or account name.")
			return
		}
		select {
		case loginMu <- struct{}{}:
			defer func() { <-loginMu }()
		default:
			desktopError(w, http.StatusConflict, "A sign-in window is already open.")
			return
		}
		checkCtx, cancelCheck := context.WithTimeout(r.Context(), 5*time.Second)
		check, err := http.NewRequestWithContext(checkCtx, http.MethodGet, target.String()+desktopConfigPath, nil)
		if err != nil {
			cancelCheck()
			desktopError(w, http.StatusBadGateway, "Invalid bridge tunnel address.")
			return
		}
		copyDesktopHeaders(check.Header, r.Header)
		res, err := transport.RoundTrip(check)
		cancelCheck()
		if err != nil {
			desktopError(w, http.StatusBadGateway, "Cannot reach the bridge over the SSH tunnel.")
			return
		}
		_ = res.Body.Close()
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusTooManyRequests {
			desktopError(w, res.StatusCode, "The bridge admin API did not authorize sign-in.")
			return
		}
		if res.StatusCode != http.StatusOK {
			desktopError(w, http.StatusBadGateway, "The bridge admin API is unavailable over the SSH tunnel.")
			return
		}
		loginCtx, cancelLogin := context.WithTimeout(r.Context(), loginHelperDeadline)
		session, err := login(loginCtx)
		cancelLogin()
		if err != nil || session == nil || session.CookieHeader == "" {
			desktopError(w, http.StatusBadRequest, "Desktop sign-in did not complete. Check the browser window, then try again.")
			return
		}
		payload, _ := json.Marshal(struct {
			desktopLoginOptions
			Cookie string `json:"cookie"`
		}{*opts, session.CookieHeader})
		importReq := r.Clone(r.Context())
		importReq.URL = &url.URL{Path: desktopImportPath}
		importReq.Body = io.NopCloser(bytes.NewReader(payload))
		importReq.TransferEncoding = nil
		importReq.ContentLength = int64(len(payload))
		proxy.ServeHTTP(w, importReq)
	})
}

func copyDesktopHeaders(dst, src http.Header) {
	for _, name := range []string{
		"Authorization",
		"X-GSB-Admin-Key",
		"X-Api-Key",
		"X-Goog-Api-Key",
		"Content-Type",
		"Accept",
		"If-None-Match",
		"Mcp-Session-Id",
		"MCP-Protocol-Version",
		"Origin",
		"Sec-Fetch-Site",
	} {
		if value := src.Get(name); value != "" {
			dst.Set(name, value)
		}
	}
}

func desktopError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message}})
}
