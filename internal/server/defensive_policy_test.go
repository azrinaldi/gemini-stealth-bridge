package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

func TestDefensiveAccountAliasesFollowCurrentPolicy(t *testing.T) {
	s := New(config.Config{APIKey: "public"})
	s.SetAccounts([]*AccountState{{ID: "first"}, {ID: "second"}})
	resolve := func(key string) AuthScope {
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		r.Header.Set("X-API-Key", key)
		return s.ResolveAuthScope(r)
	}
	if got := resolve("gsb-second"); !got.OK || got.AccountID != "second" {
		t.Fatalf("public alias resolved to %+v", got)
	}
	s.UpdateCfg(func(c *config.Config) { c.APIKey = "pool-secret" })
	if resolve("gsb-second").OK || resolve("second").OK || resolve("pool-secreu").OK || !resolve("pool-secret").OK {
		t.Fatal("private mode must accept only configured keys")
	}
	s.UpdateCfg(func(c *config.Config) {
		c.APIKey = "public"
		c.AccountTokens = map[string]string{"account-secret": "second"}
	})
	if resolve("second").OK || resolve("gsb-second").OK || resolve("account-secret").AccountID != "second" {
		t.Fatal("explicit account keys must replace inferred aliases")
	}
}

func TestDefensiveLocalAdminPolicy(t *testing.T) {
	s := New(config.Config{APIKey: "public"})
	cases := []struct {
		name, host, peer, origin, forwarded string
		want                                bool
	}{
		{"same IPv6 origin", "[::1]:80", "[::1]:51330", "http://[::1]", "", true},
		{"different origin port", "localhost:20133", "127.0.0.1:51330", "http://localhost:20134", "", false},
		{"empty peer", "localhost", "", "", "", false},
		{"non-loopback authority", "bridge.example", "127.0.0.1:51330", "", "", false},
		{"forwarding header even if empty", "localhost", "127.0.0.1:51330", "", "X-Forwarded-Proto", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/admin/config", nil)
			r.Host, r.RemoteAddr = tc.host, tc.peer
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.forwarded != "" {
				r.Header.Set(tc.forwarded, "")
			}
			if got := s.authorizeAdminAPI(httptest.NewRecorder(), r); got != tc.want {
				t.Fatalf("authorizeAdminAPI = %t; want %t", got, tc.want)
			}
		})
	}
}

func TestDefensiveCrossSiteGenerationAndMCP(t *testing.T) {
	s := New(config.Config{APIKey: "public"})
	for _, path := range []string{"/v1/chat/completions", "/responses", "/v1/images/generations", "/videos/generations", "/mcp"} {
		r := httptest.NewRequest(http.MethodPost, "http://bridge.example"+path, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://another.example")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%s: response = %d, %v", path, w.Code, w.Header())
		}
	}
}

func TestDefensiveMCPRejectsBlockedAccountWithoutModels(t *testing.T) {
	s := New(config.Config{APIKey: "secret"})
	for _, tc := range []struct {
		acct *AccountState
		code string
	}{
		{&AccountState{ID: "disabled", Disabled: true}, "account_disabled"},
		{&AccountState{ID: "quarantined", Quarantined: true}, "account_banned"},
	} {
		s.SetAccounts([]*AccountState{tc.acct})
		_, err := s.mcpWebSearch(context.Background(), AuthScope{OK: true, AccountID: tc.acct.ID}, "query", "", time.Second)
		if mcpErrorCode(err) != tc.code {
			t.Fatalf("%s: got %v; want %s", tc.acct.ID, err, tc.code)
		}
	}
}

func TestDefensiveDownloadCommand(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "https://bridge.example/v1/chat/completions", nil)
	r.Header.Set("X-Forwarded-Host", "proxy.example")
	r.Header.Set("X-Forwarded-Proto", "http")
	s := New(config.Config{})
	base := s.requestBaseURL(withRequestBaseURL(r.Context(), r, 20133))
	if base != "https://bridge.example" {
		t.Fatalf("request URL = %q", base)
	}
	cmd := mediaDownloadCommand(base+"/v1/media/image.png", "folder/portrait's\nfinal.png")
	want := `curl -q -fsS --globoff --proto '=http,https' --no-clobber -o 'portrait'\''sfinal.png' -- 'https://bridge.example/v1/media/image.png'`
	if cmd != want {
		t.Fatalf("download command = %q; want %q", cmd, want)
	}
	if mediaDownloadCommand("file:///v1/media/image.png", "image.png") != "" {
		t.Fatal("must not emit download command for non-HTTP URL")
	}
}

type defensiveMediaClient struct {
	mockUpstream
	data []byte
	mime string
}

func (c *defensiveMediaClient) DownloadMedia(context.Context, string, string) ([]byte, string, error) {
	return c.data, c.mime, nil
}

