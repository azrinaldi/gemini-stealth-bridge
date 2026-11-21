package main

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
)

func TestSingleWriterLock(t *testing.T) {
	dir := t.TempDir()

	lock1, err := config.AcquireLock(dir)
	if err != nil {
		t.Fatalf("first AcquireLock failed: %v", err)
	}
	defer func() { _ = lock1.Release() }()

	lock2, err2 := config.AcquireLock(dir)
	if err2 == nil {
		_ = lock2.Release()
		t.Fatal("expected second AcquireLock on same BRIDGE_HOME to fail immediately")
	}
	if !strings.Contains(err2.Error(), "already locked") || !strings.Contains(err2.Error(), "pid=") {
		t.Fatalf("expected error to mention already locked and pid=, got: %v", err2)
	}

	if err := lock1.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	lock3, err3 := config.AcquireLock(dir)
	if err3 != nil {
		t.Fatalf("expected AcquireLock after Release to succeed, got: %v", err3)
	}
	_ = lock3.Release()
}

func TestRunUnquarantineClearsStateAndPreservesOtherSurfaces(t *testing.T) {
	home := t.TempDir()
	acctsDir := filepath.Join(home, "accounts")
	cfg := config.Config{
		HomeDir:     home,
		AccountsDir: acctsDir,
	}
	acctPath := filepath.Join(acctsDir, "acct1.json")
	_, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:             "acct1",
		Email:            "acct1@example.com",
		Cookies:          "SAPISID=123",
		Quarantined:      true,
		QuarantineReason: "TOS_VIOLATION",
		Surfaces: map[string]string{
			"geminiWeb":   "blocked",
			"antigravity": "ok",
		},
	})
	if err != nil {
		t.Fatalf("SaveCredentialFile: %v", err)
	}

	if err := runUnquarantine(cfg, "acct1"); err != nil {
		t.Fatalf("runUnquarantine failed: %v", err)
	}

	loaded, _, err := cookies.LoadAccountFile(acctPath)
	if err != nil {
		t.Fatalf("LoadAccountFile: %v", err)
	}
	if loaded.Quarantined || loaded.QuarantineReason != "" {
		t.Fatalf("expected Quarantined cleared, got %+v", loaded)
	}
	if loaded.Surfaces["geminiWeb"] != "ok" || loaded.Surfaces["antigravity"] != "ok" {
		t.Fatalf("expected geminiWeb=ok and antigravity=ok preserved, got %+v", loaded.Surfaces)
	}
}

func TestLoginHelperRequestPolicy(t *testing.T) {
	const origin = "http://127.0.0.1:20133"
	const token = "fixture-token"
	for _, tc := range []struct {
		name, method, host, peer, origin, token, preflightMethod, preflightHeaders string
		status                                                                    int
		cors, login                                                               bool
	}{
		{name: "authorized", method: "POST", origin: origin, token: token, status: 200, cors: true, login: true},
		{name: "preflight", method: "OPTIONS", origin: origin, preflightMethod: "POST", preflightHeaders: "content-type, X-GSB-Helper-Token", status: 204, cors: true},
		{name: "preflight wrong method", method: "OPTIONS", origin: origin, preflightMethod: "GET", status: 403},
		{name: "preflight wrong header", method: "OPTIONS", origin: origin, preflightMethod: "POST", preflightHeaders: "authorization", status: 403},
		{name: "missing origin", method: "POST", token: token, status: 403},
		{name: "wrong origin port", method: "POST", origin: "http://127.0.0.1:3000", token: token, status: 403},
		{name: "origin path", method: "POST", origin: origin + "/ui", token: token, status: 403},
		{name: "non-loopback host", method: "POST", host: "remote.example:20134", origin: origin, token: token, status: 403},
		{name: "non-loopback peer", method: "POST", peer: "192.0.2.1:53421", origin: origin, token: token, status: 403},
		{name: "no token", method: "POST", origin: origin, status: 401, cors: true},
		{name: "wrong token", method: "POST", origin: origin, token: "other-fixture-token", status: 401, cors: true},
		{name: "wrong method", method: "GET", origin: origin, token: token, status: 405, cors: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			h := loginHelperHandler(token, func(ctx context.Context) (*cookies.IsolatedLoginResult, error) {
				calls++
				if _, ok := ctx.Deadline(); !ok {
					t.Error("browser login context missing deadline")
				}
				return &cookies.IsolatedLoginResult{CookieHeader: "fixture=session"}, nil
			})
			r := httptest.NewRequest(tc.method, "http://"+loginHelperAddr+"/browser-login", nil)
			r.RemoteAddr = "127.0.0.1:53421"
			if tc.peer != "" {
				r.RemoteAddr = tc.peer
			}
			if tc.host != "" {
				r.Host = tc.host
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.token != "" {
				r.Header.Set(loginHelperHeader, tc.token)
			}
			r.Header.Set("X-Forwarded-For", "127.0.0.1")
			r.Header.Set("Access-Control-Request-Method", tc.preflightMethod)
			if tc.preflightHeaders != "" {
				r.Header.Set("Access-Control-Request-Headers", tc.preflightHeaders)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || (calls == 1) != tc.login {
				t.Fatalf("status=%d, login calls=%d; want status=%d, login=%v", w.Code, calls, tc.status, tc.login)
			}
			acao := w.Header().Get("Access-Control-Allow-Origin")
			if (tc.cors && acao != origin) || (!tc.cors && acao != "") {
				t.Errorf("unexpected CORS origin %q", acao)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Error("helper response must not be cached")
			}
			if strings.Contains(w.Body.String(), "fixture=session") != tc.login {
				t.Error("unexpected fixture session visibility")
			}
		})
	}
}
