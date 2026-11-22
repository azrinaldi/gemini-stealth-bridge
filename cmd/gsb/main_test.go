package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
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

func TestStageAndPersistAccountImport_DoesNotClobberOnBootstrapFailure(t *testing.T) {
	home := t.TempDir()
	acctsDir := filepath.Join(home, "accounts")
	cfg := config.Config{HomeDir: home, AccountsDir: acctsDir}
	acctPath := filepath.Join(acctsDir, "acct1.json")

	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:     "acct1",
		Email:    "old@example.com",
		Cookies:  "SAPISID=old_sap; __Secure-1PSIDTS=old_ts",
		AuthUser: 2,
	}); err != nil {
		t.Fatal(err)
	}

	// Failing bootstrap must leave existing acct1.json completely untouched.
	_, _, err := stageAndPersistAccountImport(cfg, "acct1", "SAPISID=new_sap; __Secure-1PSIDTS=new_ts", "chrome", "Default", func(_ context.Context, ac *gemini.AccountClient) error {
		if ac.ReloadFromDiskIfNewer() {
			t.Error("staged client must not reload old disk file during bootstrap")
		}
		if !strings.Contains(ac.CookieHeader(), "new_ts") {
			t.Errorf("expected staged client to retain new_ts, got %q", ac.CookieHeader())
		}
		return errors.New("bootstrap rejected")
	})
	if err == nil {
		t.Fatal("expected error on failed bootstrap")
	}
	diskAfterFail, _, err := cookies.LoadAccountFile(acctPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diskAfterFail.Cookies, "old_ts") || diskAfterFail.Email != "old@example.com" {
		t.Fatalf("failed bootstrap modified existing account file: %+v", diskAfterFail)
	}

	// Succeeding bootstrap atomically updates acct1.json and preserves AuthUser.
	_, _, err = stageAndPersistAccountImport(cfg, "acct1", "SAPISID=new_sap; __Secure-1PSIDTS=new_ts", "chrome", "Default", func(_ context.Context, ac *gemini.AccountClient) error {
		ac.SetTokens("at_new", "bl_new", "fsid_new")
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error on succeeding bootstrap: %v", err)
	}
	diskAfterOK, _, err := cookies.LoadAccountFile(acctPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diskAfterOK.Cookies, "new_ts") || diskAfterOK.AT != "at_new" || diskAfterOK.AuthUser != 2 {
		t.Fatalf("expected atomic update preserving AuthUser=2, got %+v", diskAfterOK)
	}
}

func TestLoginHelperHandler_SingleFlightConflict(t *testing.T) {
	const origin = "http://127.0.0.1:20133"
	const token = "fixture-token"
	entered := make(chan struct{})
	release := make(chan struct{})
	h := loginHelperHandler(token, func(ctx context.Context) (*cookies.IsolatedLoginResult, error) {
		close(entered)
		<-release
		return &cookies.IsolatedLoginResult{CookieHeader: "SAPISID=ok"}, nil
	})

	mkReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://"+loginHelperAddr+"/browser-login", nil)
		r.RemoteAddr = "127.0.0.1:53421"
		r.Header.Set("Origin", origin)
		r.Header.Set(loginHelperHeader, token)
		return r
	}

	w1 := httptest.NewRecorder()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.ServeHTTP(w1, mkReq())
	}()
	<-entered

	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, mkReq())
	if w2.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on concurrent loginHelperHandler request, got %d (%s)", w2.Code, w2.Body.String())
	}

	close(release)
	wg.Wait()
	if w1.Code != http.StatusOK {
		t.Fatalf("expected first login request to return 200, got %d (%s)", w1.Code, w1.Body.String())
	}
}

func TestOfflineFallbackModels_VersionedIDsAndBareAliases(t *testing.T) {
	models := offlineFallbackModels()
	want := map[string]string{
		"gemini-4-hydrogen":          "",
		"gemini-4-hydrogen-thinking": "gemini-hydrogen-thinking",
		"gemini-3.1-pro":             "gemini-pro",
		"gemini-3.5-flash-lite":      "gemini-flash-lite",
		"gemini-auto":                "",
		"gemini-4-carbon":            "gemini-carbon",
	}
	found := make(map[string]gemini.ModelInfo, len(models))
	for _, m := range models {
		found[m.ModelName] = m
	}
	for id, bareAlias := range want {
		m, ok := found[id]
		if !ok {
			t.Fatalf("offlineFallbackModels missing %q", id)
		}
		if !gemini.MatchesModel(m, id) {
			t.Errorf("MatchesModel(%q, %q) = false", m.ModelName, id)
		}
		if bareAlias != "" && !gemini.MatchesModel(m, bareAlias) {
			t.Errorf("MatchesModel(%q, bare alias %q) = false", m.ModelName, bareAlias)
		}
		if id != "gemini-auto" {
			if _, src := gemini.ResolveModelLimits(m, 0, 0); src != "documented" {
				t.Errorf("ResolveModelLimits(%q) source = %q, want documented", id, src)
			}
		}
	}
}

func TestReadInteractiveCookieInput_SingleLineAndMultilineJSON(t *testing.T) {
	single := readInteractiveCookieInput(strings.NewReader("Cookie: SAPISID=s1; OSID=o1\n"))
	if single != "Cookie: SAPISID=s1; OSID=o1" {
		t.Fatalf("unexpected single-line read: %q", single)
	}

	multiJSON := "[\n  {\n    \"name\": \"SAPISID\",\n    \"value\": \"sap_ml\",\n    \"domain\": \".google.com\"\n  },\n  {\n    \"name\": \"OSID\",\n    \"value\": \"o_ml\",\n    \"domain\": \"gemini.google.com\"\n  }\n]\n"
	gotJSON := readInteractiveCookieInput(strings.NewReader(multiJSON))
	hdrJSON, _, err := cookies.ParseImportPayload(gotJSON)
	if err != nil {
		t.Fatalf("ParseImportPayload(multiline JSON) failed: %v", err)
	}
	if cookies.ExtractSAPISID(hdrJSON) != "sap_ml" || cookies.ParseHeader(hdrJSON)["OSID"] != "o_ml" {
		t.Fatalf("unexpected multiline JSON cookies: %q", hdrJSON)
	}

	netscape := "# Netscape HTTP Cookie File\n.google.com\tTRUE\t/\tTRUE\t0\tSAPISID\tsap_ns\n#HttpOnly_gemini.google.com\tFALSE\t/\tTRUE\t0\tOSID\to_ns\n"
	gotNS := readInteractiveCookieInput(strings.NewReader(netscape))
	hdrNS, _, err := cookies.ParseImportPayload(gotNS)
	if err != nil {
		t.Fatalf("ParseImportPayload(multiline Netscape) failed: %v", err)
	}
	if cookies.ExtractSAPISID(hdrNS) != "sap_ns" || cookies.ParseHeader(hdrNS)["OSID"] != "o_ns" {
		t.Fatalf("unexpected multiline Netscape cookies: %q", hdrNS)
	}
}