func TestDefensiveMediaPolicy(t *testing.T) {
	s := New(config.Config{})
	now := time.Now()
	s.clock = func() time.Time { return now }
	client := &defensiveMediaClient{data: []byte("unverified content"), mime: "image/svg+xml"}
	s.SetAccounts([]*AccountState{{ID: "acct", Client: client}})
	get := func(id string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/media/"+id, nil))
		return w
	}
	_, img := s.registerMedia(context.Background(), "acct", "https://media.example/image", "", "image/png", "png", "")
	w := get(img.ID)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("Content-Disposition") != "attachment" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unverified media response = %d, %v", w.Code, w.Header())
	}
	now = now.Add(mediaTTL)
	if got := get(img.ID).Code; got != http.StatusNotFound || s.mediaBytes != 0 {
		t.Fatalf("expired media status = %d; retained bytes = %d", got, s.mediaBytes)
	}
	client.data = make([]byte, maxCachedMediaBytes/8)
	client.mime = "video/mp4"
	var firstID string
	for i := range 9 {
		_, entry := s.registerMedia(context.Background(), "acct", fmt.Sprintf("https://media.example/%d", i), "", "video/mp4", "mp4", "")
		if i == 0 {
			firstID = entry.ID
		}
		if _, _, err := s.ensureMediaBytes(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	if got := get(firstID).Code; got != http.StatusNotFound || s.mediaBytes > maxCachedMediaBytes {
		t.Fatalf("oldest media status = %d; retained bytes = %d", got, s.mediaBytes)
	}
}

func TestDefensiveImportPreservesHealthyAccountOnFailedProbe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.json")
	_, err := cookies.SaveCredentialFile(path, cookies.CredentialFile{Kind: "gemini-web", Name: "existing", Email: "existing@example.test", Cookies: "SAPISID=old; __Secure-1PSID=old", AuthUser: 2})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := New(config.Config{AccountsDir: dir, HomeDir: dir})
	s.LoadCredentials()
	probeCalls := 0
	s.clientFactory = func(a *AccountState) UpstreamClient {
		if a.FilePath == path {
			t.Fatal("probe must not use live credential path")
		}
		return &mockUpstream{bootFn: func(context.Context) error {
			probeCalls++
			return errors.New("probe failed")
		}}
	}
	w := httptest.NewRecorder()
	s.handleAdminImportAccount(context.Background(), w, map[string]any{"id": "existing", "cookie": "SAPISID=new; __Secure-1PSID=new"})
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || w.Code == http.StatusOK || s.findAccountByID("existing") == nil || probeCalls != 1 {
		t.Fatalf("failed probe changed live account: status=%d, calls=%d, read error=%v", w.Code, probeCalls, err)
	}
	if got := deriveAccountIDFromEmail("Existing@example.test", []*AccountState{{ID: "existing"}, {ID: "existing-2"}}); got != "Existing-3" {
		t.Fatalf("derived duplicate ID %q", got)
	}
}

func TestDefensiveRetiredAccountCannotResaveCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.json")
	if _, err := cookies.SaveCredentialFile(path, cookies.CredentialFile{Kind: "gemini-web", Name: "existing", Cookies: "SAPISID=fixture"}); err != nil {
		t.Fatal(err)
	}
	s := New(config.Config{AccountsDir: dir, HomeDir: dir})
	s.LoadCredentials()
	account := s.findAccountByID("existing")
	if account == nil {
		t.Fatal("fixture account not loaded")
	}
	old := s.clientFor(account).(*gemini.AccountClient)
	w := httptest.NewRecorder()
	s.handleAdminDeleteAccount(w, account.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	old.SetSurfaceStatus("geminiWeb", "ok")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("retired client wrote deleted credentials: %v", err)
	}
	if release, err := s.beginAuxStream(account); err == nil {
		release()
		t.Fatal("retired account was still eligible at dispatch")
	}
}

func TestDefensiveConfigSaveFailureDoesNotPublish(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s := New(config.Config{EnvFile: filepath.Join(blocker, "config.env"), AccountStrategy: "lru"})
	s.SetAccounts([]*AccountState{{ID: "first"}, {ID: "second"}})
	w := httptest.NewRecorder()
	s.handleAdminPatchConfig(w, map[string]any{"accountStrategy": "fill-first", "persist": true})
	if w.Code == http.StatusOK || s.Config().AccountStrategy != "lru" {
		t.Fatalf("failed config patch published in memory: %d %s", w.Code, s.Config().AccountStrategy)
	}
	w = httptest.NewRecorder()
	s.handleAdminMoveAccount(w, "second", map[string]any{"direction": "up"})
	if w.Code == http.StatusOK || s.Accounts()[0].ID != "first" || len(s.Config().AccountPriority) != 0 {
		t.Fatalf("failed account move published in memory: %d %v", w.Code, s.Config().AccountPriority)
	}
}

func TestDefensiveModelRefreshSingleFlight(t *testing.T) {
	s := New(config.Config{APIKey: "public", ModelRefreshEvery: time.Minute})
	calls := make(chan struct{}, 20)
	release := make(chan struct{})
	defer close(release)
	client := &mockUpstream{modelsFn: func(ctx context.Context, force bool) (*gemini.AvailableModelsResult, error) {
		calls <- struct{}{}
		<-release
		return &gemini.AvailableModelsResult{Status: http.StatusOK, Models: []gemini.ModelInfo{testModel}}, nil
	}}
	s.SetAccounts([]*AccountState{{ID: "acct", Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now().Add(-time.Hour), Client: client}})
	request := func() {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("stale model endpoint = %d", w.Code)
		}
	}
	request()
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("no background model refresh started")
	}
	for range 10 {
		request()
	}
	select {
	case <-calls:
		t.Fatal("started a second model refresh while the first was still running")
	case <-time.After(20 * time.Millisecond):
	}
}
