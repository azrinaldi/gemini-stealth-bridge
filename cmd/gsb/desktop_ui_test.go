package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
)

const fixtureDesktopKey = "fixture-desktop-key"

func TestDesktopUISignIn(t *testing.T) {
	for _, tc := range []struct {
		name         string
		origin       string
		remoteStatus int
		wantStatus   int
		wantLogin    bool
	}{
		{"cross-origin", "http://127.0.0.1:20135", 200, 403, false},
		{"missing origin", "", 200, 200, true},
		{"admin denied", "http://" + desktopUIAddr, 401, 401, false},
		{"success", "http://" + desktopUIAddr, 200, 200, true},
		{"upstream rate limited", "http://" + desktopUIAddr, 200, 429, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const cookie = "SAPISID=fixture"
			imports := make(chan desktopLoginOptions, 1)
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("Authorization") != "Bearer fixture-admin" {
					t.Error("admin authorization not passed through")
				}
				switch r.URL.Path {
				case desktopConfigPath:
					w.WriteHeader(tc.remoteStatus)
					fmt.Fprint(w, `{}`)
				case desktopImportPath:
					var payload struct {
						desktopLoginOptions
						Cookie string `json:"cookie"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Cookie != cookie {
						t.Error("import did not receive captured session")
					}
					imports <- payload.desktopLoginOptions
					if tc.wantStatus == http.StatusTooManyRequests {
						w.Header().Set("Retry-After", "120")
					}
					w.WriteHeader(tc.wantStatus)
					if tc.wantStatus == http.StatusTooManyRequests {
						fmt.Fprint(w, `{"error":{"message":"cookie rotation rate limited (HTTP 429)"}}`)
					} else {
						fmt.Fprint(w, `{"account":{"id":"desktop-test"}}`)
					}
				default:
					t.Error("unexpected remote route")
					http.NotFound(w, r)
				}
			}))
			defer remote.Close()
			target, err := url.Parse(remote.URL)
			if err != nil {
				t.Fatal(err)
			}
			var launched bool
			h := newDesktopUIHandler(desktopUIAddr, *target, remote.Client().Transport, func(context.Context) (*cookies.IsolatedLoginResult, error) {
				launched = true
				return &cookies.IsolatedLoginResult{CookieHeader: cookie}, nil
			})
			r := httptest.NewRequest(http.MethodPost, desktopLoginPath, strings.NewReader(`{"id":"desktop-test","accountGroup":"team"}`))
			r.Host = desktopUIAddr
			r.RemoteAddr = "127.0.0.1:45000"
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer fixture-admin")
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.wantStatus || launched != tc.wantLogin {
				t.Fatalf("status=%d launch=%v; want %d and %v", w.Code, launched, tc.wantStatus, tc.wantLogin)
			}
			if strings.Contains(w.Body.String(), cookie) {
				t.Fatal("browser received captured session")
			}
			select {
			case got := <-imports:
				if !tc.wantLogin || got.ID != "desktop-test" || got.AccountGroup != "team" {
					t.Fatalf("unexpected import options: %+v", got)
				}
			default:
				if tc.wantLogin {
					t.Fatal("expected remote import")
				}
			}
			if tc.wantStatus == http.StatusTooManyRequests {
				if !strings.Contains(w.Body.String(), "cookie rotation rate limited") || w.Header().Get("Retry-After") != "120" {
					t.Fatalf("desktop response lost rate-limit cause or cooldown: %s, Retry-After=%q", w.Body.String(), w.Header().Get("Retry-After"))
				}
			}
		})
	}
}

func TestDesktopUIProxiesAPIRoutesOnSamePort(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer fixture-admin" {
			t.Fatalf("unexpected proxied request: %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gemini-4-hydrogen"}]}`)
	}))
	defer remote.Close()
	target, err := url.Parse(remote.URL)
	if err != nil {
		t.Fatal(err)
	}
	h := newDesktopUIHandler(desktopUIAddr, *target, remote.Client().Transport, nil)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Host = desktopUIAddr
	r.RemoteAddr = "127.0.0.1:45000"
	r.Header.Set("Authorization", "Bearer fixture-admin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "gemini-4-hydrogen") {
		t.Fatalf("same-port API proxy response: %d %s", w.Code, w.Body.String())
	}
}

func TestDesktopUIRejectsRemoteHTML(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<script>alert(1)</script>`)
	}))
	defer remote.Close()
	target, err := url.Parse(remote.URL)
	if err != nil {
		t.Fatal(err)
	}
	h := newDesktopUIHandler(desktopUIAddr, *target, remote.Client().Transport, nil)
	r := httptest.NewRequest(http.MethodGet, desktopConfigPath, nil)
	r.Host = desktopUIAddr
	r.RemoteAddr = "127.0.0.1:45000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "<script>") {
		t.Fatalf("unexpected desktop response: %d %s", w.Code, w.Body.String())
	}
}

func TestDesktopUICSRFBlocksAllRoutesAndForwardsAPIHeaders(t *testing.T) {
	var seenHeaders http.Header
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer remote.Close()
	target, err := url.Parse(remote.URL)
	if err != nil {
		t.Fatal(err)
	}
	h := newDesktopUIHandler(desktopUIAddr, *target, remote.Client().Transport, nil)

	for _, tc := range []struct {
		name         string
		method, path string
		origin       string
		secFetchSite string
		wantStatus   int
	}{
		{"cross-origin GET admin", http.MethodGet, desktopConfigPath, "http://evil.example", "", http.StatusForbidden},
		{"cross-origin POST chat", http.MethodPost, "/v1/chat/completions", "http://evil.example", "", http.StatusForbidden},
		{"cross-origin POST login with query bypass attempt", http.MethodPost, desktopLoginPath + "?x=1", "http://evil.example", "", http.StatusForbidden},
		{"sec-fetch-site cross-site", http.MethodGet, desktopConfigPath, "", "cross-site", http.StatusForbidden},
		{"sec-fetch-site same-site", http.MethodGet, desktopConfigPath, "", "same-site", http.StatusForbidden},
		{"same-origin allowed", http.MethodGet, desktopConfigPath, "http://" + desktopUIAddr, "same-origin", http.StatusOK},
		{"non-browser api client allowed", http.MethodPost, "/mcp", "", "", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seenHeaders = nil
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Host = desktopUIAddr
			r.RemoteAddr = "127.0.0.1:45000"
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.secFetchSite != "" {
				r.Header.Set("Sec-Fetch-Site", tc.secFetchSite)
			}
			r.Header.Set("X-Api-Key", "k1")
			r.Header.Set("X-Goog-Api-Key", "gk1")
			r.Header.Set("Accept", "text/event-stream")
			r.Header.Set("If-None-Match", `"etag1"`)
			r.Header.Set("Mcp-Session-Id", "mcp-123")
			r.Header.Set("MCP-Protocol-Version", "2025-03-26")

			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.wantStatus {
				t.Fatalf("got status %d, want %d (body=%s)", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantStatus == http.StatusForbidden && seenHeaders != nil {
				t.Fatal("cross-site request reached upstream proxy")
			}
			if tc.wantStatus == http.StatusOK {
				for k, want := range map[string]string{
					"X-Api-Key":            "k1",
					"X-Goog-Api-Key":       "gk1",
					"Accept":               "text/event-stream",
					"If-None-Match":        `"etag1"`,
					"Mcp-Session-Id":       "mcp-123",
					"MCP-Protocol-Version": "2025-03-26",
				} {
					if got := seenHeaders.Get(k); got != want {
						t.Errorf("forwarded header %s = %q, want %q", k, got, want)
					}
				}
			}
		})
	}
}
