//go:build e2e

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/toolbridge"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
	fhttp "github.com/bogdanfinn/fhttp"
)

// ---------------------------------------------------------------------------
// Shared E2E Helpers (compile-safe against wip/framewatch contracts)
// ---------------------------------------------------------------------------

type e2eMediaUpstream struct {
	*mockUpstream
	downloadFn func(ctx context.Context, rawURL, fallbackURL string) ([]byte, string, error)
}

func (m *e2eMediaUpstream) DownloadMedia(ctx context.Context, rawURL, fallbackURL string) ([]byte, string, error) {
	if m.downloadFn != nil {
		return m.downloadFn(ctx, rawURL, fallbackURL)
	}
	return e2ePNGBytes(16, 9), "image/png", nil
}

type e2eStubDoer struct {
	fn func(req *fhttp.Request) (*fhttp.Response, error)
}

func (d *e2eStubDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	return d.fn(req)
}

func (d *e2eStubDoer) CloseIdleConnections() {}

type e2eSSEEvent struct {
	Event string
	Data  map[string]any
	Raw   string
}

func e2eParseTypedSSE(t *testing.T, raw string) []e2eSSEEvent {
	t.Helper()
	var events []e2eSSEEvent
	var curEvent string
	var dataLines []string
	flush := func() {
		if curEvent == "" && len(dataLines) == 0 {
			return
		}
		joined := strings.Join(dataLines, "\n")
		var obj map[string]any
		if strings.TrimSpace(joined) != "" {
			if err := json.Unmarshal([]byte(joined), &obj); err != nil {
				t.Fatalf("failed to unmarshal SSE event %q JSON %q: %v", curEvent, joined, err)
			}
		}
		events = append(events, e2eSSEEvent{
			Event: curEvent,
			Data:  obj,
			Raw:   joined,
		})
		curEvent = ""
		dataLines = nil
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	return events
}

func e2ePNGBytes(w, h int) []byte {
	if w <= 0 {
		w = 1
	}
	if h <= 0 {
		h = 1
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 12, G: 120, B: 200, A: 255})
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func e2eQuoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func e2eNewServerWithCfg(t *testing.T, mutate func(*config.Config)) (*Server, config.Config) {
	t.Helper()
	home := t.TempDir()
	cfg := config.Load()
	cfg.HomeDir = home
	cfg.AccountsDir = filepath.Join(home, "accounts")
	cfg.CredFile = filepath.Join(home, "web-credentials.json")
	cfg.EnvFile = filepath.Join(home, "config.env")
	cfg.MaxPromptChars = 850000
	cfg.MaxOldToolChars = 32000
	cfg.SessionCache = false
	cfg.RejectDowngrade = true
	cfg.StreamBufferMode = "verified"
	if mutate != nil {
		mutate(&cfg)
	}
	srv := New(cfg)
	t.Cleanup(func() { srv.FlushSessionCacheNow() })
	return srv, cfg
}

func e2eNewServer(t *testing.T, mutate func(*config.Config)) *Server {
	t.Helper()
	srv, _ := e2eNewServerWithCfg(t, mutate)
	return srv
}

func e2eNewAccount(id, email string, client UpstreamClient, models ...gemini.ModelInfo) *AccountState {
	if len(models) == 0 {
		models = []gemini.ModelInfo{testModel}
	}
	return &AccountState{
		ID:       id,
		Email:    email,
		Client:   client,
		Models:   models,
		Capacity: 2,
		ModelsAt: time.Now(),
		Quota: []gemini.QuotaBucket{
			{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 2400},
			{ID: "weekly", Label: "Weekly", Total: 48384, Remaining: 48384},
		},
		QuotaAt: time.Now(),
	}
}

func e2eServeRequest(srv *Server, method, target string, body any, headers map[string]string, remoteAddr string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			reader = strings.NewReader(v)
		case []byte:
			reader = bytes.NewReader(v)
		default:
			raw, _ := json.Marshal(v)
			reader = bytes.NewReader(raw)
		}
	}
	req := httptest.NewRequest(method, target, reader)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	} else {
		req.RemoteAddr = "127.0.0.1:45000"
	}
	if strings.HasPrefix(target, "/") {
		req.Host = "127.0.0.1:20133"
	}
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func e2eValidChatBody(modelName string, extra map[string]any) map[string]any {
	if modelName == "" {
		modelName = testModel.ModelName
	}
	body := map[string]any{
		"model": modelName,
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func e2eDecodeJSONMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("failed to decode JSON %q: %v", string(b), err)
	}
	return m
}

func e2eExtractMediaToolResultJSON(t *testing.T, continuation string) map[string]any {
	t.Helper()
	marker := "User: [Harness tool result for " + toolbridge.BuiltinMediaToolName + "]:\n"
	idx := strings.Index(continuation, marker)
	if idx < 0 {
		t.Fatalf("missing media tool result marker in continuation: %s", continuation)
	}
	rest := continuation[idx+len(marker):]
	if end := strings.Index(rest, "\n"); end >= 0 {
		rest = rest[:end]
	}
	return e2eDecodeJSONMap(t, []byte(strings.TrimSpace(rest)))
}

func e2eParsePOSIXSingleQuotedArg(s string) (unquoted, rest string, err error) {
	if !strings.HasPrefix(s, "'") {
		return "", s, fmt.Errorf("expected leading single quote in %q", s)
	}
	var out strings.Builder
	for i := 1; i < len(s); {
		if s[i] == '\'' {
			if i+4 <= len(s) && s[i:i+4] == `'\''` {
				out.WriteByte('\'')
				i += 4
				continue
			}
			return out.String(), s[i+1:], nil
		}
		out.WriteByte(s[i])
		i++
	}
	return "", s, fmt.Errorf("unterminated single-quoted arg in %q", s)
}

func e2eAssertPOSIXSingleQuotedDownloadCmd(t *testing.T, outObj map[string]any, expectEscapedQuoteFragment string) (urlArg, fileArg string) {
	t.Helper()
	cmd, ok := outObj["local_download_cmd"].(string)
	if !ok || cmd == "" {
		t.Fatalf("missing local_download_cmd in outObj: %+v", outObj)
	}
	if strings.ContainsAny(cmd, "\r\n\x00") {
		t.Fatalf("local_download_cmd must strip control characters, got %q", cmd)
	}
	if suggested, _ := outObj["suggested_filename"].(string); strings.ContainsAny(suggested, "\r\n\x00") {
		t.Fatalf("suggested_filename must strip control characters, got %q", suggested)
	}
	const prefix = "curl -q -fsS --globoff --proto '=http,https' --no-clobber -o "
	if !strings.HasPrefix(cmd, prefix) {
		t.Fatalf("expected prefix %q in local_download_cmd, got %q", prefix, cmd)
	}
	var err error
	var rest string
	fileArg, rest, err = e2eParsePOSIXSingleQuotedArg(strings.TrimPrefix(cmd, prefix))
	if err != nil || !strings.HasPrefix(rest, " -- ") {
		t.Fatalf("unsafe output argument in %q: %v", cmd, err)
	}
	urlArg, rest, err = e2eParsePOSIXSingleQuotedArg(strings.TrimPrefix(rest, " -- "))
	if err != nil || rest != "" {
		t.Fatalf("unsafe URL argument in %q: %v", cmd, err)
	}
	if expectEscapedQuoteFragment != "" && !strings.Contains(cmd, expectEscapedQuoteFragment) {
		t.Fatalf("expected POSIX escaped quote fragment %q in %q", expectEscapedQuoteFragment, cmd)
	}
	return urlArg, fileArg
}

// ---------------------------------------------------------------------------
// TIER 1 — Feature Area A: Auth, Keys, Admin Lock & Security (1.1, 1.2, 1.6, 1.7, 1.9)
// ---------------------------------------------------------------------------

func TestE2E_T1_Auth_KeyResolutionAndAutoTokenDisable(t *testing.T) {
	t.Run("public_mode_accepts_pool_and_auto_tokens_across_headers", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.APIKey = "public"
			c.AccountTokens = nil
		})
		up := clientFromFrames([]gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)})
		a1 := e2eNewAccount("acc1", "acc1@example.com", up)
		a2 := e2eNewAccount("acc2", "acc2@example.com", up)
		srv.SetAccounts([]*AccountState{a1, a2})

		poolTok := srv.defaultPoolToken()
		cases := []struct {
			name    string
			target  string
			headers map[string]string
		}{
			{"no_key", "/v1/chat/completions", map[string]string{"Content-Type": "application/json"}},
			{"literal_public", "/v1/chat/completions", map[string]string{"Authorization": "Bearer public", "Content-Type": "application/json"}},
			{"derived_pool_token", "/v1/chat/completions", map[string]string{"Authorization": "Bearer " + poolTok, "Content-Type": "application/json"}},
			{"auto_gsb_prefix_x_api_key", "/v1/chat/completions", map[string]string{"x-api-key": "gsb-acc1"}},
			{"auto_raw_id_x_goog_key", "/v1/chat/completions", map[string]string{"x-goog-api-key": "acc1"}},
			{"query_key", "/v1/chat/completions?key=gsb-acc2", map[string]string{"Content-Type": "application/json"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rec := e2eServeRequest(srv, http.MethodPost, tc.target, e2eValidChatBody("", nil), tc.headers, "")
				if rec.Code != 200 {
					t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
				}
			})
		}
	})

	t.Run("secret_api_key_disables_auto_account_tokens", func(t *testing.T) {
		srv, cfg := e2eNewServerWithCfg(t, func(c *config.Config) {
			c.APIKey = "pool-secret-key-01"
			c.AccountTokens = nil
		})
		up := clientFromFrames([]gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)})
		a1 := e2eNewAccount("acc1", "acc1@example.com", up)
		srv.SetAccounts([]*AccountState{a1})

		// Write an extra account file to verify hot-reload also does NOT bypass APIKey != "public".
		_, err := cookies.SaveCredentialFile(filepath.Join(cfg.AccountsDir, "acc2.json"), cookies.CredentialFile{
			Kind:       "gemini-web",
			Name:       "acc2",
			Email:      "acc2@example.com",
			Cookies:    "SAPISID=s2; __Secure-1PSID=p2",
			CapturedAt: time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			t.Fatalf("save acc2: %v", err)
		}

		for _, badTok := range []string{"gsb-acc1", "acc1", "gsb-acc2", "acc2", "public", ""} {
			for _, ep := range []struct {
				method string
				path   string
				body   any
			}{
				{http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil)},
				{http.MethodGet, "/v1/accounts", nil},
				{http.MethodGet, "/v1/models", nil},
				{http.MethodPost, "/v1/refresh", nil},
				{http.MethodPost, "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}},
			} {
				hdrs := map[string]string{"Content-Type": "application/json"}
				if badTok != "" {
					hdrs["Authorization"] = "Bearer " + badTok
				}
				rec := e2eServeRequest(srv, ep.method, ep.path, ep.body, hdrs, "")
				if ep.path == "/mcp" {
					obj := e2eDecodeJSONMap(t, rec.Body.Bytes())
					if obj["error"] == nil {
						t.Fatalf("MCP with disabled auto-token %q must return JSON-RPC auth error, got %s", badTok, rec.Body.String())
					}
					continue
				}
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("%s %s with token %q must be 401 when BRIDGE_API_KEY is set, got %d: %s", ep.method, ep.path, badTok, rec.Code, rec.Body.String())
				}
			}
		}

		// Valid pool key works via all 4 carriers.
		for _, hdrs := range []map[string]string{
			{"Authorization": "Bearer pool-secret-key-01"},
			{"x-api-key": "pool-secret-key-01"},
			{"x-goog-api-key": "pool-secret-key-01"},
		} {
			rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), hdrs, "")
			if rec.Code != 200 {
				t.Fatalf("valid APIKey failed: %d %s", rec.Code, rec.Body.String())
			}
		}
		recQ := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions?key=pool-secret-key-01", e2eValidChatBody("", nil), nil, "")
		if recQ.Code != 200 {
			t.Fatalf("valid APIKey via ?key= failed: %d %s", recQ.Code, recQ.Body.String())
		}
	})

	t.Run("explicit_account_tokens_disable_auto_tokens", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.APIKey = "public"
			c.AccountTokens = map[string]string{"custom-tok-1": "acc1"}
		})
		up := clientFromFrames([]gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)})
		a1 := e2eNewAccount("acc1", "acc1@example.com", up)
		srv.SetAccounts([]*AccountState{a1})

		for _, badTok := range []string{"gsb-acc1", "acc1"} {
			rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{
				"Authorization": "Bearer " + badTok,
			}, "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected auto-token %q rejected with 401 when AccountTokens is configured, got %d", badTok, rec.Code)
			}
		}

		recGood := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{
			"Authorization": "Bearer custom-tok-1",
		}, "")
		if recGood.Code != 200 {
			t.Fatalf("expected explicit AccountToken to succeed, got %d: %s", recGood.Code, recGood.Body.String())
		}
	})

	t.Run("group_token_scoping_and_empty_group", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.APIKey = "secret-admin-pool"
			c.GroupTokens = map[string]string{
				"alice-key": "team-a",
				"empty-key": "team-empty",
			}
		})
		upA := clientFromFrames([]gemini.Frame{makeFrame("from-a", "", true, testModel.ModelID)})
		upB := clientFromFrames([]gemini.Frame{makeFrame("from-b", "", true, testModel.ModelID)})
		a1 := e2eNewAccount("a1", "a1@example.com", upA)
		a1.AccountGroup = "team-a"
		b1 := e2eNewAccount("b1", "b1@example.com", upB, gemini.ModelInfo{ModelName: "gemini-carbon", ModelID: "cccccccccccccccc", Capacity: 2})
		b1.AccountGroup = "team-b"
		srv.SetAccounts([]*AccountState{a1, b1})

		recAccts := e2eServeRequest(srv, http.MethodGet, "/v1/accounts", nil, map[string]string{"Authorization": "Bearer alice-key"}, "")
		if recAccts.Code != 200 {
			t.Fatalf("GET /v1/accounts group key failed: %d", recAccts.Code)
		}
		acctsBody := e2eDecodeJSONMap(t, recAccts.Body.Bytes())
		arr := acctsBody["accounts"].([]any)
		if len(arr) != 1 || arr[0].(map[string]any)["id"] != "a1" {
			t.Fatalf("group key leaked accounts outside team-a: %+v", arr)
		}

		recModels := e2eServeRequest(srv, http.MethodGet, "/v1/models", nil, map[string]string{"Authorization": "Bearer alice-key"}, "")
		if recModels.Code != 200 || strings.Contains(recModels.Body.String(), "gemini-carbon") {
			t.Fatalf("group key /v1/models leaked team-b model: %d %s", recModels.Code, recModels.Body.String())
		}

		recMCP := postMCP(t, srv, "/mcp", "alice-key", map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "web_search", "arguments": map[string]any{"query": "test"}},
		})
		if gotAcct := recMCP.Header().Get("X-GSB-Account"); gotAcct != "a1" {
			t.Fatalf("MCP with group key must route only to team-a (a1), got %q", gotAcct)
		}

		recEmpty := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Authorization": "Bearer empty-key"}, "")
		if recEmpty.Code == 200 {
			t.Fatalf("empty group key must not serve accounts, got 200: %s", recEmpty.Body.String())
		}
	})
}

func TestE2E_T1_AdminLock_LoopbackXFFAndHostOrigin(t *testing.T) {
	srv := e2eNewServer(t, func(c *config.Config) {
		c.WebUI = true
		c.APIKey = "public"
		c.AdminKey = ""
	})
	srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", clientFromFrames(nil))})

	t.Run("direct_loopback_allowed", func(t *testing.T) {
		for _, path := range []string{"/ui", "/v1/admin/config"} {
			rec := e2eServeRequest(srv, http.MethodGet, path, nil, map[string]string{"Host": "127.0.0.1:20133"}, "127.0.0.1:51000")
			if rec.Code != 200 {
				t.Fatalf("direct loopback GET %s expected 200, got %d: %s", path, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("multihop_xff_rejected", func(t *testing.T) {
		for _, path := range []string{"/ui", "/v1/admin/config"} {
			rec := e2eServeRequest(srv, http.MethodGet, path, nil, map[string]string{
				"Host":            "127.0.0.1:20133",
				"X-Forwarded-For": "127.0.0.1, 203.0.113.5",
			}, "127.0.0.1:51000")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("multi-hop XFF on %s must return 403, got %d: %s", path, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("multiheader_xff_rejected", func(t *testing.T) {
		for _, path := range []string{"/ui", "/v1/admin/config"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = "127.0.0.1:51000"
			req.Host = "127.0.0.1:20133"
			req.Header.Add("X-Forwarded-For", "127.0.0.1")
			req.Header.Add("X-Forwarded-For", "198.51.100.2")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("multi-header XFF on %s must return 403, got %d: %s", path, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("forwarded_header_multihop_rejected", func(t *testing.T) {
		for _, path := range []string{"/ui", "/v1/admin/config"} {
			rec := e2eServeRequest(srv, http.MethodGet, path, nil, map[string]string{
				"Host":      "127.0.0.1:20133",
				"Forwarded": "for=127.0.0.1, for=198.51.100.9",
			}, "127.0.0.1:51000")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("Forwarded multi-hop on %s must return 403, got %d: %s", path, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("dns_rebinding_nonloopback_host_and_origin_rejected", func(t *testing.T) {
		for _, path := range []string{"/ui", "/v1/admin/config"} {
			recHost := e2eServeRequest(srv, http.MethodGet, path, nil, map[string]string{
				"Host": "evil.attacker:20133",
			}, "127.0.0.1:51000")
			if recHost.Code != http.StatusForbidden {
				t.Fatalf("DNS rebinding Host on %s must return 403, got %d: %s", path, recHost.Code, recHost.Body.String())
			}
			recOrig := e2eServeRequest(srv, http.MethodGet, path, nil, map[string]string{
				"Host":   "127.0.0.1:20133",
				"Origin": "http://evil.attacker:20133",
			}, "127.0.0.1:51000")
			if recOrig.Code != http.StatusForbidden {
				t.Fatalf("non-loopback Origin on %s must return 403, got %d: %s", path, recOrig.Code, recOrig.Body.String())
			}
		}
	})

	t.Run("single_hop_spoofed_xff_x_real_ip_remote_addr_and_cross_site_patch_rejected", func(t *testing.T) {
		for _, path := range []string{"/ui", "/v1/admin/config"} {
			recXFF := e2eServeRequest(srv, http.MethodGet, path, nil, map[string]string{
				"Host":            "127.0.0.1:20133",
				"X-Forwarded-For": "127.0.0.1",
			}, "127.0.0.1:51000")
			if recXFF.Code != http.StatusForbidden {
				t.Fatalf("single-hop X-Forwarded-For: 127.0.0.1 on %s must return 403, got %d: %s", path, recXFF.Code, recXFF.Body.String())
			}
			recRealIP := e2eServeRequest(srv, http.MethodGet, path, nil, map[string]string{
				"Host":      "127.0.0.1:20133",
				"X-Real-IP": "127.0.0.1",
			}, "127.0.0.1:51000")
			if recRealIP.Code != http.StatusForbidden {
				t.Fatalf("single-hop X-Real-IP: 127.0.0.1 on %s must return 403, got %d: %s", path, recRealIP.Code, recRealIP.Body.String())
			}
			recRemote := e2eServeRequest(srv, http.MethodGet, path, nil, map[string]string{
				"Host": "127.0.0.1:20133",
			}, "203.0.113.5:5000")
			if recRemote.Code != http.StatusForbidden {
				t.Fatalf("non-loopback RemoteAddr on %s must return 403, got %d: %s", path, recRemote.Code, recRemote.Body.String())
			}
		}
		recCross := e2eServeRequest(srv, http.MethodPatch, "/v1/admin/config", map[string]any{
			"maxRpmPerAccount": 10,
		}, map[string]string{
			"Host":           "127.0.0.1:20133",
			"Content-Type":   "application/json",
			"Sec-Fetch-Site": "cross-site",
		}, "127.0.0.1:51000")
		if recCross.Code != http.StatusForbidden {
			t.Fatalf("cross-site mutating PATCH /v1/admin/config must return 403, got %d: %s", recCross.Code, recCross.Body.String())
		}
	})
}

func TestE2E_T1_MediaLocalDownloadCmd_POSIXQuotingAndBaseURL(t *testing.T) {
	t.Run("executeServerSideMediaCalls_posix_single_quoting", func(t *testing.T) {
		srv := e2eNewServer(t, nil)
		up := &e2eMediaUpstream{
			mockUpstream: clientFromFrames([]gemini.Frame{{
				RCID:            "rc1",
				Text:            "Generated",
				Done:            true,
				AnsweredModelID: testModel.ModelID,
				GeneratedImages: []gemini.GeneratedImage{{
					URL:   "https://lh3.googleusercontent.com/gg-dl/sample=s2048",
					Title: "Art",
				}},
			}}),
		}
		acct := e2eNewAccount("a1", "a1@example.com", up)
		srv.SetAccounts([]*AccountState{acct})

		ctx := withRequestBaseURL(context.Background(), httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20133/v1/chat/completions", nil), 20133)
		calls := []toolbridge.Call{{
			Name: toolbridge.BuiltinMediaToolName,
			Arguments: map[string]any{
				"media_type":  "image",
				"prompt":      "sunset",
				"output_path": "/tmp/$(id);touch /tmp/pwn'quote",
			},
		}}
		addition, clientCalls, err := srv.executeServerSideMediaCalls(ctx, []*AccountState{acct}, AuthScope{OK: true, AccountID: "a1"}, acct, &testModel, calls)
		if err != nil {
			t.Fatalf("executeServerSideMediaCalls error: %v", err)
		}
		if len(clientCalls) != 0 {
			t.Fatalf("expected generate_media intercepted, got %d clientCalls", len(clientCalls))
		}
		outObj := e2eExtractMediaToolResultJSON(t, addition)
		_, fileArg := e2eAssertPOSIXSingleQuotedDownloadCmd(t, outObj, `pwn'\''quote`)
		if !strings.Contains(fileArg, "pwn'quote") {
			t.Fatalf("expected unquoted -o filename to preserve literal quote safely, got %q", fileArg)
		}
	})

	t.Run("withRequestBaseURL_ignores_untrusted_xff_headers", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.APIKey = "secret-media-key"
		})
		up := &e2eMediaUpstream{
			mockUpstream: clientFromFrames([]gemini.Frame{{
				RCID:            "rc1",
				Text:            "ok",
				Done:            true,
				AnsweredModelID: testModel.ModelID,
				GeneratedImages: []gemini.GeneratedImage{{
					URL: "https://lh3.googleusercontent.com/gg-dl/x1=s2048",
				}},
			}}),
		}
		acct := e2eNewAccount("a1", "a1@example.com", up)
		srv.SetAccounts([]*AccountState{acct})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/images/generations", map[string]any{
			"prompt": "a cat",
		}, map[string]string{
			"Authorization":     "Bearer secret-media-key",
			"Content-Type":      "application/json",
			"Host":              "127.0.0.1:20133",
			"X-Forwarded-Host":  "evil.attacker/$(whoami)",
			"X-Forwarded-Proto": "https",
		}, "198.51.100.10:4444")
		if rec.Code != 200 {
			t.Fatalf("POST /v1/images/generations failed: %d %s", rec.Code, rec.Body.String())
		}
		resp := e2eDecodeJSONMap(t, rec.Body.Bytes())
		item := resp["data"].([]any)[0].(map[string]any)
		gotURL, _ := item["url"].(string)
		if !strings.HasPrefix(gotURL, "http://127.0.0.1:20133/v1/media/") {
			t.Fatalf("untrusted X-Forwarded-* must be ignored in media URL, got %q", gotURL)
		}
	})

	t.Run("formatDirectMediaToolContinuation_rejects_url_controls", func(t *testing.T) {
		out := formatDirectMediaToolContinuation([]gemini.GeneratedImage{{
			URL:   "http://127.0.0.1:20133/v1/media/abc'$(id)\n.png",
			Title: "pic",
		}}, nil)
		outObj := e2eExtractMediaToolResultJSON(t, out)
		if cmd, _ := outObj["local_download_cmd"].(string); cmd != "" {
			t.Fatal("expected no download command for invalid URL")
		}
	})
}

func TestE2E_T1_MCP_CORSAndGenerationCSRF(t *testing.T) {
	t.Run("mcp_disables_wildcard_cors_and_rejects_cross_site_origin", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.APIKey = "public"
		})
		srv.SetAccounts([]*AccountState{e2eNewAccount("acct", "acct@example.com", mcpSearchUpstream("ans", nil, nil))})

		recOpt := e2eServeRequest(srv, http.MethodOptions, "/mcp", nil, map[string]string{
			"Origin": "https://evil.example",
		}, "")
		if acao := recOpt.Header().Get("Access-Control-Allow-Origin"); acao == "*" {
			t.Fatalf("OPTIONS /mcp must not emit Access-Control-Allow-Origin: *")
		}

		recPost := e2eServeRequest(srv, http.MethodPost, "/mcp", map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "ping",
		}, map[string]string{
			"Origin":       "https://evil.example",
			"Content-Type": "application/json",
		}, "")
		if acao := recPost.Header().Get("Access-Control-Allow-Origin"); acao == "*" {
			t.Fatalf("POST /mcp must not emit Access-Control-Allow-Origin: *")
		}
		if recPost.Code != http.StatusForbidden {
			t.Fatalf("cross-origin POST /mcp in public mode must be rejected with 403, got %d: %s", recPost.Code, recPost.Body.String())
		}
	})

	t.Run("public_generation_endpoints_block_csrf", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.APIKey = "public"
		})
		up := &e2eMediaUpstream{
			mockUpstream: clientFromFrames([]gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)}),
		}
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		endpoints := []struct {
			path string
			body any
		}{
			{"/v1/chat/completions", e2eValidChatBody("", nil)},
			{"/v1/responses", map[string]any{"model": testModel.ModelName, "input": "hi"}},
			{"/v1/images/generations", map[string]any{"prompt": "cat"}},
			{"/v1/videos/generations", map[string]any{"prompt": "cat"}},
		}
		badHeaders := []map[string]string{
			{"Origin": "https://evil.example", "Content-Type": "application/json"},
			{"Sec-Fetch-Site": "cross-site", "Content-Type": "application/json"},
			{"Content-Type": "text/plain"},
			{"Content-Type": "application/x-www-form-urlencoded"},
		}
		for _, ep := range endpoints {
			for _, hdrs := range badHeaders {
				rec := e2eServeRequest(srv, http.MethodPost, ep.path, ep.body, hdrs, "")
				if rec.Code != http.StatusForbidden {
					t.Fatalf("CSRF check on %s with headers %v expected 403, got %d: %s", ep.path, hdrs, rec.Code, rec.Body.String())
				}
			}
			// Legitimate loopback JSON client without cross-site browser headers succeeds.
			recOK := e2eServeRequest(srv, http.MethodPost, ep.path, ep.body, map[string]string{
				"Content-Type": "application/json",
			}, "")
			if recOK.Code == http.StatusForbidden {
				t.Fatalf("legitimate JSON request to %s should not be 403, got %d: %s", ep.path, recOK.Code, recOK.Body.String())
			}
		}
	})

	t.Run("authenticated_bearer_cli_without_content_type_still_allowed", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.APIKey = "cli-secret-token"
		})
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", clientFromFrames([]gemini.Frame{
			makeFrame("cli ok", "", true, testModel.ModelID),
		}))})
		rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{
			"Authorization": "Bearer cli-secret-token",
		}, "")
		if rec.Code != 200 {
			t.Fatalf("authenticated Bearer CLI request without Content-Type expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestE2E_T1_GetMedia_MIMEAllowlistHeadersCapAndTTL(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	var curNow atomic.Value
	curNow.Store(now)

	srv := e2eNewServer(t, nil)
	srv.clock = func() time.Time { return curNow.Load().(time.Time) }

	pngBytes := e2ePNGBytes(8, 8)
	var returnMIME atomic.Value
	returnMIME.Store("image/png")
	var returnBody atomic.Value
	returnBody.Store(pngBytes)

	up := &e2eMediaUpstream{
		mockUpstream: clientFromFrames(nil),
		downloadFn: func(ctx context.Context, rawURL, fallbackURL string) ([]byte, string, error) {
			return returnBody.Load().([]byte), returnMIME.Load().(string), nil
		},
	}
	acct := e2eNewAccount("a1", "a1@example.com", up)
	srv.SetAccounts([]*AccountState{acct})

	t.Run("valid_png_and_security_headers", func(t *testing.T) {
		proxyURL, entry := srv.registerMedia(context.Background(), "a1", "https://lh3.googleusercontent.com/gg-dl/valid", "", "image/png", "png", "Valid")
		if entry == nil || proxyURL == "" {
			t.Fatal("registerMedia failed")
		}
		rec := e2eServeRequest(srv, http.MethodGet, "/v1/media/"+entry.ID, nil, nil, "")
		if rec.Code != 200 {
			t.Fatalf("GET /v1/media expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("expected X-Content-Type-Options: nosniff, got %q", got)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "sandbox") {
			t.Fatalf("expected restrictive CSP sandbox header, got %q", csp)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
			t.Fatalf("expected image/png Content-Type, got %q", ct)
		}

		// HEAD request returns headers and empty body.
		recHead := e2eServeRequest(srv, http.MethodHead, "/v1/media/"+entry.ID, nil, nil, "")
		if recHead.Code != 200 || recHead.Body.Len() != 0 {
			t.Fatalf("HEAD /v1/media expected 200 empty body, got %d len=%d", recHead.Code, recHead.Body.Len())
		}
	})

	t.Run("disallowed_mime_types_blocked_or_neutralized", func(t *testing.T) {
		for i, badMIME := range []string{"text/html", "image/svg+xml", "application/javascript"} {
			returnMIME.Store(badMIME)
			returnBody.Store([]byte("<svg onload=alert(1)><script>1</script></svg>"))
			_, entry := srv.registerMedia(context.Background(), "a1", fmt.Sprintf("https://lh3.googleusercontent.com/gg-dl/bad-%d", i), "", "image/png", "png", "Bad")
			rec := e2eServeRequest(srv, http.MethodGet, "/v1/media/"+entry.ID, nil, nil, "")
			ct := strings.ToLower(rec.Header().Get("Content-Type"))
			if strings.Contains(ct, "html") || strings.Contains(ct, "svg") || strings.Contains(ct, "javascript") {
				t.Fatalf("dangerous MIME %q served as %q (status %d)", badMIME, ct, rec.Code)
			}
			if rec.Code == 200 {
				if ct != "application/octet-stream" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
					t.Fatalf("if 200 returned for disallowed MIME %q, must be octet-stream + nosniff, got ct=%q nosniff=%q", badMIME, ct, rec.Header().Get("X-Content-Type-Options"))
				}
			}
		}
		returnMIME.Store("image/png")
		returnBody.Store(pngBytes)
	})

	t.Run("byte_cap_evicts_oldest_under_96_entries", func(t *testing.T) {
		shared8MiB := make([]byte, 8*1024*1024)
		copy(shared8MiB, pngBytes)
		returnBody.Store(shared8MiB)
		returnMIME.Store("image/png")

		var ids []string
		for i := 0; i < 20; i++ {
			_, entry := srv.registerMedia(context.Background(), "a1", fmt.Sprintf("https://lh3.googleusercontent.com/gg-dl/big-%d", i), "", "image/png", "png", "Big")
			ids = append(ids, entry.ID)
			rec := e2eServeRequest(srv, http.MethodGet, "/v1/media/"+entry.ID, nil, nil, "")
			if rec.Code != 200 {
				t.Fatalf("fetch entry %d failed: %d", i, rec.Code)
			}
		}
		// 20 * 8 MiB = 160 MiB (> 128 MiB cap, < 96 entry limit), so byte budget eviction must have evicted ids[0].
		recFirst := e2eServeRequest(srv, http.MethodGet, "/v1/media/"+ids[0], nil, nil, "")
		if recFirst.Code != http.StatusNotFound {
			t.Fatalf("expected oldest entry %s evicted by byte cap (<96 entries), got status %d", ids[0], recFirst.Code)
		}
		returnBody.Store(pngBytes)
	})

	t.Run("ttl_expiration_evicts_entry", func(t *testing.T) {
		_, entry := srv.registerMedia(context.Background(), "a1", "https://lh3.googleusercontent.com/gg-dl/ttl-item", "", "image/png", "png", "TTL")
		rec1 := e2eServeRequest(srv, http.MethodGet, "/v1/media/"+entry.ID, nil, nil, "")
		if rec1.Code != 200 {
			t.Fatalf("initial fetch failed: %d", rec1.Code)
		}
		curNow.Store(now.Add(25 * time.Hour))
		rec2 := e2eServeRequest(srv, http.MethodGet, "/v1/media/"+entry.ID, nil, nil, "")
		if rec2.Code != http.StatusNotFound {
			t.Fatalf("expected expired media entry to return 404 after 25h, got %d", rec2.Code)
		}
	})
}

// ---------------------------------------------------------------------------
// TIER 1 — Feature Area B: Concurrency & Race-Freedom (2.1, 2.2, 2.3, 2.4)
// ---------------------------------------------------------------------------

func TestE2E_T1_Concurrency_AccountStatusMapNoDeadlock(t *testing.T) {
	srv := e2eNewServer(t, func(c *config.Config) {
		c.WebUI = true
	})
	up := clientFromFrames([]gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)})
	a1 := e2eNewAccount("a1", "a1@example.com", up)
	a2 := e2eNewAccount("a2", "a2@example.com", up)
	srv.SetAccounts([]*AccountState{a1, a2})

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				rec := e2eServeRequest(srv, http.MethodGet, "/v1/accounts", nil, nil, "")
				if rec.Code != 200 {
					t.Errorf("GET /v1/accounts unexpected status %d", rec.Code)
					return
				}
				_, _ = AccountStatus(a1, srv.now())
			}
		}()
	}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for ctx.Err() == nil {
				if id%2 == 0 {
					srv.recordModelExhausted(a1, testModel.ModelName)
					srv.clearModelExhausted(a1, testModel.ModelName)
				} else {
					srv.markAccountFailure(a1, "transient error")
					srv.RefreshAccount(ctx, a1, false)
				}
			}
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock detected in concurrent accountStatusMap / AccountStatus / mutations")
	}
}

func TestE2E_T1_Concurrency_EarlySSEGraceChatAndResponses(t *testing.T) {
	origKeepalive := defaultKeepaliveInterval
	defaultKeepaliveInterval = 10 * time.Millisecond
	t.Cleanup(func() { defaultKeepaliveInterval = origKeepalive })

	srv := e2eNewServer(t, func(c *config.Config) {
		c.EarlySSEGrace = 12 * time.Millisecond
		c.RejectDowngrade = true
		c.StreamBufferMode = "verified"
		c.LiveThoughts = true
		c.MaxAttempts = 1
		c.MaxConcurrentPerAccount = 0
		c.MaxRPMPerAccount = 0
	})

	var seq atomic.Int64
	up := &mockUpstream{
		streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			n := seq.Add(1)
			switch n % 4 {
			case 0:
				return nil, transport.NewUpstreamError("bad gateway", 502, "upstream_degraded", nil, nil)
			case 1:
				select {
				case <-time.After(45 * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			case 2:
				return nil, transport.NewUpstreamError("auth fail", 401, "upstream_auth_error", nil, nil)
			}
			_ = onFrame(&gemini.LineParseResult{
				CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{makeFrame("hello", "thinking", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
		},
	}
	srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

	ts := httptest.NewServer(srv)
	defer ts.Close()

	errCh := make(chan string, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			reqCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if idx%5 == 0 {
				reqCtx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
			}
			defer cancel()

			path := "/v1/chat/completions"
			payload := map[string]any{
				"model":    testModel.ModelName,
				"stream":   true,
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			}
			if idx%2 == 1 {
				path = "/v1/responses"
				payload = map[string]any{
					"model":  testModel.ModelName,
					"stream": true,
					"input":  "hi",
				}
			}
			raw, _ := json.Marshal(payload)
			req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, ts.URL+path, bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if idx%5 == 0 {
				if err == nil {
					_, _ = io.ReadAll(resp.Body)
					_ = resp.Body.Close()
				}
				return
			}
			if err != nil {
				errCh <- fmt.Sprintf("req %d (%s) unexpected Do error: %v", idx, path, err)
				return
			}
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				errCh <- fmt.Sprintf("req %d (%s) unexpected ReadAll error: %v", idx, path, readErr)
				return
			}
			switch resp.StatusCode {
			case http.StatusOK, http.StatusUnauthorized, http.StatusBadGateway:
			default:
				errCh <- fmt.Sprintf("req %d (%s) unexpected status %d: %s", idx, path, resp.StatusCode, string(body))
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}
}

func TestE2E_T1_Concurrency_ConfigSnapshotUnderAdminPatch(t *testing.T) {
	srv := e2eNewServer(t, func(c *config.Config) {
		c.WebUI = true
	})
	up := clientFromFrames([]gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)})
	a1 := e2eNewAccount("a1", "a1@example.com", up)
	a2 := e2eNewAccount("a2", "a2@example.com", up)
	srv.SetAccounts([]*AccountState{a1, a2})

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	errCh := make(chan string, 64)
	reportErr := func(msg string) {
		select {
		case errCh <- msg:
		default:
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			strategies := []string{"rotate", "lru", "fill-first"}
			for step := 0; ctx.Err() == nil; step++ {
				recPatch := e2eServeRequest(srv, http.MethodPatch, "/v1/admin/config", map[string]any{
					"accountStrategy":  strategies[(idx+step)%len(strategies)],
					"maxRpmPerAccount": 590 + (step % 11),
					"liveThoughts":     step%2 == 0,
					"rejectDowngrade":  step%2 == 1,
				}, map[string]string{"Content-Type": "application/json"}, "")
				if recPatch.Code != http.StatusOK {
					reportErr(fmt.Sprintf("PATCH /v1/admin/config unexpected status %d: %s", recPatch.Code, recPatch.Body.String()))
					return
				}
				recMove := e2eServeRequest(srv, http.MethodPost, "/v1/admin/accounts/a1/move", map[string]any{
					"direction": "down",
				}, map[string]string{"Content-Type": "application/json"}, "")
				if recMove.Code != http.StatusOK {
					reportErr(fmt.Sprintf("POST /v1/admin/accounts/a1/move unexpected status %d: %s", recMove.Code, recMove.Body.String()))
					return
				}
			}
		}(i)
	}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for ctx.Err() == nil {
				var rec *httptest.ResponseRecorder
				var label string
				switch idx % 5 {
				case 0:
					label = "POST /v1/chat/completions"
					rec = e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Content-Type": "application/json"}, "")
				case 1:
					label = "POST /v1/responses"
					rec = e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{"model": testModel.ModelName, "input": "hi"}, map[string]string{"Content-Type": "application/json"}, "")
				case 2:
					label = "GET /v1/models"
					rec = e2eServeRequest(srv, http.MethodGet, "/v1/models", nil, nil, "")
					if rec.Code != http.StatusOK {
						reportErr(fmt.Sprintf("%s unexpected status %d: %s", label, rec.Code, rec.Body.String()))
						return
					}
					continue
				case 3:
					label = "GET /v1/accounts"
					rec = e2eServeRequest(srv, http.MethodGet, "/v1/accounts", nil, nil, "")
					if rec.Code != http.StatusOK {
						reportErr(fmt.Sprintf("%s unexpected status %d: %s", label, rec.Code, rec.Body.String()))
						return
					}
					continue
				case 4:
					label = "POST /mcp"
					rec = e2eServeRequest(srv, http.MethodPost, "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, map[string]string{"Authorization": "Bearer public", "Content-Type": "application/json"}, "")
				}
				if rec.Code != http.StatusOK && rec.Code != http.StatusTooManyRequests {
					reportErr(fmt.Sprintf("%s unexpected status %d: %s", label, rec.Code, rec.Body.String()))
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}
}

func TestE2E_T1_Concurrency_AccountQuotaSliceCOW(t *testing.T) {
	srv := e2eNewServer(t, nil)
	up := &mockUpstream{
		streamFn: clientFromFrames([]gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)}).streamFn,
		quotaFn: func(ctx context.Context) []gemini.QuotaBucket {
			return []gemini.QuotaBucket{
				{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 1000},
				{ID: "weekly", Label: "Weekly", Total: 48384, Remaining: 40000},
			}
		},
	}
	a1 := e2eNewAccount("a1", "a1@example.com", up)
	srv.SetAccounts([]*AccountState{a1})

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	errCh := make(chan string, 64)
	reportErr := func(msg string) {
		select {
		case errCh <- msg:
		default:
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				srv.recordModelExhausted(a1, testModel.ModelName)
				srv.clearModelExhausted(a1, testModel.ModelName)
				srv.RefreshAccount(ctx, a1, false)
			}
		}()
	}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for ctx.Err() == nil {
				switch idx % 3 {
				case 0:
					rec := e2eServeRequest(srv, http.MethodGet, "/v1/quota", nil, nil, "")
					if rec.Code != http.StatusOK {
						reportErr(fmt.Sprintf("GET /v1/quota unexpected status %d: %s", rec.Code, rec.Body.String()))
						return
					}
				case 1:
					rec := e2eServeRequest(srv, http.MethodGet, "/v1/accounts", nil, nil, "")
					if rec.Code != http.StatusOK {
						reportErr(fmt.Sprintf("GET /v1/accounts unexpected status %d: %s", rec.Code, rec.Body.String()))
						return
					}
				case 2:
					rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Content-Type": "application/json"}, "")
					if rec.Code != http.StatusOK && rec.Code != http.StatusTooManyRequests {
						reportErr(fmt.Sprintf("POST /v1/chat/completions unexpected status %d: %s", rec.Code, rec.Body.String()))
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}
}

func TestE2E_T1_Concurrency_AdminRateLimitAndHotReload(t *testing.T) {
	srv := e2eNewServer(t, func(c *config.Config) {
		c.WebUI = true
		c.AdminKey = "admin-secret"
		c.APIKey = "public"
	})
	srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", clientFromFrames(nil))})

	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()

	errCh := make(chan string, 32)
	reportErr := func(msg string) {
		select {
		case errCh <- msg:
		default:
		}
	}
	var admin401Count, admin429Count atomic.Int64
	var models401Count atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ip := fmt.Sprintf("198.51.100.%d:9000", 10+idx)
			for ctx.Err() == nil {
				rec := e2eServeRequest(srv, http.MethodGet, "/v1/admin/config", nil, map[string]string{
					"Authorization": "Bearer wrong-admin-token",
				}, ip)
				switch rec.Code {
				case http.StatusUnauthorized:
					admin401Count.Add(1)
				case http.StatusTooManyRequests:
					admin429Count.Add(1)
				default:
					reportErr(fmt.Sprintf("GET /v1/admin/config unexpected status %d: %s", rec.Code, rec.Body.String()))
					return
				}
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for ctx.Err() == nil {
				rec := e2eServeRequest(srv, http.MethodGet, "/v1/models", nil, map[string]string{
					"Authorization": fmt.Sprintf("Bearer gsb-unknown-%d", idx),
				}, "")
				if rec.Code != http.StatusUnauthorized {
					reportErr(fmt.Sprintf("GET /v1/models with unknown bearer token unexpected status %d: %s", rec.Code, rec.Body.String()))
					return
				}
				models401Count.Add(1)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}
	if admin401Count.Load() == 0 {
		t.Fatalf("expected 401 responses on /v1/admin/config before rate limit, got 0")
	}
	if admin429Count.Load() == 0 {
		t.Fatalf("expected 429 rate-limited responses on /v1/admin/config across burst, got 0")
	}
	if models401Count.Load() == 0 {
		t.Fatalf("expected 401 responses on /v1/models with unknown bearer tokens, got 0")
	}
}

// ---------------------------------------------------------------------------
// TIER 1 — Feature Area C: Auth/Status Recovery & Streaming Thinking (3.6, 4.2, 4.3)
// ---------------------------------------------------------------------------

func TestE2E_T1_Status_StickyExpiredAndQuarantinedRecovery(t *testing.T) {
	t.Run("successful_chat_clears_sticky_expired_status", func(t *testing.T) {
		srv := e2eNewServer(t, nil)
		up := clientFromFrames([]gemini.Frame{makeFrame("recovered", "", true, testModel.ModelID)})
		acct := e2eNewAccount("a1", "a1@example.com", up)
		srv.SetAccounts([]*AccountState{acct})

		srv.markAccountFailure(acct, zzReal1016Msg)
		if st, _ := AccountStatus(acct, srv.now()); st != StatusExpired {
			t.Fatalf("expected initial status %q, got %q", StatusExpired, st)
		}

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 {
			t.Fatalf("expected 200 chat, got %d: %s", rec.Code, rec.Body.String())
		}
		if st, reason := AccountStatus(acct, srv.now()); st != StatusOK {
			t.Fatalf("expected successful chat to clear sticky expired status, got %q (%s)", st, reason)
		}
	})

	t.Run("healthy_probe_clears_quarantined_status", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.WebUI = true
		})
		acct := e2eNewAccount("a1", "a1@example.com", &mockUpstream{})
		acct.Quarantined = true
		acct.QuarantineReason = "upstream rejected this account (code 1033)"
		srv.SetAccounts([]*AccountState{acct})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/admin/accounts/a1/probe", nil, nil, "")
		if rec.Code != 200 {
			t.Fatalf("probe failed: %d %s", rec.Code, rec.Body.String())
		}
		body := e2eDecodeJSONMap(t, rec.Body.Bytes())
		acctMap := body["account"].(map[string]any)
		if acctMap["status"] != StatusOK || acctMap["quarantined"] != false {
			t.Fatalf("expected healthy probe to clear quarantine, got %+v", acctMap)
		}
	})

	t.Run("healthy_rotate_clears_stale_auth_failure", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.WebUI = true
		})
		up := &mockUpstream{
			rotateFn: func(ctx context.Context) (*cookies.RotateResult, error) {
				return &cookies.RotateResult{Status: 200, Rotated: true}, nil
			},
		}
		acct := e2eNewAccount("a1", "a1@example.com", up)
		srv.SetAccounts([]*AccountState{acct})
		srv.markAccountFailure(acct, zzReal1016Msg)

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/admin/accounts/a1/rotate", nil, nil, "")
		if rec.Code != 200 {
			t.Fatalf("rotate failed: %d %s", rec.Code, rec.Body.String())
		}
		if st, reason := AccountStatus(acct, srv.now()); st != StatusOK {
			t.Fatalf("expected rotated cookies to clear auth failure, got %q (%s)", st, reason)
		}
	})
}

func TestE2E_T1_AuthUserSecondaryAccountIsolation(t *testing.T) {
	srv, cfg := e2eNewServerWithCfg(t, func(c *config.Config) {
		c.WebUI = true
	})
	if err := os.MkdirAll(cfg.AccountsDir, 0o700); err != nil {
		t.Fatalf("mkdir accounts: %v", err)
	}
	secPath := filepath.Join(cfg.AccountsDir, "sec.json")
	_, err := cookies.SaveCredentialFile(secPath, cookies.CredentialFile{
		Kind:       "gemini-web",
		Name:       "sec",
		Email:      "sec@example.com",
		Cookies:    "SAPISID=sap_sec; __Secure-1PSID=psid_sec",
		AuthUser:   2,
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("save sec credential: %v", err)
	}

	var seenAppURL, seenStreamAuthUser string
	doer := &e2eStubDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.Contains(u, "/u/2/app"):
				seenAppURL = u
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_sec","cfb2h":"bl_sec","FdrFJe":"555","oPEP7c":"sec@example.com"}`)),
				}, nil
			case strings.Contains(u, "batchexecute"):
				seenStreamAuthUser = req.URL.Query().Get("authuser")
				return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader(")]}'\n\n[]"))}, nil
			default:
				return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
			}
		},
	}
	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
	ac, err := gemini.LoadAccountFromFile(secPath, pool, cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile failed: %v", err)
	}
	if err := ac.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap authUser=2 failed: %v", err)
	}
	if !strings.Contains(seenAppURL, "/u/2/app") {
		t.Fatalf("expected secondary account bootstrap to hit /u/2/app, got %q", seenAppURL)
	}
	_ = ac.GetQuota(context.Background())
	if seenStreamAuthUser != "2" {
		t.Fatalf("expected authuser=2 on batchexecute/quota request, got %q", seenStreamAuthUser)
	}

	// Re-importing with id="sec" via admin import must preserve AuthUser: 2.
	srv.clientFactory = func(a *AccountState) UpstreamClient {
		return &mockUpstream{}
	}
	srv.LoadCredentials()
	rec := e2eServeRequest(srv, http.MethodPost, "/v1/admin/accounts/import", map[string]any{
		"id":     "sec",
		"cookie": "SAPISID=sap_sec_new; __Secure-1PSID=psid_sec_new",
	}, map[string]string{"Content-Type": "application/json"}, "")
	if rec.Code != 200 {
		t.Fatalf("admin re-import failed: %d %s", rec.Code, rec.Body.String())
	}
	reloaded, _, err := cookies.LoadAccountFile(secPath)
	if err != nil {
		t.Fatalf("LoadAccountFile after re-import: %v", err)
	}
	if reloaded.AuthUser != 2 {
		t.Fatalf("expected AuthUser=2 preserved across re-import, got %d", reloaded.AuthUser)
	}
}

func TestE2E_T1_Streaming_LiveThoughtsAndActivityUnderStrictMode(t *testing.T) {
	t.Run("incremental_reasoning_streams_before_done_under_reject_downgrade", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.RejectDowngrade = true
			c.StreamBufferMode = "verified"
			c.LiveThoughts = true
		})

		releaseFrame2 := make(chan struct{})
		up := &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				if err := onFrame(&gemini.LineParseResult{
					CID: "c1", RID: "r1", AnsweredModelID: "",
					Frames: []gemini.Frame{makeFrame("", "Step 1: inspect files", false, "")},
				}); err != nil {
					return nil, err
				}
				select {
				case <-releaseFrame2:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				if err := onFrame(&gemini.LineParseResult{
					CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
					Frames: []gemini.Frame{makeFrame("Final answer.", "Step 1: inspect files\nStep 2: verify", true, testModel.ModelID)},
				}); err != nil {
					return nil, err
				}
				return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
			},
		}
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		ts := httptest.NewServer(srv)
		defer ts.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		raw, _ := json.Marshal(e2eValidChatBody("", map[string]any{"stream": true}))
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("stream request failed: %v", err)
		}
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		sawStep1BeforeRelease := false
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "Step 1: inspect files") {
				sawStep1BeforeRelease = true
				close(releaseFrame2)
				break
			}
		}
		if !sawStep1BeforeRelease {
			close(releaseFrame2)
			t.Fatal("expected Step 1 reasoning_content to stream before Frame 2 was released under BRIDGE_REJECT_DOWNGRADE=1")
		}
		rest, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(rest), "Final answer.") {
			t.Fatalf("expected Final answer after Frame 2 release, got %s", string(rest))
		}
	})

	t.Run("real_wire_jspb_pj6_activity_streams_reasoning_progress", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.RejectDowngrade = true
			c.StreamBufferMode = "verified"
			c.LiveThoughts = true
		})

		releaseFinal := make(chan struct{})
		doer := &e2eStubDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				u := req.URL.String()
				if strings.Contains(u, "/app") {
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{},
						Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_live","cfb2h":"bl_live","FdrFJe":"777","oPEP7c":"live@example.com"}`)),
					}, nil
				}
				if strings.Contains(u, "StreamGenerate") {
					pr, pw := io.Pipe()
					go func() {
						defer pw.Close()
						// Frame 1: pj[6] ("7" in 1-based sparse map) activity progress + pj[39] ("40") model identity.
						inner1 := `[null,["c_p","r_p"],null,null,[["rc_p",[""],null,null,null,null,null,null,[1]]],{"7":["Searching the web",["google_search",["gs","Searching",null]]],"40":"` + testModel.ModelID + `"}]`
						line1 := ")]}'\n\n" + `[["wrb.fr",null,` + e2eQuoteJSON(inner1) + `]]` + "\n"
						_, _ = io.WriteString(pw, line1)
						<-releaseFinal
						inner2 := `[null,["c_p","r_p"],null,null,[["rc_p",["Result ready."],null,null,null,null,null,null,[2]]],{"40":"` + testModel.ModelID + `"}]`
						line2 := `[["wrb.fr",null,` + e2eQuoteJSON(inner2) + `]]` + "\n"
						_, _ = io.WriteString(pw, line2)
					}()
					return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: pr}, nil
				}
				return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader("[]"))}, nil
			},
		}
		pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
		ac := gemini.NewAccountClient("a1", "SAPISID=s1; __Secure-1PSID=p1", "", pool, cookies.NewRotator())
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "live@example.com", ac)})

		ts := httptest.NewServer(srv)
		defer ts.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		raw, _ := json.Marshal(e2eValidChatBody("", map[string]any{"stream": true}))
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		sawActivityReasoning := false
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, `"reasoning_content"`) {
				sawActivityReasoning = true
				close(releaseFinal)
				break
			}
		}
		if !sawActivityReasoning {
			close(releaseFinal)
			t.Fatal("expected pj[6] activity progress to emit reasoning_content before final frame")
		}
		rest, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(rest), "Result ready.") {
			t.Fatalf("expected final answer after release, got %s", string(rest))
		}
	})

	t.Run("downgrade_aborts_and_unknown_identity_never_leaks_reasoning", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.RejectDowngrade = true
			c.StreamBufferMode = "verified"
			c.LiveThoughts = true
			c.MaxAttempts = 1
		})
		acct := e2eNewAccount("a1", "a1@example.com", clientFromFrames([]gemini.Frame{
			makeFrame("downgraded text", "downgraded thought", true, lowerModelID),
		}))
		srv.SetAccounts([]*AccountState{acct})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", map[string]any{"stream": true}), map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 429 || strings.Contains(rec.Body.String(), "downgraded text") {
			t.Fatalf("expected 429 model_substitution with no leaked downgraded text, got %d: %s", rec.Code, rec.Body.String())
		}

		// Reconciled invariant: TestStrictIdentityUnknownDoesNotEmitReasoning
		acct2 := e2eNewAccount("a2", "a2@example.com", clientFromFrames([]gemini.Frame{
			makeFrame("answer", "secret reasoning", true, ""),
		}))
		srv.SetAccounts([]*AccountState{acct2})
		recUnk := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", map[string]any{"stream": true}), map[string]string{"Content-Type": "application/json"}, "")
		if recUnk.Code != 502 || strings.Contains(recUnk.Body.String(), "secret reasoning") || strings.Contains(recUnk.Body.String(), "data:") {
			t.Fatalf("expected 502 model_identity_unknown with zero leaked reasoning, got %d: %s", recUnk.Code, recUnk.Body.String())
		}
	})
}

func TestE2E_T1_Retry_UpstreamTransient1013ChatAndResponses(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			name := fmt.Sprintf("%s_stream_%v", strings.TrimPrefix(endpoint, "/v1/"), streaming)
			t.Run(name, func(t *testing.T) {
				// 1. Pool failover across 2 accounts on upstream_transient (1013).
				srv := e2eNewServer(t, func(c *config.Config) {
					c.MaxAttempts = 3
					c.MaxUpstreamAttempts = 3
					c.APIKey = "public"
				})
				var calls1, calls2 atomic.Int32
				up1 := &mockUpstream{
					streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
						calls1.Add(1)
						return nil, transport.NewUpstreamError("transient error 1013", 503, "upstream_transient", nil, nil)
					},
				}
				up2 := &mockUpstream{
					streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
						calls2.Add(1)
						_ = onFrame(&gemini.LineParseResult{
							CID: "c2", RID: "r2", AnsweredModelID: testModel.ModelID,
							Frames: []gemini.Frame{makeFrame("recovered-ok", "", true, testModel.ModelID)},
						})
						return &gemini.StreamResponse{Status: 200, CID: "c2", RID: "r2"}, nil
					},
				}
				srv.SetAccounts([]*AccountState{
					e2eNewAccount("a1", "a1@example.com", up1),
					e2eNewAccount("a2", "a2@example.com", up2),
				})

				body := map[string]any{"model": testModel.ModelName, "stream": streaming}
				if endpoint == "/v1/chat/completions" {
					body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
				} else {
					body["input"] = "hi"
				}
				rec := e2eServeRequest(srv, http.MethodPost, endpoint, body, map[string]string{"Content-Type": "application/json"}, "")
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), "recovered-ok") {
					t.Fatalf("pool failover on upstream_transient failed: code=%d body=%s", rec.Code, rec.Body.String())
				}
				if got := rec.Header().Get("X-GSB-Retry-Reason"); got != "upstream_transient" {
					t.Fatalf("expected X-GSB-Retry-Reason=upstream_transient, got %q", got)
				}
				if calls1.Load() != 1 || calls2.Load() != 1 {
					t.Fatalf("expected failover a1->a2 (1,1), got (%d,%d)", calls1.Load(), calls2.Load())
				}

				// 2. Pinned same-account retry on upstream_transient.
				srvPin := e2eNewServer(t, func(c *config.Config) {
					c.MaxAttempts = 3
					c.MaxUpstreamAttempts = 3
				})
				var pinCalls atomic.Int32
				upPin := &mockUpstream{
					streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
						if pinCalls.Add(1) == 1 {
							return nil, transport.NewUpstreamError("transient 1013", 503, "upstream_transient", nil, nil)
						}
						_ = onFrame(&gemini.LineParseResult{
							CID: "cp", RID: "rp", AnsweredModelID: testModel.ModelID,
							Frames: []gemini.Frame{makeFrame("pinned-ok", "", true, testModel.ModelID)},
						})
						return &gemini.StreamResponse{Status: 200, CID: "cp", RID: "rp"}, nil
					},
				}
				srvPin.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", upPin)})
				recPin := e2eServeRequest(srvPin, http.MethodPost, endpoint, body, map[string]string{"Content-Type": "application/json"}, "")
				if recPin.Code != 200 || !strings.Contains(recPin.Body.String(), "pinned-ok") || pinCalls.Load() != 2 {
					t.Fatalf("same-account retry on upstream_transient failed: code=%d calls=%d body=%s", recPin.Code, pinCalls.Load(), recPin.Body.String())
				}
			})
		}
	}
}

func TestE2E_T1_Responses_EarlySSEGraceStrictMode(t *testing.T) {
	srv := e2eNewServer(t, func(c *config.Config) {
		c.RejectDowngrade = true
		c.StreamBufferMode = "verified"
		c.EarlySSEGrace = 20 * time.Millisecond
		c.MaxAttempts = 1
	})

	up := &mockUpstream{
		streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			select {
			case <-time.After(140 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			_ = onFrame(&gemini.LineParseResult{
				CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{makeFrame("after-grace", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
		},
	}
	srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	raw, _ := json.Marshal(map[string]any{"model": testModel.ModelName, "stream": true, "input": "hi"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/responses", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/responses failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 early grace flush, got %d", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed >= 110*time.Millisecond {
		t.Fatalf("expected HTTP headers flushed by EarlySSEGrace (<110ms), took %s", elapsed)
	}
	scanner := bufio.NewScanner(resp.Body)
	var earlyBuf strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		earlyBuf.WriteString(line + "\n")
		if strings.Contains(earlyBuf.String(), "response.created") {
			if d := time.Since(start); d >= 110*time.Millisecond {
				t.Fatalf("expected response.created flushed via grace (<110ms), took %s", d)
			}
			break
		}
	}
	if !strings.Contains(earlyBuf.String(), ": ping") || !strings.Contains(earlyBuf.String(), "response.created") {
		t.Fatalf("expected early : ping and response.created, got %s", earlyBuf.String())
	}
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "after-grace") {
		t.Fatalf("expected final answer after grace, got %s", string(rest))
	}
}

// ---------------------------------------------------------------------------
// TIER 1 — Feature Area D: /v1/responses Parity, Toolbridge, Routing & Edge (5.1–5.4e)
// ---------------------------------------------------------------------------

func TestE2E_T1_Responses_FullParityWithChat(t *testing.T) {
	t.Run("max_output_tokens_truncates_output", func(t *testing.T) {
		srv := e2eNewServer(t, nil)
		up := clientFromFrames([]gemini.Frame{
			makeFrame("one two three four five six seven eight nine ten eleven twelve", "", true, testModel.ModelID),
		})
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
			"model":             testModel.ModelName,
			"input":             "count",
			"max_output_tokens": 5,
		}, map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "twelve") {
			t.Fatalf("expected max_output_tokens=5 to truncate answer before 'twelve', got %s", rec.Body.String())
		}
		respObj := e2eDecodeJSONMap(t, rec.Body.Bytes())
		if respObj["status"] != "incomplete" {
			t.Fatalf("expected non-stream max_output_tokens status=incomplete, got %v", respObj["status"])
		}
		inc, _ := respObj["incomplete_details"].(map[string]any)
		if inc["reason"] != "max_output_tokens" {
			t.Fatalf("expected incomplete_details.reason=max_output_tokens, got %v", inc)
		}

		// Streaming max_output_tokens must emit response.incomplete with status=incomplete & reason=max_output_tokens.
		recStream := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
			"model":             testModel.ModelName,
			"input":             "count",
			"stream":            true,
			"max_output_tokens": 5,
		}, map[string]string{"Content-Type": "application/json"}, "")
		if recStream.Code != 200 {
			t.Fatalf("expected streaming 200, got %d: %s", recStream.Code, recStream.Body.String())
		}
		if strings.Contains(recStream.Body.String(), "twelve") {
			t.Fatalf("expected streaming max_output_tokens=5 to truncate 'twelve', got %s", recStream.Body.String())
		}
		events := e2eParseTypedSSE(t, recStream.Body.String())
		var foundIncomplete bool
		for _, ev := range events {
			if ev.Event == "response.incomplete" {
				r, _ := ev.Data["response"].(map[string]any)
				incDet, _ := r["incomplete_details"].(map[string]any)
				if r["status"] == "incomplete" && incDet["reason"] == "max_output_tokens" {
					foundIncomplete = true
				}
			}
		}
		if !foundIncomplete {
			t.Fatalf("expected response.incomplete SSE event with reason=max_output_tokens, got: %s", recStream.Body.String())
		}
	})

	t.Run("stop_sequences_truncate_output", func(t *testing.T) {
		srv := e2eNewServer(t, nil)
		up := clientFromFrames([]gemini.Frame{
			makeFrame("alpha beta HALT gamma delta", "", true, testModel.ModelID),
		})
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		for _, streaming := range []bool{false, true} {
			rec := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
				"model":  testModel.ModelName,
				"input":  "test stop",
				"stream": streaming,
				"stop":   []string{"HALT"},
			}, map[string]string{"Content-Type": "application/json"}, "")
			if rec.Code != 200 {
				t.Fatalf("stream=%v expected 200, got %d: %s", streaming, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "HALT") || strings.Contains(rec.Body.String(), "gamma") {
				t.Fatalf("stream=%v expected stop sequence HALT to truncate output, got %s", streaming, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "alpha beta") {
				t.Fatalf("stream=%v expected prefix before HALT preserved, got %s", streaming, rec.Body.String())
			}
		}
	})

	t.Run("text_format_json_object_unwraps_fence_in_json_and_stream", func(t *testing.T) {
		srv := e2eNewServer(t, nil)
		bigJSON := fmt.Sprintf("```json\n{\"status\":\"ok\",\"padding\":%q}\n```", strings.Repeat("x", 300))
		up := clientFromFrames([]gemini.Frame{
			makeFrame(bigJSON, "", false, testModel.ModelID),
			makeFrame(bigJSON, "", true, testModel.ModelID),
		})
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		for _, streaming := range []bool{false, true} {
			rec := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
				"model":  testModel.ModelName,
				"input":  "return json",
				"stream": streaming,
				"text":   map[string]any{"format": map[string]any{"type": "json_object"}},
			}, map[string]string{"Content-Type": "application/json"}, "")
			if rec.Code != 200 {
				t.Fatalf("stream=%v failed: %d %s", streaming, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "```json") {
				t.Fatalf("stream=%v leaked markdown json fence: %s", streaming, rec.Body.String())
			}
			if !streaming {
				respObj := e2eDecodeJSONMap(t, rec.Body.Bytes())
				text := respObj["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
				parsed := e2eDecodeJSONMap(t, []byte(text))
				if parsed["status"] != "ok" {
					t.Fatalf("expected unwrapped JSON status=ok, got %q", text)
				}
			} else {
				events := e2eParseTypedSSE(t, rec.Body.String())
				var deltas strings.Builder
				var completedText string
				for _, ev := range events {
					if ev.Event == "response.output_text.delta" {
						d, _ := ev.Data["delta"].(string)
						deltas.WriteString(d)
					}
					if ev.Event == "response.output_text.done" {
						completedText, _ = ev.Data["text"].(string)
					}
				}
				if deltas.Len() == 0 || completedText == "" {
					t.Fatalf("expected non-empty streamed JSON text, deltas=%q done=%q", deltas.String(), completedText)
				}
				parsed := e2eDecodeJSONMap(t, []byte(completedText))
				if parsed["status"] != "ok" {
					t.Fatalf("expected streamed unwrapped JSON status=ok, got %q", completedText)
				}
			}
		}
	})

	t.Run("strip_tool_guard_from_output_and_reasoning", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.LiveThoughts = true
		})
		up := clientFromFrames([]gemini.Frame{
			makeFrame(
				"Clean answer.\n<prohibit_tool_calling>0 tool calls remaining</prohibit_tool_calling>",
				"Thinking.\n0 tool calls remaining",
				true,
				testModel.ModelID,
			),
		})
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
			"model": testModel.ModelName,
			"input": "hi",
		}, map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "prohibit_tool_calling") || strings.Contains(rec.Body.String(), "0 tool calls remaining") {
			t.Fatalf("StripToolGuard failed to strip guard from /v1/responses: %s", rec.Body.String())
		}
		respObj := e2eDecodeJSONMap(t, rec.Body.Bytes())
		outputs, _ := respObj["output"].([]any)
		var gotReasoning, gotMessage string
		for _, raw := range outputs {
			item, _ := raw.(map[string]any)
			switch item["type"] {
			case "reasoning":
				sum, _ := item["summary"].([]any)
				if len(sum) > 0 {
					gotReasoning, _ = sum[0].(map[string]any)["text"].(string)
				}
			case "message":
				cnt, _ := item["content"].([]any)
				if len(cnt) > 0 {
					gotMessage, _ = cnt[0].(map[string]any)["text"].(string)
				}
			}
		}
		if !strings.Contains(gotMessage, "Clean answer.") || !strings.Contains(gotReasoning, "Thinking.") {
			t.Fatalf("expected clean output 'Clean answer.' and reasoning 'Thinking.', got msg=%q reasoning=%q", gotMessage, gotReasoning)
		}
	})

	t.Run("retry_resets_sse_state_and_avoids_duplicate_reasoning_added", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.LiveThoughts = true
			c.MaxAttempts = 3
			c.MaxUpstreamAttempts = 3
		})
		var calls atomic.Int32
		up := &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				if calls.Add(1) == 1 {
					_ = onFrame(&gemini.LineParseResult{
						CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
						Frames: []gemini.Frame{makeFrame("", "thought 1", false, testModel.ModelID)},
					})
					return nil, transport.NewUpstreamError("transient", 503, "upstream_transient", nil, nil)
				}
				_ = onFrame(&gemini.LineParseResult{
					CID: "c2", RID: "r2", AnsweredModelID: testModel.ModelID,
					Frames: []gemini.Frame{makeFrame("final", "thought 2", true, testModel.ModelID)},
				})
				return &gemini.StreamResponse{Status: 200, CID: "c2", RID: "r2"}, nil
			},
		}
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
			"model":  testModel.ModelName,
			"input":  "hi",
			"stream": true,
		}, map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if calls.Load() != 2 {
			t.Fatalf("expected 2 upstream calls across upstream_transient retry, got %d", calls.Load())
		}
		bodyStr := rec.Body.String()
		if !strings.Contains(bodyStr, "thought 2") || !strings.Contains(bodyStr, "final") || !strings.Contains(bodyStr, "response.completed") {
			t.Fatalf("expected retry attempt 2 to stream 'thought 2', 'final', and 'response.completed', got: %s", bodyStr)
		}
		events := e2eParseTypedSSE(t, bodyStr)
		rsAddedCount := 0
		var reasoning strings.Builder
		for _, ev := range events {
			if ev.Event == "response.output_item.added" {
				if item, ok := ev.Data["item"].(map[string]any); ok && item["type"] == "reasoning" {
					rsAddedCount++
				}
			}
			if ev.Event == "response.reasoning_summary_text.delta" {
				delta, _ := ev.Data["delta"].(string)
				reasoning.WriteString(delta)
			}
		}
		if rsAddedCount != 1 {
			t.Fatalf("expected exactly 1 reasoning response.output_item.added across retry, got count=%d", rsAddedCount)
		}
		if got := reasoning.String(); got != "thought 1\n\nthought 2" {
			t.Fatalf("reasoning deltas replayed or lost across retry: %q", got)
		}
	})

	t.Run("empty_reply_stream_emits_added_before_done", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.MaxAttempts = 1
		})
		// Emit non-empty tool-stripped or whitespace-only text after verified identity so stream finishes empty.
		up := clientFromFrames([]gemini.Frame{
			makeFrame("<prohibit_tool_calling>0 tool calls remaining</prohibit_tool_calling>", "", true, testModel.ModelID),
		})
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
			"model":  testModel.ModelName,
			"input":  "hi",
			"stream": true,
		}, map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		events := e2eParseTypedSSE(t, rec.Body.String())
		idxAdded, idxDone, idxCompleted := -1, -1, -1
		for i, ev := range events {
			if ev.Event == "response.content_part.added" && idxAdded == -1 {
				idxAdded = i
			}
			if ev.Event == "response.content_part.done" && idxDone == -1 {
				idxDone = i
			}
			if ev.Event == "response.completed" && idxCompleted == -1 {
				idxCompleted = i
			}
		}
		if idxAdded < 0 || idxDone <= idxAdded || idxCompleted < 0 {
			t.Fatalf("expected response.content_part.added before .done and response.completed, got idxAdded=%d idxDone=%d idxCompleted=%d body=%s", idxAdded, idxDone, idxCompleted, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "prohibit_tool_calling") {
			t.Fatalf("StripToolGuard failed to strip guard before empty reply check: %s", rec.Body.String())
		}
	})

	t.Run("post_header_error_uses_responses_sse_format", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.EarlySSE = true
			c.RejectDowngrade = false
			c.MaxAttempts = 1
		})
		up := &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				return nil, transport.NewUpstreamError("upstream exploded", 502, "upstream_degraded", nil, nil)
			},
		}
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
			"model":  testModel.ModelName,
			"input":  "hi",
			"stream": true,
		}, map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 {
			t.Fatalf("expected 200 SSE header, got %d", rec.Code)
		}
		events := e2eParseTypedSSE(t, rec.Body.String())
		foundResponsesErr := false
		for _, ev := range events {
			if ev.Event == "error" || ev.Event == "response.failed" {
				code, _ := ev.Data["code"].(string)
				msg, _ := ev.Data["message"].(string)
				if code == "upstream_degraded" && strings.Contains(msg, "upstream exploded") {
					foundResponsesErr = true
				}
			}
		}
		if !foundResponsesErr {
			t.Fatalf("expected Responses-format SSE error event with code=upstream_degraded, got: %s", rec.Body.String())
		}
	})

	t.Run("eager_mode_with_tools_holds_refusal_window_before_flushing", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.StreamBufferMode = "eager"
			c.RejectDowngrade = false
			c.MaxAttempts = 2
			c.MaxUpstreamAttempts = 2
		})
		var calls atomic.Int32
		up := &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				if calls.Add(1) == 1 {
					// Refusal within RefusalWindowBytes (<2000 bytes) must NOT flush before Done so retry succeeds!
					_ = onFrame(&gemini.LineParseResult{
						CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
						Frames: []gemini.Frame{makeFrame("I cannot execute tool calls because my tool budget is exhausted.", "", false, testModel.ModelID)},
					})
					_ = onFrame(&gemini.LineParseResult{
						CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
						Frames: []gemini.Frame{makeFrame("I cannot execute tool calls because my tool budget is exhausted.", "", true, testModel.ModelID)},
					})
					return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
				}
				_ = onFrame(&gemini.LineParseResult{
					CID: "c2", RID: "r2", AnsweredModelID: testModel.ModelID,
					Frames: []gemini.Frame{makeFrame("```json\n{\"tool_calls\":[{\"name\":\"ping_tool\",\"arguments\":{}}]}\n```", "", true, testModel.ModelID)},
				})
				return &gemini.StreamResponse{Status: 200, CID: "c2", RID: "r2"}, nil
			},
		}
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
			"model":  testModel.ModelName,
			"input":  "call ping_tool",
			"stream": true,
			"tools": []any{map[string]any{
				"type":       "function",
				"name":       "ping_tool",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
			}},
		}, map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 || calls.Load() != 2 {
			t.Fatalf("expected eager mode refusal window retry to succeed on attempt 2, got code=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "budget is exhausted") {
			t.Fatalf("refusal prose leaked in eager mode before RefusalWindowBytes (%d): %s", toolbridge.RefusalWindowBytes, rec.Body.String())
		}
	})
}

func TestE2E_T1_Toolbridge_CompactSchemaMapPreservesUserKeys(t *testing.T) {
	t.Run("compact_schema_map_preserves_property_and_defs_keys", func(t *testing.T) {
		schema := map[string]any{
			"type":                 "object",
			"title":                "TopLevelMetadataTitle",
			"examples":             []any{"metadata example"},
			"$schema":              "https://json-schema.org/draft/2020-12/schema",
			"additionalProperties": false,
			"properties": map[string]any{
				"title":                map[string]any{"type": "string", "title": "NestedMeta", "description": "Issue title"},
				"examples":             map[string]any{"type": "string"},
				"$schema":              map[string]any{"type": "string"},
				"additionalProperties": map[string]any{"type": "boolean"},
			},
			"$defs": map[string]any{
				"Item": map[string]any{
					"type":  "object",
					"title": "MetaShouldStrip",
					"properties": map[string]any{
						"examples": map[string]any{"type": "string"},
					},
				},
			},
			"definitions": map[string]any{
				"LegacyItem": map[string]any{
					"type":  "object",
					"title": "LegacyMetaShouldStrip",
					"properties": map[string]any{
						"title":    map[string]any{"type": "string"},
						"examples": map[string]any{"type": "string"},
					},
				},
			},
			"required": []any{"title", "examples"},
		}

		compacted := toolbridge.CompactSchemaMap(schema, 200)
		if _, exists := compacted["title"]; exists {
			t.Fatalf("top-level metadata 'title' should be stripped")
		}
		if _, exists := compacted["$schema"]; exists {
			t.Fatalf("top-level metadata '$schema' should be stripped")
		}
		props := compacted["properties"].(map[string]any)
		for _, keepKey := range []string{"title", "examples", "$schema", "additionalProperties"} {
			if _, ok := props[keepKey]; !ok {
				t.Fatalf("user tool property %q inside 'properties' was stripped by CompactSchemaMap!", keepKey)
			}
		}
		defs := compacted["$defs"].(map[string]any)
		item := defs["Item"].(map[string]any)
		itemProps := item["properties"].(map[string]any)
		if _, ok := itemProps["examples"]; !ok {
			t.Fatalf("user property 'examples' inside $defs.Item.properties was stripped!")
		}
		legacyDefs := compacted["definitions"].(map[string]any)
		legacyItem := legacyDefs["LegacyItem"].(map[string]any)
		legacyProps := legacyItem["properties"].(map[string]any)
		for _, keepKey := range []string{"title", "examples"} {
			if _, ok := legacyProps[keepKey]; !ok {
				t.Fatalf("user property %q inside definitions.LegacyItem.properties was stripped!", keepKey)
			}
		}
	})

	t.Run("e2e_chat_with_tool_having_title_and_examples_properties", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.ToolSchemaCompact = true
		})
		var capturedPrompt string
		up := &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				capturedPrompt = p
				_ = onFrame(&gemini.LineParseResult{
					CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
					Frames: []gemini.Frame{makeFrame("```json\n{\"tool_calls\":[{\"name\":\"create_issue\",\"arguments\":{\"title\":\"Bug\",\"examples\":\"repro\"}}]}\n```", "", true, testModel.ModelID)},
				})
				return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
			},
		}
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", map[string]any{
			"model":    testModel.ModelName,
			"messages": []any{map[string]any{"role": "user", "content": "create issue"}},
			"tools": []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "create_issue",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title":    map[string]any{"type": "string"},
							"examples": map[string]any{"type": "string"},
						},
						"required": []any{"title", "examples"},
					},
				},
			}},
		}, map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"tool_calls"`) || !strings.Contains(rec.Body.String(), `"create_issue"`) {
			t.Fatalf("tool call with title/examples properties failed: %d %s", rec.Code, rec.Body.String())
		}
		var propsFound bool
		for _, line := range strings.Split(capturedPrompt, "\n") {
			if !strings.Contains(line, `"create_issue"`) || !strings.HasPrefix(strings.TrimSpace(line), "{") {
				continue
			}
			var def map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &def); err == nil {
				if params, ok := def["parameters"].(map[string]any); ok {
					if props, ok := params["properties"].(map[string]any); ok {
						_, hasTitle := props["title"]
						_, hasExamples := props["examples"]
						if hasTitle && hasExamples {
							propsFound = true
						}
					}
				}
			}
		}
		if !propsFound {
			t.Fatalf("flattened prompt lost 'title'/'examples' inside parameters.properties: %s", capturedPrompt)
		}
	})
}

func TestE2E_T1_Routing_StrategiesAndDisabledQuarantinedBlocking(t *testing.T) {
	t.Run("fill_first_skips_missing_model_and_rpm_limit_and_reclaims", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.AccountStrategy = "fill-first"
			c.AccountPriority = []string{"a1", "a2"}
			c.MaxRPMPerAccount = 1
		})
		proOnly := gemini.ModelInfo{ModelName: "gemini-pro", ModelID: testModel.ModelID, Capacity: 2}
		carbonOnly := gemini.ModelInfo{ModelName: "gemini-carbon", ModelID: "cccccccccccccccc", Capacity: 2}
		a1 := e2eNewAccount("a1", "a1@example.com", clientFromFrames([]gemini.Frame{makeFrame("from-a1", "", true, testModel.ModelID)}), proOnly)
		a2 := e2eNewAccount("a2", "a2@example.com", clientFromFrames([]gemini.Frame{makeFrame("from-a2", "", true, "cccccccccccccccc")}), proOnly, carbonOnly)
		srv.SetAccounts([]*AccountState{a1, a2})

		// Request carbon: a1 lacks gemini-carbon -> must skip a1 and select a2 directly!
		recCarbon := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("gemini-carbon", nil), map[string]string{"Content-Type": "application/json"}, "")
		if recCarbon.Code != 200 || recCarbon.Header().Get("X-GSB-Account") != "a2" {
			t.Fatalf("fill-first should route gemini-carbon directly to a2 (a1 lacks it), got code=%d acct=%q body=%s", recCarbon.Code, recCarbon.Header().Get("X-GSB-Account"), recCarbon.Body.String())
		}

		// The carbon request consumed a2's RPM slot. Isolate the next scenario
		// so the second pro request has an eligible fallback.
		a2.mu.Lock()
		a2.reqTimestamps = nil
		a2.mu.Unlock()

		// Request pro: a1 is healthy and within RPM -> routes to a1.
		recPro1 := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("gemini-pro", nil), map[string]string{"Content-Type": "application/json"}, "")
		if recPro1.Code != 200 || recPro1.Header().Get("X-GSB-Account") != "a1" {
			t.Fatalf("fill-first should pick a1 for pro, got code=%d acct=%q", recPro1.Code, recPro1.Header().Get("X-GSB-Account"))
		}

		// Second pro request within 60s: a1 hit MaxRPMPerAccount=1 -> must route to a2 (not fail 429 on a1).
		a2.mu.Lock()
		a2.Client = clientFromFrames([]gemini.Frame{makeFrame("from-a2-pro", "", true, testModel.ModelID)})
		a2.mu.Unlock()
		recPro2 := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("gemini-pro", nil), map[string]string{"Content-Type": "application/json"}, "")
		if recPro2.Code != 200 || recPro2.Header().Get("X-GSB-Account") != "a2" {
			t.Fatalf("fill-first should skip RPM-capped a1 and route to a2, got code=%d acct=%q body=%s", recPro2.Code, recPro2.Header().Get("X-GSB-Account"), recPro2.Body.String())
		}
	})

	t.Run("lru_skips_cooling_missing_model_and_rpm_capped", func(t *testing.T) {
		now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
		srv := e2eNewServer(t, func(c *config.Config) {
			c.AccountStrategy = "lru"
			c.MaxRPMPerAccount = 1
		})
		srv.clock = func() time.Time { return now }
		proOnly := gemini.ModelInfo{ModelName: "gemini-pro", ModelID: testModel.ModelID, Capacity: 2}
		carbonOnly := gemini.ModelInfo{ModelName: "gemini-carbon", ModelID: "cccccccccccccccc", Capacity: 2}

		a1 := e2eNewAccount("a1", "a1@example.com", clientFromFrames([]gemini.Frame{makeFrame("a1", "", true, testModel.ModelID)}), proOnly)
		a2 := e2eNewAccount("a2", "a2@example.com", clientFromFrames([]gemini.Frame{makeFrame("a2", "", true, testModel.ModelID)}), proOnly)
		a3 := e2eNewAccount("a3", "a3@example.com", clientFromFrames([]gemini.Frame{makeFrame("a3", "", true, testModel.ModelID)}), proOnly)
		a4 := e2eNewAccount("a4", "a4@example.com", clientFromFrames([]gemini.Frame{makeFrame("a4", "", true, "cccccccccccccccc")}), proOnly, carbonOnly)
		a1.LastUsed = now.Add(-10 * time.Minute)
		a1.LastFail = now.Add(-10 * time.Second) // cooling!
		a1.Fails = 1
		a2.LastUsed = now.Add(-1 * time.Minute)
		a3.LastUsed = now.Add(-5 * time.Minute) // least recently used healthy!
		a4.LastUsed = now                      // most recently used, only one with gemini-carbon
		srv.SetAccounts([]*AccountState{a1, a2, a3, a4})

		// 1. lru skips cooling a1 and selects older healthy a3 over a2/a4.
		rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 || rec.Header().Get("X-GSB-Account") != "a3" {
			t.Fatalf("lru expected a3 (skipping cooling a1, older than a2), got code=%d acct=%q", rec.Code, rec.Header().Get("X-GSB-Account"))
		}

		// 2. Now a3 is RPM-capped (MaxRPMPerAccount=1) and updated LastUsed; next pro request skips a3 (RPM-capped) and a1 (cooling) -> picks a2.
		rec2 := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Content-Type": "application/json"}, "")
		if rec2.Code != 200 || rec2.Header().Get("X-GSB-Account") != "a2" {
			t.Fatalf("lru expected a2 (skipping cooling a1 and RPM-capped a3), got code=%d acct=%q", rec2.Code, rec2.Header().Get("X-GSB-Account"))
		}

		// 3. Request gemini-carbon: a1/a2/a3 lack gemini-carbon -> lru must skip them and pick a4!
		a4.mu.Lock()
		a4.reqTimestamps = nil
		a4.mu.Unlock()
		recCarbon := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("gemini-carbon", nil), map[string]string{"Content-Type": "application/json"}, "")
		if recCarbon.Code != 200 || recCarbon.Header().Get("X-GSB-Account") != "a4" {
			t.Fatalf("lru expected a4 for gemini-carbon (others lack model), got code=%d acct=%q", recCarbon.Code, recCarbon.Header().Get("X-GSB-Account"))
		}
	})

	t.Run("disabled_and_quarantined_blocked_across_all_paths_and_mcp", func(t *testing.T) {
		for _, mode := range []string{"disabled", "quarantined"} {
			t.Run(mode, func(t *testing.T) {
				srv := e2eNewServer(t, func(c *config.Config) {
					c.WebUI = true
					c.APIKey = "public"
					c.GroupTokens = map[string]string{"gkey": "team-g"}
				})
				a1 := e2eNewAccount("a1", "a1@example.com", clientFromFrames([]gemini.Frame{makeFrame("should-never-serve", "", true, testModel.ModelID)}))
				a1.AccountGroup = "team-g"
				if mode == "disabled" {
					a1.Disabled = true
				} else {
					a1.Quarantined = true
					a1.QuarantineReason = "TOS_VIOLATION"
				}
				srv.SetAccounts([]*AccountState{a1})

				// 1. 1-account pool
				recPool := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Content-Type": "application/json"}, "")
				if recPool.Code == 200 {
					t.Fatalf("1-account %s pool must not serve, got 200: %s", mode, recPool.Body.String())
				}

				// 2. Explicit account key
				recAcct := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{
					"Authorization": "Bearer gsb-a1",
					"Content-Type":  "application/json",
				}, "")
				if recAcct.Code == 200 {
					t.Fatalf("explicit account key on %s account must not serve, got 200", mode)
				}

				// 3. Group key
				recGrp := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{
					"Authorization": "Bearer gkey",
					"Content-Type":  "application/json",
				}, "")
				if recGrp.Code == 200 {
					t.Fatalf("group key on %s account must not serve, got 200", mode)
				}

				// 4. /v1/responses, /v1/images/generations, /v1/videos/generations
				recResp := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
					"model": testModel.ModelName,
					"input": "hi",
				}, map[string]string{"Content-Type": "application/json"}, "")
				if recResp.Code == 200 {
					t.Fatalf("/v1/responses on %s account must not serve, got 200", mode)
				}
				recImg := e2eServeRequest(srv, http.MethodPost, "/v1/images/generations", map[string]any{
					"prompt": "cat",
				}, map[string]string{"Content-Type": "application/json"}, "")
				if recImg.Code == 200 {
					t.Fatalf("/v1/images/generations on %s account must not serve, got 200", mode)
				}
				recVid := e2eServeRequest(srv, http.MethodPost, "/v1/videos/generations", map[string]any{
					"prompt": "wave",
				}, map[string]string{"Content-Type": "application/json"}, "")
				if recVid.Code == 200 {
					t.Fatalf("/v1/videos/generations on %s account must not serve, got 200", mode)
				}

				// 5. MCP web_search
				recMCP := postMCP(t, srv, "/mcp", "public", map[string]any{
					"jsonrpc": "2.0", "id": 1, "method": "tools/call",
					"params": map[string]any{"name": "web_search", "arguments": map[string]any{"query": "test"}},
				})
				var mcpOut struct {
					Result struct {
						IsError bool `json:"isError"`
					} `json:"result"`
				}
				_ = json.Unmarshal(recMCP.Body.Bytes(), &mcpOut)
				if !mcpOut.Result.IsError {
					t.Fatalf("MCP web_search on %s account must fail (isError:true), got %s", mode, recMCP.Body.String())
				}
			})
		}
	})
}

func TestE2E_T1_EdgeFixes_54a_to_54e(t *testing.T) {
	t.Run("5.4a_json_object_buffered_stream_suppresses_fence", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.RejectDowngrade = true
			c.StreamBufferMode = "verified"
		})
		fenced := fmt.Sprintf("```json\n{\"ok\":true,\"data\":%q}\n```", strings.Repeat("a", 300))
		up := clientFromFrames([]gemini.Frame{
			makeFrame(fenced, "", false, testModel.ModelID),
			makeFrame(fenced, "", true, testModel.ModelID),
		})
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", map[string]any{
			"stream":          true,
			"response_format": map[string]any{"type": "json_object"},
		}), map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "```json") {
			t.Fatalf("buffered stream leaked raw json fence for response_format=json_object: %s", rec.Body.String())
		}
		var content strings.Builder
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok || data == "[DONE]" {
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				t.Fatalf("invalid chat SSE chunk %q: %v", data, err)
			}
			for _, choice := range chunk.Choices {
				content.WriteString(choice.Delta.Content)
			}
		}
		parsed := e2eDecodeJSONMap(t, []byte(content.String()))
		if parsed["ok"] != true {
			t.Fatalf("expected unwrapped streamed JSON with ok=true, got %q", content.String())
		}
	})

	t.Run("5.4b_live_tool_call_streamer_index_alignment_with_generate_media", func(t *testing.T) {
		var deltas []map[string]any
		tools := toolbridge.AugmentToolsWithMediaGenerator([]any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":       "bash",
					"parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}, "required": []any{"cmd"}},
				},
			},
		}, "auto")
		streamer := toolbridge.NewLiveToolCallStreamer(tools, "auto", func(delta map[string]any) {
			deltas = append(deltas, delta)
		})
		payload := "{\"tool_calls\":[{\"name\":\"generate_media\",\"arguments\":{\"media_type\":\"image\",\"prompt\":\"cat\"}},{\"name\":\"bash\",\"arguments\":{\"cmd\":\"ls\"}}]}"
		if err := streamer.PushError(payload); err != nil {
			t.Fatalf("PushError failed: %v", err)
		}
		if _, err := streamer.FinalizeError([]toolbridge.Call{{Name: "bash", Arguments: map[string]any{"cmd": "ls"}}}, payload); err != nil {
			t.Fatalf("FinalizeError failed: %v", err)
		}
		if len(deltas) == 0 {
			t.Fatal("expected emitted tool_call deltas for bash after generate_media, got 0")
		}
		for _, d := range deltas {
			tcs, _ := d["tool_calls"].([]map[string]any)
			for _, tc := range tcs {
				if idx, _ := tc["index"].(int); idx != 0 {
					t.Fatalf("expected client tool bash after generate_media to have client index 0, got %d", idx)
				}
			}
		}
	})

	t.Run("5.4c_utf8_rune_safe_slicing", func(t *testing.T) {
		emoji := strings.Repeat("🚀日本語é", 80)
		if cut, _ := truncateToTokens(emoji, 5); !utf8.ValidString(cut) {
			t.Fatalf("truncateToTokens produced invalid UTF-8: %x", cut)
		}
		if bounded := prompt.BoundOldToolText(emoji, 37, "tool"); !utf8.ValidString(bounded) {
			t.Fatalf("BoundOldToolText produced invalid UTF-8: %x", bounded)
		}
		if merged := mergeStreamedThoughts(emoji[:100], emoji); !utf8.ValidString(merged) {
			t.Fatalf("mergeStreamedThoughts produced invalid UTF-8")
		}

		// >240-byte stream boundary mid-UTF8 on Chat SSE and /v1/responses SSE
		srv := e2eNewServer(t, func(c *config.Config) {
			c.RejectDowngrade = true
			c.StreamBufferMode = "verified"
		})
		txt := strings.Repeat("a", 239) + "🚀" + "tail"
		up := clientFromFrames([]gemini.Frame{
			makeFrame(txt, "", false, testModel.ModelID),
			makeFrame(txt, "", true, testModel.ModelID),
		})
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})
		rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", map[string]any{"stream": true}), map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 || !utf8.Valid(rec.Body.Bytes()) || !strings.Contains(rec.Body.String(), "🚀") {
			t.Fatalf("chat stream output invalid UTF-8 or bad status %d: %s", rec.Code, rec.Body.String())
		}
		recResp := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
			"model":  testModel.ModelName,
			"input":  "hi",
			"stream": true,
		}, map[string]string{"Content-Type": "application/json"}, "")
		if recResp.Code != 200 || !utf8.Valid(recResp.Body.Bytes()) || !strings.Contains(recResp.Body.String(), "🚀tail") {
			t.Fatalf("/v1/responses stream output invalid UTF-8 or bad status %d: %s", recResp.Code, recResp.Body.String())
		}
	})

	t.Run("5.4d_classifyEmpty_and_reset_headers_for_5h_and_weekly", func(t *testing.T) {
		now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
		srv := e2eNewServer(t, nil)
		srv.clock = func() time.Time { return now }
		acct := e2eNewAccount("a1", "a1@example.com", &mockUpstream{})
		resetTime := now.Add(2 * time.Hour).UTC().Format(time.RFC3339)
		acct.Quota = []gemini.QuotaBucket{
			{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 0, ResetAt: resetTime},
			{ID: "weekly", Label: "Weekly", Total: 48384, Remaining: 1000, ResetAt: now.Add(48 * time.Hour).UTC().Format(time.RFC3339)},
		}
		srv.SetAccounts([]*AccountState{acct})

		err := srv.classifyEmpty(context.Background(), acct, "gemini-pro")
		ue, ok := err.(*transport.UpstreamError)
		if !ok || ue.StatusCode != 429 || ue.Code != "rate_limit_error" {
			t.Fatalf("expected 429 rate_limit_error for 5h exhausted bucket, got %v", err)
		}
		if ue.Headers["retry-after"] == "" || ue.Headers["x-resets-at"] != resetTime {
			t.Fatalf("expected retry-after and x-resets-at=%q, got %+v", resetTime, ue.Headers)
		}

		hdrs := srv.lookupQuotaResetHeaders(context.Background(), acct, "gemini-pro")
		if hdrs["retry-after"] == "" || hdrs["x-resets-at"] != resetTime {
			t.Fatalf("lookupQuotaResetHeaders missed 5h bucket: %+v", hdrs)
		}

		// Weekly-only exhausted bucket (5h > 0, weekly == 0).
		weeklyReset := now.Add(48 * time.Hour).UTC().Format(time.RFC3339)
		acct.mu.Lock()
		acct.Quota = []gemini.QuotaBucket{
			{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 1000, ResetAt: resetTime},
			{ID: "weekly", Label: "Weekly", Total: 48384, Remaining: 0, ResetAt: weeklyReset},
		}
		acct.mu.Unlock()
		errWeekly := srv.classifyEmpty(context.Background(), acct, "gemini-pro")
		ueWeekly, ok := errWeekly.(*transport.UpstreamError)
		if !ok || ueWeekly.StatusCode != 429 || ueWeekly.Code != "rate_limit_error" {
			t.Fatalf("expected 429 rate_limit_error for weekly-only exhausted bucket, got %v", errWeekly)
		}
		if ueWeekly.Headers["retry-after"] == "" || ueWeekly.Headers["x-resets-at"] != weeklyReset {
			t.Fatalf("expected weekly x-resets-at=%q, got %+v", weeklyReset, ueWeekly.Headers)
		}
		hdrsWeekly := srv.lookupQuotaResetHeaders(context.Background(), acct, "gemini-pro")
		if hdrsWeekly["retry-after"] == "" || hdrsWeekly["x-resets-at"] != weeklyReset {
			t.Fatalf("lookupQuotaResetHeaders missed weekly bucket: %+v", hdrsWeekly)
		}
	})

	t.Run("5.4e_numeric_thinking_level_and_case_insensitive_model_prefix", func(t *testing.T) {
		srv := e2eNewServer(t, nil)
		var capturedLevel int
		up := &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				capturedLevel = opts.ThinkingLevel
				_ = onFrame(&gemini.LineParseResult{CID: "c", RID: "r", AnsweredModelID: testModel.ModelID, Frames: []gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)}})
				return &gemini.StreamResponse{Status: 200}, nil
			},
		}
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", map[string]any{
			"model":          testModel.ModelName,
			"thinking_level": 2,
			"messages":       []any{map[string]any{"role": "user", "content": "hi"}},
		}, map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 || capturedLevel != 2 {
			t.Fatalf("expected numeric thinking_level: 2 -> opts.ThinkingLevel 2, got code=%d level=%d", rec.Code, capturedLevel)
		}

		keys := gemini.DocumentedModelKeys(gemini.ModelInfo{
			ModelName:   "Gemini-4-Hydrogen",
			DisplayName: "Gemini-4-Hydrogen",
			Description: "Gemini-4-Hydrogen",
		})
		for _, k := range keys {
			if strings.HasPrefix(k, "gemini-gemini-") {
				t.Fatalf("DocumentedModelKeys produced duplicate prefix %q", k)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// TIER 1 — Feature Area E: Operational Reliability (6.2, 6.3a-c) & AGENTS.md Contracts
// ---------------------------------------------------------------------------

func TestE2E_T1_Operational_ImportEnvModelsCacheAccounting(t *testing.T) {
	t.Run("6.2_import_email_collision_and_preserve_existing_on_failed_probe", func(t *testing.T) {
		srv, cfg := e2eNewServerWithCfg(t, func(c *config.Config) {
			c.WebUI = true
		})
		if err := os.MkdirAll(cfg.AccountsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		acct1Path := filepath.Join(cfg.AccountsDir, "acct1.json")
		_, err := cookies.SaveCredentialFile(acct1Path, cookies.CredentialFile{
			Kind: "gemini-web", Name: "acct1", Email: "alice@example.com",
			Cookies: "SAPISID=good1; __Secure-1PSID=psid1", CapturedAt: time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			t.Fatal(err)
		}

		var failProbe atomic.Bool
		srv.clientFactory = func(a *AccountState) UpstreamClient {
			return &mockUpstream{
				bootFn: func(ctx context.Context) error {
					if failProbe.Load() {
						return transport.NewUpstreamError("bad cookies", 401, "upstream_auth_error", nil, nil)
					}
					return nil
				},
			}
		}
		srv.LoadCredentials()

		// 1. Re-import acct1 with failing probe must NOT delete acct1.json or evict acct1.
		failProbe.Store(true)
		recFail := e2eServeRequest(srv, http.MethodPost, "/v1/admin/accounts/import", map[string]any{
			"id":     "acct1",
			"cookie": "SAPISID=bad; __Secure-1PSID=bad",
		}, map[string]string{"Content-Type": "application/json"}, "")
		if recFail.Code == 200 {
			t.Fatalf("expected failed probe import to fail, got 200")
		}
		if _, statErr := os.Stat(acct1Path); statErr != nil {
			t.Fatalf("failed probe deleted existing account file acct1.json: %v", statErr)
		}
		if srv.findAccountByID("acct1") == nil {
			t.Fatal("failed probe evicted existing healthy acct1 from pool")
		}

		// 2. Two POST /v1/admin/accounts/import HTTP calls without "id" for colliding email prefixes (alice@gmail.com + alice@yahoo.com) end-to-end.
		failProbe.Store(false)
		modelsInner := fmt.Sprintf(`[null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[[%q,%q]]]`, testModel.ModelID, testModel.ModelName)
		modelsEsc, _ := json.Marshal(modelsInner)
		modelsBody := ")]}'\n\n" + `[["wrb.fr","otAQ7b",` + string(modelsEsc) + `]]`
		srv.clientFactory = func(a *AccountState) UpstreamClient {
			if failProbe.Load() {
				return &mockUpstream{
					bootFn: func(ctx context.Context) error {
						return transport.NewUpstreamError("bad cookies", 401, "upstream_auth_error", nil, nil)
					},
				}
			}
			cookieHdr := ""
			emailForImport := "alice@example.com"
			if a.FilePath != "" {
				if rec, _, loadErr := cookies.LoadAccountFile(a.FilePath); loadErr == nil && rec != nil {
					cookieHdr = rec.Cookies
					switch {
					case strings.Contains(rec.Cookies, "alice-gmail"):
						emailForImport = "alice@gmail.com"
					case strings.Contains(rec.Cookies, "alice-yahoo"):
						emailForImport = "alice@yahoo.com"
					}
				}
			}
			doer := &e2eStubDoer{
				fn: func(req *fhttp.Request) (*fhttp.Response, error) {
					u := req.URL.String()
					switch {
					case strings.Contains(u, "/app"):
						return &fhttp.Response{
							StatusCode: 200,
							Header:     fhttp.Header{},
							Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"SNlM0e":"at_imp","cfb2h":"bl_imp","FdrFJe":"111","oPEP7c":%q}`, emailForImport))),
						}, nil
					case strings.Contains(u, "batchexecute"):
						return &fhttp.Response{
							StatusCode: 200,
							Header:     fhttp.Header{},
							Body:       io.NopCloser(strings.NewReader(modelsBody)),
						}, nil
					default:
						return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader("[]"))}, nil
					}
				},
			}
			pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
			t.Cleanup(pool.Reset)
			return gemini.NewAccountClient(a.ID, cookieHdr, a.FilePath, pool, cookies.NewRotator())
		}

		recImp1 := e2eServeRequest(srv, http.MethodPost, "/v1/admin/accounts/import", map[string]any{
			"cookie": "SAPISID=alice-gmail; __Secure-1PSID=p1",
		}, map[string]string{"Content-Type": "application/json"}, "")
		if recImp1.Code != 200 {
			t.Fatalf("first email-derived import failed: %d %s", recImp1.Code, recImp1.Body.String())
		}
		imp1Obj := e2eDecodeJSONMap(t, recImp1.Body.Bytes())
		id1 := imp1Obj["account"].(map[string]any)["id"].(string)

		recImp2 := e2eServeRequest(srv, http.MethodPost, "/v1/admin/accounts/import", map[string]any{
			"cookie": "SAPISID=alice-yahoo; __Secure-1PSID=p2",
		}, map[string]string{"Content-Type": "application/json"}, "")
		if recImp2.Code != 200 {
			t.Fatalf("second colliding email-derived import failed: %d %s", recImp2.Code, recImp2.Body.String())
		}
		imp2Obj := e2eDecodeJSONMap(t, recImp2.Body.Bytes())
		id2 := imp2Obj["account"].(map[string]any)["id"].(string)

		if id1 != "alice" || id2 != "alice-2" {
			t.Fatalf("expected colliding email imports to derive distinct IDs 'alice' and 'alice-2', got %q and %q", id1, id2)
		}
		if srv.findAccountByID("alice") == nil || srv.findAccountByID("alice-2") == nil {
			t.Fatalf("expected both 'alice' and 'alice-2' present in pool after collision import")
		}

		// 3. Web UI isolated login launch JS handler passes proxyUrl and accountGroup
		// to both /v1/admin/accounts/browser-login and fallback /v1/admin/accounts/import.
		recUI := e2eServeRequest(srv, http.MethodGet, "/ui", nil, nil, "")
		if recUI.Code != 200 {
			t.Fatalf("GET /ui failed: %d", recUI.Code)
		}
		html := recUI.Body.String()
		const jsAnchor = "document.getElementById('btn-isolated-login').addEventListener"
		idx := strings.Index(html, jsAnchor)
		if idx < 0 {
			t.Fatalf("missing JS click handler %q in /ui HTML", jsAnchor)
		}
		endIdx := strings.Index(html[idx:], "document.getElementById('form-import')")
		if endIdx < 0 {
			t.Fatalf("missing form-import boundary after %q in /ui HTML", jsAnchor)
		}
		handler := html[idx : idx+endIdx]
		blIdx := strings.Index(handler, "/v1/admin/accounts/browser-login")
		impIdx := strings.Index(handler, "/v1/admin/accounts/import")
		if blIdx < 0 || impIdx <= blIdx {
			t.Fatalf("isolated login JS handler missing browser-login/import endpoints: %s", handler)
		}
		blBlock := handler[blIdx:impIdx]
		if catchIdx := strings.Index(blBlock, "catch"); catchIdx > 0 {
			blBlock = blBlock[:catchIdx]
		}
		impBlock := handler[impIdx:]
		if !strings.Contains(blBlock, "proxyUrl") || !strings.Contains(blBlock, "accountGroup") {
			t.Fatalf("browser-login request in isolated login handler must send proxyUrl and accountGroup, got: %s", blBlock)
		}
		if !strings.Contains(impBlock, "proxyUrl") || !strings.Contains(impBlock, "accountGroup") {
			t.Fatalf("fallback import request in isolated login handler must send proxyUrl and accountGroup, got: %s", impBlock)
		}
	})

	t.Run("6.3a_concurrent_SaveEnvFile_and_admin_config_patch", func(t *testing.T) {
		srv, cfg := e2eNewServerWithCfg(t, func(c *config.Config) {
			c.WebUI = true
		})
		srv.SetAccounts([]*AccountState{
			e2eNewAccount("a1", "a1@example.com", clientFromFrames(nil)),
			e2eNewAccount("a2", "a2@example.com", clientFromFrames(nil)),
		})
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				for j := 0; j < 5; j++ {
					if err := config.SaveEnvFile(cfg.EnvFile, map[string]string{
						"BRIDGE_MAX_RPM_PER_ACCOUNT": fmt.Sprintf("%d", 10+(idx+j)%20),
					}); err != nil {
						t.Errorf("SaveEnvFile failed: %v", err)
					}
					rec := e2eServeRequest(srv, http.MethodPatch, "/v1/admin/config", map[string]any{
						"maxRpmPerAccount": 15 + (idx+j)%10,
						"persist":          true,
					}, map[string]string{"Content-Type": "application/json"}, "")
					if rec.Code != 200 {
						t.Errorf("PATCH /v1/admin/config failed: %d %s", rec.Code, rec.Body.String())
					}
				}
			}(i)
		}
		wg.Wait()
		envMap := config.LoadEnvFile(cfg.EnvFile)
		if envMap["BRIDGE_MAX_RPM_PER_ACCOUNT"] == "" {
			t.Fatalf("expected valid config.env after concurrent writes, map=%+v", envMap)
		}
	})

	t.Run("6.3b_models_singleflight_and_session_cache_flush_reload", func(t *testing.T) {
		srv, cfg := e2eNewServerWithCfg(t, func(c *config.Config) {
			c.SessionCache = true
			c.ModelRefreshEvery = 5 * time.Minute
		})
		var modelCalls atomic.Int32
		started := make(chan struct{})
		var startedOnce sync.Once
		gate := make(chan struct{})
		up := &mockUpstream{
			modelsFn: func(ctx context.Context, force bool) (*gemini.AvailableModelsResult, error) {
				if force {
					modelCalls.Add(1)
					startedOnce.Do(func() { close(started) })
					<-gate
				}
				return &gemini.AvailableModelsResult{Status: 200, Models: []gemini.ModelInfo{testModel}, Capacity: 2}, nil
			},
			streamFn: clientFromFrames([]gemini.Frame{makeFrame("cached-reply", "", true, testModel.ModelID)}).streamFn,
		}
		acct := e2eNewAccount("a1", "a1@example.com", up)
		acct.ModelsAt = time.Now().Add(-10 * time.Minute) // stale warm cache -> triggers background singleflight refresh
		srv.SetAccounts([]*AccountState{acct})

		errCh := make(chan string, 10)
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := e2eServeRequest(srv, http.MethodGet, "/v1/models", nil, nil, "")
				if rec.Code != http.StatusOK {
					errCh <- fmt.Sprintf("GET /v1/models unexpected status %d: %s", rec.Code, rec.Body.String())
				}
			}()
		}
		wg.Wait()
		close(errCh)
		for msg := range errCh {
			t.Fatal(msg)
		}
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for background /v1/models singleflight refresh to enter modelsFn")
		}
		// Another GET /v1/models while refresh is in-flight must not launch a second refresh.
		if recDuring := e2eServeRequest(srv, http.MethodGet, "/v1/models", nil, nil, ""); recDuring.Code != 200 {
			t.Fatalf("GET /v1/models during refresh failed: %d", recDuring.Code)
		}
		close(gate)
		deadline := time.Now().Add(2 * time.Second)
		for srv.modelsRefreshing.Load() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if srv.modelsRefreshing.Load() {
			t.Fatal("background /v1/models refresh did not finish")
		}
		if got := modelCalls.Load(); got != 1 {
			t.Fatalf("expected /v1/models background refresh singleflight to issue exactly 1 call, got %d", got)
		}

		// Session cache flush + reload on new Server instance.
		msgs := []any{
			map[string]any{"role": "user", "content": "turn 1"},
		}
		rec1 := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", map[string]any{
			"model":    testModel.ModelName,
			"messages": msgs,
		}, map[string]string{"Content-Type": "application/json"}, "")
		if rec1.Code != 200 {
			t.Fatalf("turn 1 failed: %d", rec1.Code)
		}
		srv.FlushSessionCacheNow()
		cachePath := filepath.Join(cfg.HomeDir, "session-cache.json")
		fi, err := os.Stat(cachePath)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("expected session-cache.json with 0600 perms, err=%v mode=%v", err, fi)
		}

		srv2 := New(cfg)
		t.Cleanup(func() { srv2.FlushSessionCacheNow() })
		var gotMeta []any
		up2 := &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				gotMeta = opts.Metadata
				_ = onFrame(&gemini.LineParseResult{CID: "synthetic-cid", RID: "synthetic-rid", AnsweredModelID: testModel.ModelID, Frames: []gemini.Frame{makeFrame("turn 2", "", true, testModel.ModelID)}})
				return &gemini.StreamResponse{Status: 200, CID: "synthetic-cid", RID: "synthetic-rid"}, nil
			},
		}
		srv2.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up2)})
		rec2 := e2eServeRequest(srv2, http.MethodPost, "/v1/chat/completions", map[string]any{
			"model": testModel.ModelName,
			"messages": []any{
				map[string]any{"role": "user", "content": "turn 1"},
				map[string]any{"role": "assistant", "content": "cached-reply"},
				map[string]any{"role": "user", "content": "turn 2"},
			},
		}, map[string]string{"Content-Type": "application/json"}, "")
		if rec2.Code != 200 || len(gotMeta) == 0 {
			t.Fatalf("expected new Server to hit persisted session-cache.json, code=%d meta=%v", rec2.Code, gotMeta)
		}
	})

	t.Run("6.3c_failover_updates_mid_checks_rpm_and_mcp_media_accounting", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.MaxAttempts = 3
			c.MaxRPMPerAccount = 1
		})
		mA := gemini.ModelInfo{ModelName: "gemini-pro", ModelID: testModel.ModelID, Capacity: 2}
		mB := gemini.ModelInfo{ModelName: "gemini-4-hydrogen", ModelID: "dddddddddddddddd", Capacity: 2, Aliases: []string{"gemini-pro"}}

		up1 := &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				return nil, transport.NewUpstreamError("transient", 503, "upstream_transient", nil, nil)
			},
		}
		var quotaCalls atomic.Int32
		streamStarted := make(chan struct{})
		releaseSearch := make(chan struct{})
		up2 := &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				select {
				case <-streamStarted:
				default:
					close(streamStarted)
				}
				<-releaseSearch
				_ = onFrame(&gemini.LineParseResult{
					CID: "c2", RID: "r2", AnsweredModelID: mB.ModelID,
					Frames: []gemini.Frame{makeFrame("from-mB", "", true, mB.ModelID)},
				})
				return &gemini.StreamResponse{Status: 200, CID: "c2", RID: "r2"}, nil
			},
			quotaFn: func(ctx context.Context) []gemini.QuotaBucket {
				quotaCalls.Add(1)
				return []gemini.QuotaBucket{{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 2399}}
			},
		}
		a1 := e2eNewAccount("a1", "a1@example.com", up1, mA)
		a2 := e2eNewAccount("a2", "a2@example.com", up2, mB)
		srv.SetAccounts([]*AccountState{a1, a2})

		// Close releaseSearch immediately for failover test.
		close(releaseSearch)
		rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("gemini-pro", nil), map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 || rec.Header().Get("X-GSB-Model") != "gemini-4-hydrogen" {
			t.Fatalf("expected failover to update X-GSB-Model to gemini-4-hydrogen, got code=%d model=%q", rec.Code, rec.Header().Get("X-GSB-Model"))
		}

		// Reset a2 timestamps so we can test MCP activeStreams + quota refresh accounting.
		a2.mu.Lock()
		a2.reqTimestamps = nil
		a2.QuotaAt = time.Now().Add(-2 * time.Minute)
		a2.mu.Unlock()
		streamStarted2 := make(chan struct{})
		releaseSearch2 := make(chan struct{})
		up2.streamFn = func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			close(streamStarted2)
			<-releaseSearch2
			_ = onFrame(&gemini.LineParseResult{
				CID: "cm", RID: "rm", AnsweredModelID: mB.ModelID,
				Frames: []gemini.Frame{makeFrame("search ok", "", true, mB.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200, CID: "cm", RID: "rm"}, nil
		}
		srv.SetAccounts([]*AccountState{a2})

		mcpDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			mcpDone <- e2eServeRequest(srv, http.MethodPost, "/mcp", map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": "web_search", "arguments": map[string]any{"query": "go"}},
			}, map[string]string{"Authorization": "Bearer public", "Content-Type": "application/json"}, "")
		}()
		<-streamStarted2
		recAccts := e2eServeRequest(srv, http.MethodGet, "/v1/accounts", nil, nil, "")
		acctsMap := e2eDecodeJSONMap(t, recAccts.Body.Bytes())
		activeDuring := acctsMap["accounts"].([]any)[0].(map[string]any)["activeStreams"].(float64)
		close(releaseSearch2)
		recMCP := <-mcpDone
		if recMCP.Code != http.StatusOK {
			t.Fatalf("MCP web_search failed: %d %s", recMCP.Code, recMCP.Body.String())
		}
		if activeDuring < 1 {
			t.Fatalf("expected MCP web_search to increment activeStreams during execution, got %v", activeDuring)
		}
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			a2.mu.RLock()
			refreshing := a2.quotaRefreshing
			a2.mu.RUnlock()
			if quotaCalls.Load() > 0 && !refreshing {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		a2.mu.RLock()
		stillRefreshingAfterMCP := a2.quotaRefreshing
		a2.mu.RUnlock()
		if quotaCalls.Load() == 0 || stillRefreshingAfterMCP {
			t.Fatalf("expected MCP web_search success to complete quota refresh, calls=%d refreshing=%v", quotaCalls.Load(), stillRefreshingAfterMCP)
		}

		// Media endpoint activeStreams + quota refresh + failure/quarantine accounting (Audit 6.3c).
		a2.mu.Lock()
		a2.reqTimestamps = nil
		a2.QuotaAt = time.Now().Add(-2 * time.Minute)
		a2.mu.Unlock()
		quotaBeforeMedia := quotaCalls.Load()
		mediaStarted := make(chan struct{})
		releaseMedia := make(chan struct{})
		mediaUp := &e2eMediaUpstream{
			mockUpstream: &mockUpstream{
				streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
					close(mediaStarted)
					<-releaseMedia
					_ = onFrame(&gemini.LineParseResult{
						CID: "cimg", RID: "rimg", AnsweredModelID: mB.ModelID,
						Frames: []gemini.Frame{{
							RCID: "rcimg", Text: "ok", Done: true, AnsweredModelID: mB.ModelID,
							GeneratedImages: []gemini.GeneratedImage{{URL: "https://lh3.googleusercontent.com/gg-dl/m=s2048"}},
						}},
					})
					return &gemini.StreamResponse{Status: 200, CID: "cimg", RID: "rimg"}, nil
				},
				quotaFn: up2.quotaFn,
			},
		}
		a2.mu.Lock()
		a2.Client = mediaUp
		a2.mu.Unlock()
		mediaDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			mediaDone <- e2eServeRequest(srv, http.MethodPost, "/v1/images/generations", map[string]any{"prompt": "mountain"}, map[string]string{"Content-Type": "application/json"}, "")
		}()
		<-mediaStarted
		recAcctsMedia := e2eServeRequest(srv, http.MethodGet, "/v1/accounts", nil, nil, "")
		acctsMediaMap := e2eDecodeJSONMap(t, recAcctsMedia.Body.Bytes())
		activeMedia := acctsMediaMap["accounts"].([]any)[0].(map[string]any)["activeStreams"].(float64)
		close(releaseMedia)
		recMediaGen := <-mediaDone
		if recMediaGen.Code != 200 {
			t.Fatalf("POST /v1/images/generations failed: %d %s", recMediaGen.Code, recMediaGen.Body.String())
		}
		if activeMedia < 1 {
			t.Fatalf("expected POST /v1/images/generations to increment activeStreams during execution, got %v", activeMedia)
		}
		deadline = time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			a2.mu.RLock()
			refreshing := a2.quotaRefreshing
			a2.mu.RUnlock()
			if quotaCalls.Load() > quotaBeforeMedia && !refreshing {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		a2.mu.RLock()
		stillRefreshingAfterMedia := a2.quotaRefreshing
		a2.mu.RUnlock()
		if quotaCalls.Load() <= quotaBeforeMedia || stillRefreshingAfterMedia {
			t.Fatalf("expected POST /v1/images/generations success to complete quota refresh, calls=%d before=%d refreshing=%v", quotaCalls.Load(), quotaBeforeMedia, stillRefreshingAfterMedia)
		}

		// Failure / quarantine accounting on /v1/videos/generations.
		a2.mu.Lock()
		a2.reqTimestamps = nil
		a2.mu.Unlock()
		mediaUp.streamFn = func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			return nil, transport.NewUpstreamError("TOS_VIOLATION", 403, "account_banned", nil, nil)
		}
		recVidErr := e2eServeRequest(srv, http.MethodPost, "/v1/videos/generations", map[string]any{"prompt": "sunset"}, map[string]string{"Content-Type": "application/json"}, "")
		if recVidErr.Code != 403 {
			t.Fatalf("expected 403 on account_banned video generation, got %d", recVidErr.Code)
		}
		a2.mu.RLock()
		quarantined := a2.Quarantined
		activeAfter := a2.activeStreams
		a2.mu.RUnlock()
		if !quarantined || activeAfter != 0 {
			t.Fatalf("expected /v1/videos/generations account_banned to quarantine account and clear activeStreams, got quarantined=%v active=%d", quarantined, activeAfter)
		}
	})
}

func TestE2E_T1_Contracts_HealthReadyzModelsRefresh(t *testing.T) {
	srv := e2eNewServer(t, nil)

	// 0 accounts -> /readyz?strict=1 returns 503.
	recReady0 := e2eServeRequest(srv, http.MethodGet, "/readyz?strict=1", nil, nil, "")
	if recReady0.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on /readyz?strict=1 with 0 accounts, got %d", recReady0.Code)
	}

	up := clientFromFrames(nil)
	a1 := e2eNewAccount("a1", "a1@example.com", up)
	a2 := e2eNewAccount("a2", "a2@example.com", up)
	a2.Quarantined = true
	srv.SetAccounts([]*AccountState{a1, a2})

	recHealth := e2eServeRequest(srv, http.MethodGet, "/health", nil, nil, "")
	if recHealth.Code != 200 || !strings.Contains(recHealth.Body.String(), `"accounts":2`) {
		t.Fatalf("unexpected /health: %d %s", recHealth.Code, recHealth.Body.String())
	}

	recReadyPartial := e2eServeRequest(srv, http.MethodGet, "/readyz?strict=1", nil, nil, "")
	if recReadyPartial.Code != 200 || !strings.Contains(recReadyPartial.Body.String(), `"status":"degraded"`) {
		t.Fatalf("expected 200 degraded on partial healthy pool, got %d %s", recReadyPartial.Code, recReadyPartial.Body.String())
	}

	recModels := e2eServeRequest(srv, http.MethodGet, "/v1/models", nil, nil, "")
	etag := recModels.Header().Get("ETag")
	if recModels.Code != 200 || etag == "" {
		t.Fatalf("expected /v1/models 200 + ETag, got %d etag=%q", recModels.Code, etag)
	}
	rec304 := e2eServeRequest(srv, http.MethodGet, "/v1/models", nil, map[string]string{"If-None-Match": etag}, "")
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified on matching ETag, got %d", rec304.Code)
	}

	recRefBad := e2eServeRequest(srv, http.MethodPost, "/v1/refresh", nil, map[string]string{"Origin": "https://evil.example"}, "")
	if recRefBad.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on /v1/refresh with Origin header, got %d", recRefBad.Code)
	}
	recRefOK := e2eServeRequest(srv, http.MethodPost, "/v1/refresh", nil, nil, "")
	if recRefOK.Code != 200 {
		t.Fatalf("expected 200 on /v1/refresh without Origin, got %d: %s", recRefOK.Code, recRefOK.Body.String())
	}
}

func TestE2E_T1_Contracts_ChatModelPrefixesThinkAndGrounding(t *testing.T) {
	for _, includeGrounding := range []bool{false, true} {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.IncludeGrounding = includeGrounding
		})
		var capturedOpts gemini.StreamOptions
		up := &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				capturedOpts = opts
				src := []gemini.GroundingSource{{URL: "https://example.org/doc", Title: "Doc"}}
				_ = onFrame(&gemini.LineParseResult{
					CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID, GroundingSources: src,
					Frames: []gemini.Frame{makeFrame("grounded answer", "", true, testModel.ModelID)},
				})
				return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1", GroundingSources: src}, nil
			},
		}
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		for _, prefix := range []string{"gsb/", "google/", "models/", "vx/", "vertex/"} {
			rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", map[string]any{
				"model":       prefix + testModel.ModelName + "@think=high",
				"temporary":   true,
				"temperature": 0.9,
				"top_p":       0.5,
				"seed":        42,
				"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
			}, map[string]string{"Content-Type": "application/json"}, "")
			if rec.Code != 200 || capturedOpts.ThinkingLevel != 2 || !capturedOpts.Temporary {
				t.Fatalf("prefix %q failed: code=%d level=%d temp=%v body=%s", prefix, rec.Code, capturedOpts.ThinkingLevel, capturedOpts.Temporary, rec.Body.String())
			}
			hasSources := strings.Contains(rec.Body.String(), "https://example.org/doc")
			if hasSources != includeGrounding {
				t.Fatalf("IncludeGrounding=%v mismatch on response sources: %s", includeGrounding, rec.Body.String())
			}
		}
	}
}

func TestE2E_T1_Contracts_ImagesAndVideosEndpoints(t *testing.T) {
	srv := e2eNewServer(t, nil)
	png16x9 := e2ePNGBytes(16, 9)
	up := &e2eMediaUpstream{
		mockUpstream: clientFromFrames([]gemini.Frame{{
			RCID:            "rc1",
			Text:            "done",
			Done:            true,
			AnsweredModelID: testModel.ModelID,
			GeneratedImages: []gemini.GeneratedImage{{
				URL:   "https://lh3.googleusercontent.com/gg-dl/img1=s2048",
				Title: "Cat",
			}},
			GeneratedVideos: []gemini.GeneratedVideo{{
				URL:          "https://lh3.googleusercontent.com/gg-dl/vid1.mp4",
				ThumbnailURL: "https://lh3.googleusercontent.com/gg-dl/thumb1.jpg",
				Title:        "CatVideo",
			}},
		}}),
		downloadFn: func(ctx context.Context, rawURL, fallbackURL string) ([]byte, string, error) {
			if strings.Contains(rawURL, "vid1") {
				return []byte("\x00\x00\x00\x18ftypmp42"), "video/mp4", nil
			}
			return png16x9, "image/png", nil
		},
	}
	srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

	recImg := e2eServeRequest(srv, http.MethodPost, "/v1/images/generations", map[string]any{
		"prompt":          "a cat",
		"size":            "1280x720",
		"response_format": "b64_json",
	}, map[string]string{"Content-Type": "application/json"}, "")
	if recImg.Code != 200 {
		t.Fatalf("images/generations failed: %d %s", recImg.Code, recImg.Body.String())
	}
	imgBody := e2eDecodeJSONMap(t, recImg.Body.Bytes())
	imgItem := imgBody["data"].([]any)[0].(map[string]any)
	if imgItem["b64_json"] != base64.StdEncoding.EncodeToString(png16x9) || imgItem["size"] != "16x9" || imgItem["aspect_ratio"] != "16:9" {
		t.Fatalf("unexpected image response item: %+v", imgItem)
	}

	recConflict := e2eServeRequest(srv, http.MethodPost, "/v1/images/generations", map[string]any{
		"prompt":       "a cat",
		"aspect_ratio": "16:9",
		"size":         "1080x1920",
	}, map[string]string{"Content-Type": "application/json"}, "")
	if recConflict.Code != 400 {
		t.Fatalf("conflicting aspect_ratio + size must return 400, got %d", recConflict.Code)
	}

	recAuto := e2eServeRequest(srv, http.MethodPost, "/v1/images/generations", map[string]any{
		"prompt": "a cat",
		"size":   "auto",
	}, map[string]string{"Content-Type": "application/json"}, "")
	if recAuto.Code != 200 {
		t.Fatalf("size=auto must return 200, got %d: %s", recAuto.Code, recAuto.Body.String())
	}

	recVid := e2eServeRequest(srv, http.MethodPost, "/v1/videos/generations", map[string]any{
		"prompt": "a running cat",
	}, map[string]string{"Content-Type": "application/json"}, "")
	if recVid.Code != 200 || !strings.Contains(recVid.Body.String(), "thumbnail_url") || !strings.Contains(recVid.Body.String(), "/v1/media/") {
		t.Fatalf("videos/generations unexpected: %d %s", recVid.Code, recVid.Body.String())
	}
}

func TestE2E_T1_Contracts_MCPSurface(t *testing.T) {
	srv := mcpTestServer(t, mcpSearchUpstream("Go 1.26 notes", []gemini.GroundingSource{
		{URL: "https://go.dev/doc/go1.26", Title: "Go 1.26"},
	}, nil))

	for _, path := range []string{"/mcp", "/v1/mcp"} {
		recInit := postMCP(t, srv, path, "audit", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18"}})
		if recInit.Code != 200 {
			t.Fatalf("%s initialize failed: %d", path, recInit.Code)
		}
		recCall := postMCP(t, srv, path, "audit", map[string]any{
			"jsonrpc": "2.0", "id": 2, "method": "tools/call",
			"params": map[string]any{"name": "web_search", "arguments": map[string]any{"query": "go 1.26"}},
		})
		if recCall.Code != 200 || recCall.Header().Get("X-GSB-Sources") != "1" || recCall.Header().Get("X-GSB-Duration-Ms") == "" {
			t.Fatalf("%s tools/call headers unexpected: code=%d sources=%q dur=%q", path, recCall.Code, recCall.Header().Get("X-GSB-Sources"), recCall.Header().Get("X-GSB-Duration-Ms"))
		}
	}

	recNotif := postMCP(t, srv, "/mcp", "audit", map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if recNotif.Code != http.StatusAccepted {
		t.Fatalf("expected 202 notification, got %d", recNotif.Code)
	}
	recGet := e2eServeRequest(srv, http.MethodGet, "/mcp", nil, nil, "")
	if recGet.Code != http.StatusMethodNotAllowed || !strings.Contains(recGet.Header().Get("Allow"), "POST") {
		t.Fatalf("expected 405 Allow: POST on GET /mcp, got %d allow=%q", recGet.Code, recGet.Header().Get("Allow"))
	}
	huge := strings.Repeat("a", 8*1024*1024+32)
	recBig := e2eServeRequest(srv, http.MethodPost, "/mcp", huge, map[string]string{"Authorization": "Bearer audit"}, "")
	if !strings.Contains(recBig.Body.String(), "exceeds 8MB limit") {
		t.Fatalf("expected 8MB JSON-RPC error, got %s", recBig.Body.String())
	}
}

func TestE2E_T1_Contracts_AdminConfigAndSessionEndpoints(t *testing.T) {
	srv := e2eNewServer(t, func(c *config.Config) {
		c.WebUI = true
	})
	a1 := e2eNewAccount("a1", "a1@example.com", &mockUpstream{
		quotaFn: func(ctx context.Context) []gemini.QuotaBucket {
			return []gemini.QuotaBucket{{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 2300}}
		},
	})
	a1.Quarantined = true
	srv.SetAccounts([]*AccountState{a1})

	if rec := e2eServeRequest(srv, http.MethodGet, "/v1/sessions", nil, nil, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "cache_entries") {
		t.Fatalf("/v1/sessions failed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e2eServeRequest(srv, http.MethodGet, "/v1/quota?fresh=1", nil, nil, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "2300") {
		t.Fatalf("/v1/quota?fresh=1 failed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e2eServeRequest(srv, http.MethodGet, "/v1/admin/config", nil, nil, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "accountStrategy") {
		t.Fatalf("/v1/admin/config failed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e2eServeRequest(srv, http.MethodPost, "/v1/admin/accounts/a1/unquarantine", nil, nil, ""); rec.Code != 200 || a1.Quarantined {
		t.Fatalf("unquarantine failed: %d q=%v", rec.Code, a1.Quarantined)
	}
	if rec := e2eServeRequest(srv, http.MethodPost, "/v1/admin/accounts/a1/rotate", nil, nil, ""); rec.Code != 200 {
		t.Fatalf("rotate failed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e2eServeRequest(srv, http.MethodDelete, "/v1/admin/accounts/a1", nil, nil, ""); rec.Code != 200 || len(srv.Accounts()) != 0 {
		t.Fatalf("delete failed: %d remaining=%d", rec.Code, len(srv.Accounts()))
	}
}

// ---------------------------------------------------------------------------
// TIER 2 — Boundary & Corner Cases (Areas A–E)
// ---------------------------------------------------------------------------

func TestE2E_T2_Boundaries_Security(t *testing.T) {
	t.Run("same_length_prefix_constant_time_mismatch", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) { c.APIKey = "secret-token-1234" })
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", clientFromFrames(nil))})
		rec := e2eServeRequest(srv, http.MethodGet, "/v1/models", nil, map[string]string{"Authorization": "Bearer secret-token-1235"}, "")
		if rec.Code != 401 {
			t.Fatalf("expected 401 for same-length mismatch, got %d", rec.Code)
		}
	})

	t.Run("ipv6_xff_and_loopback_host_variants", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) { c.WebUI = true; c.APIKey = "public" })
		recBadXFF := e2eServeRequest(srv, http.MethodGet, "/v1/admin/config", nil, map[string]string{
			"Host":            "[::1]:20133",
			"x-forwarded-for": "::1, 2001:db8::1",
		}, "[::1]:5000")
		if recBadXFF.Code != 403 {
			t.Fatalf("expected 403 for IPv6 multi-hop XFF, got %d", recBadXFF.Code)
		}
		recNipIO := e2eServeRequest(srv, http.MethodGet, "/v1/admin/config", nil, map[string]string{
			"Host": "127.0.0.1.nip.io:20133",
		}, "127.0.0.1:5000")
		if recNipIO.Code != 403 {
			t.Fatalf("expected 403 for DNS rebinding domain 127.0.0.1.nip.io, got %d", recNipIO.Code)
		}
	})

	t.Run("local_download_cmd_flag_injection_and_newlines", func(t *testing.T) {
		srv := e2eNewServer(t, nil)
		up := &e2eMediaUpstream{
			mockUpstream: clientFromFrames([]gemini.Frame{{
				RCID: "rc1", Done: true, AnsweredModelID: testModel.ModelID,
				GeneratedImages: []gemini.GeneratedImage{{URL: "https://lh3.googleusercontent.com/gg-dl/x=s2048"}},
			}}),
		}
		acct := e2eNewAccount("a1", "a1@example.com", up)
		srv.SetAccounts([]*AccountState{acct})
		add, _, err := srv.executeServerSideMediaCalls(context.Background(), []*AccountState{acct}, AuthScope{OK: true, AccountID: "a1"}, acct, &testModel, []toolbridge.Call{{
			Name:      toolbridge.BuiltinMediaToolName,
			Arguments: map[string]any{"media_type": "image", "prompt": "p", "output_path": "-o\n$(id)`uname`'../../etc/passwd\r"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		outObj := e2eExtractMediaToolResultJSON(t, add)
		_, fileArg := e2eAssertPOSIXSingleQuotedDownloadCmd(t, outObj, "")
		if fileArg != "passwd" {
			t.Fatalf("expected only safe basename, got %q", fileArg)
		}
	})

	t.Run("origin_null_csrf_blocked_and_charset_utf8_allowed", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) { c.APIKey = "public" })
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", clientFromFrames([]gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)}))})
		recNull := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{
			"Origin":       "null",
			"Content-Type": "application/json",
		}, "")
		if recNull.Code != 403 {
			t.Fatalf("expected Origin: null blocked with 403 in public mode, got %d", recNull.Code)
		}
		recCharset := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{
			"Content-Type": "application/json; charset=utf-8",
		}, "")
		if recCharset.Code != 200 {
			t.Fatalf("expected application/json; charset=utf-8 allowed, got %d: %s", recCharset.Code, recCharset.Body.String())
		}
	})

	t.Run("media_path_traversal_and_zero_byte_media", func(t *testing.T) {
		srv := e2eNewServer(t, nil)
		recTrav := e2eServeRequest(srv, http.MethodGet, "/v1/media/../../etc/passwd", nil, nil, "")
		if recTrav.Code != 404 && recTrav.Code != 400 {
			t.Fatalf("expected 404/400 on media traversal, got %d", recTrav.Code)
		}
		recEmpty := e2eServeRequest(srv, http.MethodGet, "/v1/media/", nil, nil, "")
		if recEmpty.Code != 404 {
			t.Fatalf("expected 404 on empty media id, got %d", recEmpty.Code)
		}

		// Zero-byte upstream media download must not panic/500 and must preserve security headers.
		var declMIME atomic.Value
		declMIME.Store("image/png")
		up := &e2eMediaUpstream{
			mockUpstream: clientFromFrames(nil),
			downloadFn: func(ctx context.Context, rawURL, fallbackURL string) ([]byte, string, error) {
				return []byte{}, declMIME.Load().(string), nil
			},
		}
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})
		_, zeroEntry := srv.registerMedia(context.Background(), "a1", "https://lh3.googleusercontent.com/gg-dl/zero1", "", "image/png", "png", "Zero1")
		recZero := e2eServeRequest(srv, http.MethodGet, "/v1/media/"+zeroEntry.ID, nil, nil, "")
		if recZero.Code != 200 || recZero.Body.Len() != 0 {
			t.Fatalf("expected 200 empty body for zero-byte media, got %d len=%d", recZero.Code, recZero.Body.Len())
		}
		if recZero.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(recZero.Header().Get("Content-Security-Policy"), "sandbox") {
			t.Fatalf("missing security headers on zero-byte media: %+v", recZero.Header())
		}

		// Zero-byte with empty declared MIME falls back to octet-stream + attachment (not text/plain).
		declMIME.Store("")
		_, zeroNoMIME := srv.registerMedia(context.Background(), "a1", "https://lh3.googleusercontent.com/gg-dl/zero2", "", "", "bin", "Zero2")
		recZeroBin := e2eServeRequest(srv, http.MethodGet, "/v1/media/"+zeroNoMIME.ID, nil, nil, "")
		if recZeroBin.Code != 200 || recZeroBin.Body.Len() != 0 || recZeroBin.Header().Get("Content-Type") != "application/octet-stream" {
			t.Fatalf("expected 200 octet-stream empty body for undeclared zero-byte media, got %d ct=%q len=%d", recZeroBin.Code, recZeroBin.Header().Get("Content-Type"), recZeroBin.Body.Len())
		}
	})
}

func TestE2E_T2_Boundaries_Concurrency(t *testing.T) {
	t.Run("single_account_status_lock_contention", func(t *testing.T) {
		srv := e2eNewServer(t, nil)
		a1 := e2eNewAccount("a1", "a1@example.com", clientFromFrames(nil))
		srv.SetAccounts([]*AccountState{a1})
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ctx.Err() == nil {
					_ = srv.accountStatusMap(a1, srv.now())
					srv.recordModelExhausted(a1, testModel.ModelName)
				}
			}()
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("deadlock on single-account contention")
		}
	})

	t.Run("early_sse_grace_1ns_vs_0", func(t *testing.T) {
		for _, grace := range []time.Duration{time.Nanosecond, 0} {
			srv := e2eNewServer(t, func(c *config.Config) {
				c.EarlySSEGrace = grace
			})
			srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", clientFromFrames([]gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)}))})
			rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", map[string]any{"stream": true}), map[string]string{"Content-Type": "application/json"}, "")
			if rec.Code != 200 {
				t.Fatalf("grace=%v failed: %d", grace, rec.Code)
			}
		}
	})

	t.Run("invalid_admin_config_patch_does_not_corrupt_state", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) { c.WebUI = true })
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", clientFromFrames(nil))})
		rec1 := e2eServeRequest(srv, http.MethodPatch, "/v1/admin/config", map[string]any{"maxRpmPerAccount": -1}, map[string]string{"Content-Type": "application/json"}, "")
		rec2 := e2eServeRequest(srv, http.MethodPatch, "/v1/admin/config", map[string]any{"accountStrategy": "bogus"}, map[string]string{"Content-Type": "application/json"}, "")
		if rec1.Code != 400 || rec2.Code != 400 {
			t.Fatalf("expected 400 for invalid config patch, got %d, %d", rec1.Code, rec2.Code)
		}
	})

	t.Run("recordModelExhausted_strike_escalation_under_concurrent_quota_reads", func(t *testing.T) {
		srv := e2eNewServer(t, nil)
		a1 := e2eNewAccount("a1", "a1@example.com", clientFromFrames(nil))
		srv.SetAccounts([]*AccountState{a1})
		errCh := make(chan string, 32)
		reportErr := func(msg string) {
			select {
			case errCh <- msg:
			default:
			}
		}
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 7; j++ {
					srv.recordModelExhausted(a1, testModel.ModelName)
				}
			}()
		}
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 7; j++ {
					rec := e2eServeRequest(srv, http.MethodGet, "/v1/quota", nil, nil, "")
					if rec.Code != http.StatusOK {
						reportErr(fmt.Sprintf("GET /v1/quota unexpected status %d: %s", rec.Code, rec.Body.String()))
						return
					}
					var payload struct {
						Accounts []map[string]any `json:"accounts"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || len(payload.Accounts) != 1 {
						reportErr(fmt.Sprintf("GET /v1/quota invalid payload (err=%v): %s", err, rec.Body.String()))
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errCh)
		for msg := range errCh {
			t.Fatal(msg)
		}

		subID := "sub:" + strings.ToLower(strings.TrimSpace(testModel.ModelName))
		now := srv.now()
		a1.mu.RLock()
		strikes := a1.subStrikes[subID]
		quotaCopy := append([]gemini.QuotaBucket(nil), a1.Quota...)
		a1.mu.RUnlock()

		if strikes != 28 {
			t.Fatalf("expected 28 escalated strikes for %q, got %d", subID, strikes)
		}
		if len(quotaCopy) == 0 {
			t.Fatalf("expected non-empty a1.Quota after recordModelExhausted")
		}
		var foundSynthetic *gemini.QuotaBucket
		for i := range quotaCopy {
			if quotaCopy[i].ID == subID {
				foundSynthetic = &quotaCopy[i]
				break
			}
		}
		if foundSynthetic == nil {
			t.Fatalf("expected synthetic quota bucket %q in %+v", subID, quotaCopy)
		}
		if !foundSynthetic.Synthetic || foundSynthetic.Remaining != 0 || foundSynthetic.Strikes != 28 {
			t.Fatalf("unexpected synthetic bucket state: %+v", *foundSynthetic)
		}
		resetAt, err := time.Parse(time.RFC3339, foundSynthetic.ResetAt)
		if err != nil || !resetAt.After(now.Add(23*time.Hour)) {
			t.Fatalf("expected escalated 24h cooldown ResetAt after %s, got %q (err=%v)", now.UTC().Format(time.RFC3339), foundSynthetic.ResetAt, err)
		}
		if st, _ := AccountStatus(a1, now); st != StatusExhausted {
			t.Fatalf("expected AccountStatus %q after strike escalation, got %q", StatusExhausted, st)
		}
	})

	t.Run("concurrent_delete_account_and_chat", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) { c.WebUI = true })
		a1 := e2eNewAccount("a1", "a1@example.com", clientFromFrames([]gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)}))
		srv.SetAccounts([]*AccountState{a1})
		errCh := make(chan string, 4)
		reportErr := func(msg string) {
			select {
			case errCh <- msg:
			default:
			}
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			rec := e2eServeRequest(srv, http.MethodDelete, "/v1/admin/accounts/a1", nil, nil, "")
			if rec.Code != http.StatusOK {
				reportErr(fmt.Sprintf("DELETE /v1/admin/accounts/a1 unexpected status %d: %s", rec.Code, rec.Body.String()))
			}
		}()
		go func() {
			defer wg.Done()
			rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Content-Type": "application/json"}, "")
			switch rec.Code {
			case http.StatusOK, http.StatusNotFound, http.StatusTooManyRequests, http.StatusServiceUnavailable:
			default:
				reportErr(fmt.Sprintf("POST /v1/chat/completions unexpected status %d: %s", rec.Code, rec.Body.String()))
			}
		}()
		wg.Wait()
		close(errCh)
		for msg := range errCh {
			t.Fatal(msg)
		}

		recAccounts := e2eServeRequest(srv, http.MethodGet, "/v1/accounts", nil, nil, "")
		if recAccounts.Code != http.StatusOK {
			t.Fatalf("GET /v1/accounts unexpected status %d: %s", recAccounts.Code, recAccounts.Body.String())
		}
		payload := e2eDecodeJSONMap(t, recAccounts.Body.Bytes())
		accts, ok := payload["accounts"].([]any)
		if !ok || len(accts) != 0 {
			t.Fatalf("expected 0 accounts after concurrent delete, got %v", payload["accounts"])
		}
		if remaining := srv.Accounts(); len(remaining) != 0 {
			t.Fatalf("expected srv.Accounts() empty after delete, got %d", len(remaining))
		}
	})
}

func TestE2E_T2_Boundaries_AuthAndStreaming(t *testing.T) {
	t.Run("responses_clears_sticky_expired_while_1016_refresh_keeps_expired", func(t *testing.T) {
		srv := e2eNewServer(t, nil)
		up := clientFromFrames([]gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)})
		a1 := e2eNewAccount("a1", "a1@example.com", up)
		srv.SetAccounts([]*AccountState{a1})
		srv.markAccountFailure(a1, zzReal1016Msg)

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{"model": testModel.ModelName, "input": "hi"}, map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 {
			t.Fatalf("responses failed: %d", rec.Code)
		}
		if st, _ := AccountStatus(a1, srv.now()); st != StatusOK {
			t.Fatalf("expected responses success to clear sticky expired, got %q", st)
		}

		// If AvailableModels fails with 1016, status must remain expired.
		up.modelsFn = func(ctx context.Context, force bool) (*gemini.AvailableModelsResult, error) {
			return nil, transport.NewUpstreamError(zzReal1016Msg, 401, "upstream_auth_error", nil, nil)
		}
		srv.RefreshAccount(context.Background(), a1, true)
		if st, _ := AccountStatus(a1, srv.now()); st != StatusExpired {
			t.Fatalf("expected 1016 AvailableModels failure to set StatusExpired, got %q", st)
		}
	})

	t.Run("multibyte_incremental_thoughts_under_strict_mode", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.RejectDowngrade = true
			c.LiveThoughts = true
		})
		up := clientFromFrames([]gemini.Frame{
			makeFrame("", "🚀", false, testModel.ModelID),
			makeFrame("", "🚀日本語", false, testModel.ModelID),
			makeFrame("done", "🚀日本語é", true, testModel.ModelID),
		})
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})
		rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", map[string]any{"stream": true}), map[string]string{"Content-Type": "application/json"}, "")
		var reasoning strings.Builder
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok || data == "[DONE]" {
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Reasoning string `json:"reasoning_content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				t.Fatal(err)
			}
			for _, choice := range chunk.Choices {
				reasoning.WriteString(choice.Delta.Reasoning)
			}
		}
		if rec.Code != 200 || !utf8.Valid(rec.Body.Bytes()) || reasoning.String() != "🚀日本語é" {
			t.Fatalf("unexpected multibyte thoughts stream: %d %q", rec.Code, reasoning.String())
		}
	})

	t.Run("upstream_transient_respects_max_attempts_1_vs_3", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.MaxAttempts = 1
			c.MaxUpstreamAttempts = 1
		})
		var calls atomic.Int32
		up := &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				if calls.Add(1) < 3 {
					return nil, transport.NewUpstreamError("transient 1013", 503, "upstream_transient", nil, nil)
				}
				_ = onFrame(&gemini.LineParseResult{CID: "c", RID: "r", AnsweredModelID: testModel.ModelID, Frames: []gemini.Frame{makeFrame("ok3", "", true, testModel.ModelID)}})
				return &gemini.StreamResponse{Status: 200}, nil
			},
		}
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})
		rec1 := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Content-Type": "application/json"}, "")
		if rec1.Code != 503 {
			t.Fatalf("expected 503 with MaxAttempts=1, got %d", rec1.Code)
		}

		calls.Store(0)
		srv3 := e2eNewServer(t, func(c *config.Config) {
			c.MaxAttempts = 3
			c.MaxUpstreamAttempts = 3
		})
		srv3.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})
		rec3 := e2eServeRequest(srv3, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Content-Type": "application/json"}, "")
		if rec3.Code != 200 || calls.Load() != 3 {
			t.Fatalf("expected 200 on attempt 3 with MaxAttempts=3, got code=%d calls=%d", rec3.Code, calls.Load())
		}
	})

	t.Run("responses_cancel_at_early_sse_grace_clears_active_streams", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.EarlySSEGrace = 15 * time.Millisecond
		})
		up := &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
		a1 := e2eNewAccount("a1", "a1@example.com", up)
		srv.SetAccounts([]*AccountState{a1})

		ts := httptest.NewServer(srv)
		defer ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		raw, _ := json.Marshal(map[string]any{"model": testModel.ModelName, "stream": true, "input": "hi"})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/responses", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		resp.Body.Close()
		ts.Close()
		a1.mu.RLock()
		active := a1.activeStreams
		a1.mu.RUnlock()
		if active != 0 {
			t.Fatalf("expected activeStreams=0 after cancel, got %d", active)
		}
	})

	t.Run("secondary_auth_user_and_pj6_activity_before_downgrade_abort", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.RejectDowngrade = true
			c.StreamBufferMode = "verified"
			c.LiveThoughts = true
			c.MaxAttempts = 1
		})
		var sawAppU2, sawStreamAuthUser2 atomic.Bool
		doer := &e2eStubDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				u := req.URL.String()
				if strings.Contains(u, "/app") {
					if strings.Contains(u, "/u/2/app") {
						sawAppU2.Store(true)
					}
					return &fhttp.Response{
						StatusCode: 200, Header: fhttp.Header{},
						Body: io.NopCloser(strings.NewReader(`{"SNlM0e":"at_sub","cfb2h":"bl_sub","FdrFJe":"123","oPEP7c":"sub@example.com"}`)),
					}, nil
				}
				if strings.Contains(u, "StreamGenerate") {
					if req.URL.Query().Get("authuser") == "2" {
						sawStreamAuthUser2.Store(true)
					}
					inner1 := `[null,["c_d","r_d"],null,null,[["rc_d",[""],null,null,null,null,null,null,[1]]],{"7":["Searching",["google_search",["gs","Searching",null]]]}]`
					inner2 := `[null,["c_d","r_d"],null,null,[["rc_d",["downgraded answer"],null,null,null,null,null,null,[2]]],{"40":"` + lowerModelID + `"}]`
					wire := ")]}'\n\n" + `[["wrb.fr",null,` + e2eQuoteJSON(inner1) + `]]` + "\n" + `[["wrb.fr",null,` + e2eQuoteJSON(inner2) + `]]` + "\n"
					return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader(wire))}, nil
				}
				return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader("[]"))}, nil
			},
		}
		pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
		t.Cleanup(pool.Reset)
		ac := gemini.NewAccountClient("sub2", "SAPISID=s1; __Secure-1PSID=p1", "", pool, cookies.NewRotator())
		ac.SetAuthUser(2)
		acct := e2eNewAccount("sub2", "sub@example.com", ac)
		srv.SetAccounts([]*AccountState{acct})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", map[string]any{"stream": true}), map[string]string{"Content-Type": "application/json"}, "")
		if !sawAppU2.Load() || !sawStreamAuthUser2.Load() {
			t.Fatalf("expected secondary account authUser=2 routing (/u/2/app=%v, StreamGenerate authuser=2=%v)", sawAppU2.Load(), sawStreamAuthUser2.Load())
		}
		if strings.Contains(rec.Body.String(), "downgraded answer") {
			t.Fatalf("downgraded text leaked after pj[6] activity: %s", rec.Body.String())
		}
		if rec.Code != 429 && !(rec.Code == 200 && strings.Contains(rec.Body.String(), "model_substitution")) {
			t.Fatalf("expected 429 or 200 with model_substitution SSE error on downgrade, got %d: %s", rec.Code, rec.Body.String())
		}
		if rec.Code == 200 && !strings.Contains(rec.Body.String(), "Searching") {
			t.Fatalf("expected pj[6] Searching activity emitted in reasoning_content before downgrade abort, got: %s", rec.Body.String())
		}
	})
}

func TestE2E_T2_Boundaries_ParityToolbridgeRouting(t *testing.T) {
	t.Run("responses_max_output_tokens_1_and_empty_stop_and_json_schema", func(t *testing.T) {
		srv := e2eNewServer(t, nil)
		up := clientFromFrames([]gemini.Frame{
			makeFrame("```json\n{\"msg\":\"hello {world}\"}\n```", "", true, testModel.ModelID),
		})
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
			"model": testModel.ModelName,
			"input": "hi",
			"stop":  []any{},
			"text":  map[string]any{"format": map[string]any{"type": "json_schema", "name": "out", "schema": map[string]any{"type": "object"}}},
		}, map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "```json") || !strings.Contains(rec.Body.String(), `{world}`) {
			t.Fatalf("unexpected json_schema response: %d %s", rec.Code, rec.Body.String())
		}

		// Verify max_output_tokens=1 truncates a multi-token response.
		srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", clientFromFrames([]gemini.Frame{
			makeFrame("alpha beta gamma delta epsilon", "", true, testModel.ModelID),
		}))})
		recTrunc := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
			"model":             testModel.ModelName,
			"input":             "hi",
			"max_output_tokens": 1,
			"stop":              []any{},
		}, map[string]string{"Content-Type": "application/json"}, "")
		if recTrunc.Code != 200 || strings.Contains(recTrunc.Body.String(), "epsilon") || strings.Contains(recTrunc.Body.String(), "delta") {
			t.Fatalf("expected max_output_tokens=1 to truncate multi-token response, got %d: %s", recTrunc.Code, recTrunc.Body.String())
		}
	})

	t.Run("compact_schema_map_cjk_description_and_pattern_properties", func(t *testing.T) {
		in := map[string]any{
			"type": "object",
			"patternProperties": map[string]any{
				"^x-": map[string]any{
					"properties": map[string]any{"title": map[string]any{"type": "string"}},
				},
			},
			"description": strings.Repeat("日本語", 40),
		}
		out := toolbridge.CompactSchemaMap(in, 20)
		if !utf8.ValidString(out["description"].(string)) {
			t.Fatal("CompactSchemaMap truncated CJK description mid-rune")
		}
		pp := out["patternProperties"].(map[string]any)["^x-"].(map[string]any)["properties"].(map[string]any)
		if _, ok := pp["title"]; !ok {
			t.Fatal("CompactSchemaMap stripped 'title' inside patternProperties.*.properties")
		}
	})

	t.Run("lite_model_bypasses_5h_exhausted_unless_disabled", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.QuotaPreflight = "strict"
		})
		liteModel := gemini.ModelInfo{ModelName: "gemini-flash-lite", ModelID: "eeeeeeeeeeeeeeee", Capacity: 1}
		a1 := e2eNewAccount("a1", "a1@example.com", clientFromFrames([]gemini.Frame{makeFrame("lite-ok", "", true, liteModel.ModelID)}), testModel, liteModel)
		a1.Quota = []gemini.QuotaBucket{{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 0, ResetAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}
		srv.SetAccounts([]*AccountState{a1})

		recLite := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("gemini-flash-lite", nil), map[string]string{"Content-Type": "application/json"}, "")
		if recLite.Code != 200 {
			t.Fatalf("expected gemini-flash-lite to succeed when 5h is 0, got %d: %s", recLite.Code, recLite.Body.String())
		}
		a1.mu.Lock()
		a1.Disabled = true
		a1.mu.Unlock()
		recDisabled := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("gemini-flash-lite", nil), map[string]string{"Content-Type": "application/json"}, "")
		if recDisabled.Code == 200 {
			t.Fatal("disabled account must NOT serve gemini-flash-lite")
		}
	})

	t.Run("streamer_interleaved_generate_media_and_documented_model_keys", func(t *testing.T) {
		var indices []int
		tools := toolbridge.AugmentToolsWithMediaGenerator([]any{
			map[string]any{"type": "function", "function": map[string]any{"name": "toolA", "parameters": map[string]any{"type": "object"}}},
			map[string]any{"type": "function", "function": map[string]any{"name": "toolB", "parameters": map[string]any{"type": "object"}}},
		}, "auto")
		s := toolbridge.NewLiveToolCallStreamer(tools, "auto", func(delta map[string]any) {
			for _, tc := range delta["tool_calls"].([]map[string]any) {
				indices = append(indices, tc["index"].(int))
			}
		})
		if err := s.PushError(`{"tool_calls":[{"name":"generate_media","arguments":{"media_type":"image","prompt":"a"}},{"name":"toolA","arguments":{}},{"name":"generate_media","arguments":{"media_type":"image","prompt":"b"}},{"name":"toolB","arguments":{}}]}`); err != nil {
			t.Fatalf("PushError failed: %v", err)
		}
		if len(indices) != 4 || indices[0] != 0 || indices[1] != 0 || indices[2] != 1 || indices[3] != 1 {
			t.Fatalf("expected client indices [0, 0, 1, 1] (header+args for toolA=0, toolB=1) after filtering two generate_media calls, got %v", indices)
		}

		keys := gemini.DocumentedModelKeys(gemini.ModelInfo{ModelName: "GEMINI-4-CARBON", DisplayName: "4 Carbon", Description: "gemini-4-carbon"})
		if len(keys) != 1 || keys[0] != "gemini-4-carbon" {
			t.Fatalf("expected single deduplicated key 'gemini-4-carbon', got %v", keys)
		}
	})

	t.Run("classifyEmpty_past_reset_returns_502", func(t *testing.T) {
		srv := e2eNewServer(t, nil)
		a1 := e2eNewAccount("a1", "a1@example.com", &mockUpstream{})
		a1.Quota = []gemini.QuotaBucket{{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 0, ResetAt: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)}}
		srv.SetAccounts([]*AccountState{a1})
		err := srv.classifyEmpty(context.Background(), a1, "gemini-pro")
		ue, ok := err.(*transport.UpstreamError)
		if !ok || ue.StatusCode != 502 {
			t.Fatalf("expected 502 empty_upstream_response when ResetAt is in the past, got %v", err)
		}
		if hdrs := srv.lookupQuotaResetHeaders(context.Background(), a1, "gemini-pro"); hdrs != nil {
			t.Fatalf("expected nil reset headers for expired ResetAt in past, got %+v", hdrs)
		}
	})
}

func TestE2E_T2_Boundaries_Operational(t *testing.T) {
	t.Run("deriveAccountIDFromEmail_long_special_and_collisions", func(t *testing.T) {
		existing := []*AccountState{{ID: "alice"}, {ID: "alice-2"}}
		got := deriveAccountIDFromEmail("alice@other.com", existing)
		if got == "alice" || got == "alice-2" {
			t.Fatalf("expected unique derived id avoiding alice & alice-2, got %q", got)
		}
		longEmail := strings.Repeat("a", 60) + "+tag@example.com"
		gotLong := deriveAccountIDFromEmail(longEmail, nil)
		if len(gotLong) > 64 || !reSafeID.MatchString(gotLong) {
			t.Fatalf("invalid derived long id %q", gotLong)
		}
	})

	t.Run("SaveEnvFile_newline_sanitization_and_permissions", func(t *testing.T) {
		envPath := filepath.Join(t.TempDir(), "config.env")
		if err := config.SaveEnvFile(envPath, map[string]string{
			"BRIDGE_ACCOUNT_STRATEGY": "lru\nBRIDGE_API_KEY=evil",
		}); err != nil {
			t.Fatalf("SaveEnvFile failed: %v", err)
		}
		fi, err := os.Stat(envPath)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("expected 0600 permissions on config.env, err=%v mode=%v", err, fi)
		}
		m := config.LoadEnvFile(envPath)
		if m["BRIDGE_API_KEY"] != "" {
			t.Fatalf("newline injection created extra env key: %+v", m)
		}
	})

	t.Run("failover_enforces_max_rpm_on_fallback_account", func(t *testing.T) {
		srv := e2eNewServer(t, func(c *config.Config) {
			c.MaxAttempts = 3
			c.MaxRPMPerAccount = 1
		})
		a1 := e2eNewAccount("a1", "a1@example.com", &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				return nil, transport.NewUpstreamError("transient", 503, "upstream_transient", nil, nil)
			},
		})
		a2 := e2eNewAccount("a2", "a2@example.com", clientFromFrames([]gemini.Frame{makeFrame("a2", "", true, testModel.ModelID)}))
		a2.reqTimestamps = []time.Time{time.Now()} // a2 already at MaxRPMPerAccount=1!
		srv.SetAccounts([]*AccountState{a1, a2})

		rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Content-Type": "application/json"}, "")
		if rec.Header().Get("X-GSB-Account") == "a2" && rec.Code == 200 {
			t.Fatalf("failover bypassed MaxRPMPerAccount=1 on fallback account a2")
		}
	})

	t.Run("media_endpoint_tracks_active_streams_and_quarantines_on_account_banned", func(t *testing.T) {
		srv := e2eNewServer(t, nil)
		started := make(chan struct{})
		release := make(chan struct{})
		var quotaCalls atomic.Int32
		up := &e2eMediaUpstream{
			mockUpstream: &mockUpstream{
				streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
					close(started)
					<-release
					_ = onFrame(&gemini.LineParseResult{
						CID: "cm1", RID: "rm1", AnsweredModelID: testModel.ModelID,
						Frames: []gemini.Frame{{
							RCID: "rcm1", Done: true, AnsweredModelID: testModel.ModelID,
							GeneratedImages: []gemini.GeneratedImage{{URL: "https://lh3.googleusercontent.com/gg-dl/op=s2048"}},
						}},
					})
					return &gemini.StreamResponse{Status: 200, CID: "cm1", RID: "rm1"}, nil
				},
				quotaFn: func(ctx context.Context) []gemini.QuotaBucket {
					quotaCalls.Add(1)
					return []gemini.QuotaBucket{{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 2398}}
				},
			},
		}
		a1 := e2eNewAccount("a1", "a1@example.com", up)
		a1.QuotaAt = time.Now().Add(-2 * time.Minute)
		srv.SetAccounts([]*AccountState{a1})

		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done <- e2eServeRequest(srv, http.MethodPost, "/v1/images/generations", map[string]any{"prompt": "sky"}, map[string]string{"Content-Type": "application/json"}, "")
		}()
		<-started
		a1.mu.RLock()
		activeInFlight := a1.activeStreams
		a1.mu.RUnlock()
		close(release)
		recOK := <-done
		if recOK.Code != 200 || activeInFlight < 1 {
			t.Fatalf("expected /v1/images/generations activeStreams>=1 in-flight and 200, got active=%d code=%d", activeInFlight, recOK.Code)
		}
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			a1.mu.RLock()
			refreshing := a1.quotaRefreshing
			a1.mu.RUnlock()
			if quotaCalls.Load() > 0 && !refreshing {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		a1.mu.RLock()
		stillRefreshing := a1.quotaRefreshing
		a1.mu.RUnlock()
		if quotaCalls.Load() == 0 || stillRefreshing {
			t.Fatalf("expected /v1/images/generations success to complete quota refresh, calls=%d refreshing=%v", quotaCalls.Load(), stillRefreshing)
		}

		up.streamFn = func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			return nil, transport.NewUpstreamError("TOS_VIOLATION", 403, "account_banned", nil, nil)
		}
		recBan := e2eServeRequest(srv, http.MethodPost, "/v1/videos/generations", map[string]any{"prompt": "wave"}, map[string]string{"Content-Type": "application/json"}, "")
		if recBan.Code != 403 {
			t.Fatalf("expected 403 on banned video generation, got %d", recBan.Code)
		}
		a1.mu.RLock()
		quarantined := a1.Quarantined
		activeAfter := a1.activeStreams
		a1.mu.RUnlock()
		if !quarantined || activeAfter != 0 {
			t.Fatalf("expected account quarantined=true and activeStreams=0 after failure, got q=%v active=%d", quarantined, activeAfter)
		}
	})

	t.Run("models_singleflight_bg_refresh_cold_fill_and_disabled_reimport_safety", func(t *testing.T) {
		srv, cfg := e2eNewServerWithCfg(t, func(c *config.Config) {
			c.WebUI = true
			c.ModelRefreshEvery = 5 * time.Minute
		})
		if err := os.MkdirAll(cfg.AccountsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(cfg.AccountsDir, "paused1.json")
		if _, err := cookies.SaveCredentialFile(p, cookies.CredentialFile{
			Kind: "gemini-web", Name: "paused1", Email: "paused1@example.com",
			Cookies: "SAPISID=s1; __Secure-1PSID=p1", Disabled: true, CapturedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			t.Fatal(err)
		}
		var modelCalls atomic.Int32
		started := make(chan struct{})
		var startedOnce sync.Once
		gate := make(chan struct{})
		up := &mockUpstream{
			modelsFn: func(ctx context.Context, force bool) (*gemini.AvailableModelsResult, error) {
				modelCalls.Add(1)
				startedOnce.Do(func() { close(started) })
				<-gate
				return &gemini.AvailableModelsResult{Status: 200, Models: []gemini.ModelInfo{testModel}, Capacity: 2}, nil
			},
			bootFn: func(ctx context.Context) error {
				return transport.NewUpstreamError("bad cookie", 401, "upstream_auth_error", nil, nil)
			},
		}
		// Ensure POST /v1/admin/accounts/import uses mock upstream and never touches external network.
		srv.clientFactory = func(a *AccountState) UpstreamClient { return up }

		// 1. Stale warm cache: server.go singleflights background refresh via srv.modelsRefreshing.
		acct := e2eNewAccount("paused1", "paused1@example.com", up)
		acct.Disabled = true
		acct.FilePath = p
		acct.ModelsAt = time.Now().Add(-10 * time.Minute)
		srv.SetAccounts([]*AccountState{acct})

		errCh := make(chan string, 8)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := e2eServeRequest(srv, http.MethodGet, "/v1/models", nil, nil, "")
				if rec.Code != http.StatusOK {
					errCh <- fmt.Sprintf("GET /v1/models unexpected status %d: %s", rec.Code, rec.Body.String())
				}
			}()
		}
		wg.Wait()
		close(errCh)
		for msg := range errCh {
			t.Fatal(msg)
		}
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for background /v1/models singleflight refresh")
		}
		close(gate)
		deadline := time.Now().Add(2 * time.Second)
		for srv.modelsRefreshing.Load() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if srv.modelsRefreshing.Load() {
			t.Fatal("background /v1/models refresh did not complete")
		}
		if got := modelCalls.Load(); got != 1 {
			t.Fatalf("expected /v1/models singleflight == 1 call, got %d", got)
		}

		// 2. Empty cold cache (!hasWarmModels): synchronous EnsureModels populates models cleanly.
		acct.mu.Lock()
		acct.Models = nil
		acct.ModelsAt = time.Time{}
		acct.mu.Unlock()
		modelCalls.Store(0)
		recCold := e2eServeRequest(srv, http.MethodGet, "/v1/models", nil, nil, "")
		if recCold.Code != http.StatusOK || !strings.Contains(recCold.Body.String(), testModel.ModelName) {
			t.Fatalf("cold /v1/models failed to populate models: code=%d body=%s", recCold.Code, recCold.Body.String())
		}

		// 3. Failed re-import on disabled account uses mock bootFn (no live network) and preserves paused1.json.
		recFail := e2eServeRequest(srv, http.MethodPost, "/v1/admin/accounts/import", map[string]any{
			"id":     "paused1",
			"cookie": "SAPISID=bad; __Secure-1PSID=bad",
		}, map[string]string{"Content-Type": "application/json"}, "")
		if recFail.Code != 401 {
			t.Fatalf("expected failed probe import to return 401 from mock bootFn, got %d: %s", recFail.Code, recFail.Body.String())
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("failed probe deleted paused1.json: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// TIER 3 — Cross-Feature Pairwise Combinations
// ---------------------------------------------------------------------------

func TestE2E_T3_SecurityPlusConcurrency_AdminPatchAndMediaTTL(t *testing.T) {
	now := time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC)
	var curNow atomic.Value
	curNow.Store(now)

	srv := e2eNewServer(t, func(c *config.Config) {
		c.WebUI = true
		c.APIKey = "public"
	})
	srv.clock = func() time.Time { return curNow.Load().(time.Time) }
	up := &e2eMediaUpstream{mockUpstream: clientFromFrames(nil)}
	srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

	_, entry := srv.registerMedia(context.Background(), "a1", "https://lh3.googleusercontent.com/gg-dl/t3=s2048", "", "image/png", "png", "T3")
	var wg sync.WaitGroup
	errCh := make(chan string, 32)
	reportErr := func(msg string) {
		select {
		case errCh <- msg:
		default:
		}
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			recPatch := e2eServeRequest(srv, http.MethodPatch, "/v1/admin/config", map[string]any{"maxRpmPerAccount": 10 + idx}, map[string]string{"Content-Type": "application/json"}, "127.0.0.1:4000")
			if recPatch.Code != http.StatusOK {
				reportErr(fmt.Sprintf("PATCH /v1/admin/config unexpected status %d: %s", recPatch.Code, recPatch.Body.String()))
				return
			}
			recSpoof := e2eServeRequest(srv, http.MethodGet, "/v1/admin/config", nil, map[string]string{"X-Forwarded-For": "127.0.0.1, 203.0.113.1"}, "127.0.0.1:4000")
			if recSpoof.Code != 403 {
				reportErr(fmt.Sprintf("expected 403 on multi-hop XFF during concurrency, got %d", recSpoof.Code))
				return
			}
			recMedia := e2eServeRequest(srv, http.MethodGet, "/v1/media/"+entry.ID, nil, nil, "")
			if recMedia.Code != 200 || recMedia.Header().Get("X-Content-Type-Options") != "nosniff" {
				reportErr(fmt.Sprintf("unexpected /v1/media response: code=%d nosniff=%q", recMedia.Code, recMedia.Header().Get("X-Content-Type-Options")))
				return
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}
	curNow.Store(now.Add(25 * time.Hour))
	if recExp := e2eServeRequest(srv, http.MethodGet, "/v1/media/"+entry.ID, nil, nil, ""); recExp.Code != 404 {
		t.Fatalf("expected 404 after TTL, got %d", recExp.Code)
	}
}

func TestE2E_T3_StrictDowngradePlusLiveThoughtsPlusResponsesRetry(t *testing.T) {
	srv := e2eNewServer(t, func(c *config.Config) {
		c.RejectDowngrade = true
		c.StreamBufferMode = "verified"
		c.EarlySSEGrace = 20 * time.Millisecond
		c.LiveThoughts = true
		c.MaxAttempts = 3
		c.MaxRPMPerAccount = 2
	})
	m2 := gemini.ModelInfo{ModelName: "gemini-4-hydrogen", ModelID: "dddddddddddddddd", Capacity: 2, Aliases: []string{"gemini-pro"}}
	up1 := &mockUpstream{
		streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			time.Sleep(40 * time.Millisecond) // allow EarlySSEGrace to flush
			return nil, transport.NewUpstreamError("transient 1013", 503, "upstream_transient", nil, nil)
		},
	}
	up2 := &mockUpstream{
		streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			_ = onFrame(&gemini.LineParseResult{CID: "c2", RID: "r2", AnsweredModelID: m2.ModelID, Frames: []gemini.Frame{makeFrame("", "Reasoning live...", false, m2.ModelID)}})
			_ = onFrame(&gemini.LineParseResult{CID: "c2", RID: "r2", AnsweredModelID: m2.ModelID, Frames: []gemini.Frame{makeFrame("Done.<prohibit_tool_calling>0 tool calls remaining</prohibit_tool_calling>", "Reasoning live...", true, m2.ModelID)}})
			return &gemini.StreamResponse{Status: 200, CID: "c2", RID: "r2"}, nil
		},
	}
	srv.SetAccounts([]*AccountState{
		e2eNewAccount("a1", "a1@example.com", up1, testModel),
		e2eNewAccount("a2", "a2@example.com", up2, m2),
	})

	ts := httptest.NewServer(srv)
	defer ts.Close()
	raw, _ := json.Marshal(map[string]any{"model": "gemini-pro", "stream": true, "input": "go"})
	resp, err := http.Post(ts.URL+"/v1/responses", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)
	if resp.StatusCode != 200 || !strings.Contains(bodyStr, "Reasoning live...") || !strings.Contains(bodyStr, "response.completed") {
		t.Fatalf("T3 responses retry + grace + live thoughts failed: code=%d body=%s", resp.StatusCode, bodyStr)
	}
	if strings.Contains(bodyStr, "prohibit_tool_calling") {
		t.Fatalf("StripToolGuard missed guard in /v1/responses: %s", bodyStr)
	}
}

func TestE2E_T3_CompactSchemaPlusLiveToolCallsPlusMediaInterception(t *testing.T) {
	srv := e2eNewServer(t, func(c *config.Config) {
		c.LiveToolCalls = true
		c.RejectDowngrade = true
		c.ToolSchemaCompact = true
	})
	var round1Prompt string
	var calls atomic.Int32
	up := &e2eMediaUpstream{
		mockUpstream: &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				if calls.Add(1) == 1 {
					round1Prompt = p
					payload := "```json\n{\"tool_calls\":[{\"name\":\"generate_media\",\"arguments\":{\"media_type\":\"image\",\"prompt\":\"rocket\"}},{\"name\":\"create_issue\",\"arguments\":{\"title\":\"🚀\",\"examples\":\"日本語\"}}]}\n```"
					_ = onFrame(&gemini.LineParseResult{
						CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
						Frames: []gemini.Frame{makeFrame(payload, "", false, testModel.ModelID)},
					})
					_ = onFrame(&gemini.LineParseResult{
						CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
						Frames: []gemini.Frame{makeFrame(payload, "", true, testModel.ModelID)},
					})
					return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
				}
				_ = onFrame(&gemini.LineParseResult{
					CID: "c1", RID: "r2", AnsweredModelID: testModel.ModelID,
					Frames: []gemini.Frame{{
						RCID: "rc1", Done: true, AnsweredModelID: testModel.ModelID,
						GeneratedImages: []gemini.GeneratedImage{{URL: "https://lh3.googleusercontent.com/gg-dl/rocket=s2048", Title: "Rocket"}},
					}},
				})
				return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r2"}, nil
			},
		},
	}
	srv.SetAccounts([]*AccountState{e2eNewAccount("a1", "a1@example.com", up)})

	rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    testModel.ModelName,
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "draw an image of a rocket and file issue"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "create_issue",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title":    map[string]any{"type": "string"},
						"examples": map[string]any{"type": "string"},
					},
					"required": []any{"title", "examples"},
				},
			},
		}},
	}, map[string]string{"Content-Type": "application/json"}, "")
	if rec.Code != 200 || !utf8.Valid(rec.Body.Bytes()) {
		t.Fatalf("failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 upstream calls (tool call + server-side generate_media execution), got %d", calls.Load())
	}
	var propsFound bool
	for _, line := range strings.Split(round1Prompt, "\n") {
		if !strings.Contains(line, `"create_issue"`) || !strings.HasPrefix(strings.TrimSpace(line), "{") {
			continue
		}
		var def map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &def); err == nil {
			if params, ok := def["parameters"].(map[string]any); ok {
				if props, ok := params["properties"].(map[string]any); ok {
					_, hasTitle := props["title"]
					_, hasExamples := props["examples"]
					if hasTitle && hasExamples {
						propsFound = true
					}
				}
			}
		}
	}
	if !propsFound {
		t.Fatalf("round 1 prompt lost 'title'/'examples' inside create_issue parameters.properties: %s", round1Prompt)
	}
	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, `"create_issue"`) || !strings.Contains(bodyStr, `"index":0`) {
		t.Fatalf("expected create_issue streamed at client index 0, got: %s", bodyStr)
	}
	if strings.Contains(bodyStr, `"index":1`) {
		t.Fatalf("unexpected client index 1 in SSE output (LiveToolCallStreamer index-shift duplicate): %s", bodyStr)
	}
}

func TestE2E_T3_RoutingDisabledPlusGroupKeyPlusMCPAndMediaFailover(t *testing.T) {
	srv := e2eNewServer(t, func(c *config.Config) {
		c.APIKey = "pool-secret"
		c.GroupTokens = map[string]string{"team-a-key": "team-a"}
		c.MaxRPMPerAccount = 10
	})
	var a1Calls, a2Calls, a3Calls atomic.Int32
	up1 := &e2eMediaUpstream{
		mockUpstream: &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				a1Calls.Add(1)
				_ = onFrame(&gemini.LineParseResult{
					CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
					Frames: []gemini.Frame{{
						RCID: "rc1", Text: "img-a1", Done: true, AnsweredModelID: testModel.ModelID,
						GeneratedImages: []gemini.GeneratedImage{{URL: "https://lh3.googleusercontent.com/gg-dl/a1=s2048"}},
					}},
				})
				return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
			},
		},
	}
	var failA2Media atomic.Bool
	up2 := &e2eMediaUpstream{
		mockUpstream: &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				a2Calls.Add(1)
				if failA2Media.Load() {
					return nil, transport.NewUpstreamError("transient", 503, "upstream_transient", nil, nil)
				}
				_ = onFrame(&gemini.LineParseResult{
					CID: "c2", RID: "r2", AnsweredModelID: testModel.ModelID,
					Frames: []gemini.Frame{{
						RCID: "rc2", Text: "search-a2", Done: true, AnsweredModelID: testModel.ModelID,
						GeneratedImages: []gemini.GeneratedImage{{URL: "https://lh3.googleusercontent.com/gg-dl/a2=s2048"}},
					}},
				})
				return &gemini.StreamResponse{Status: 200, CID: "c2", RID: "r2"}, nil
			},
		},
	}
	up3 := &e2eMediaUpstream{
		mockUpstream: &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				a3Calls.Add(1)
				_ = onFrame(&gemini.LineParseResult{
					CID: "c3", RID: "r3", AnsweredModelID: testModel.ModelID,
					Frames: []gemini.Frame{makeFrame("b", "", true, testModel.ModelID)},
				})
				return &gemini.StreamResponse{Status: 200, CID: "c3", RID: "r3"}, nil
			},
		},
	}
	a1 := e2eNewAccount("a1", "a1@example.com", up1)
	a1.AccountGroup = "team-a"
	a1.Disabled = true
	a2 := e2eNewAccount("a2", "a2@example.com", up2)
	a2.AccountGroup = "team-a"
	a3 := e2eNewAccount("a3", "a3@example.com", up3)
	a3.AccountGroup = "team-b"
	srv.SetAccounts([]*AccountState{a1, a2, a3})

	recChat := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Authorization": "Bearer team-a-key"}, "")
	if recChat.Code != 200 || recChat.Header().Get("X-GSB-Account") != "a2" {
		t.Fatalf("expected chat routed to a2 (skipping disabled a1 & out-of-group a3), got %d %q", recChat.Code, recChat.Header().Get("X-GSB-Account"))
	}
	recMCP := postMCP(t, srv, "/mcp", "team-a-key", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "web_search", "arguments": map[string]any{"query": "test"}},
	})
	if recMCP.Header().Get("X-GSB-Account") != "a2" {
		t.Fatalf("expected MCP routed to a2, got %q", recMCP.Header().Get("X-GSB-Account"))
	}

	// Media endpoint skips disabled a1 and out-of-group a3, serving via a2.
	recImg1 := e2eServeRequest(srv, http.MethodPost, "/v1/images/generations", map[string]any{"prompt": "beacon"}, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer team-a-key"}, "")
	if recImg1.Code != 200 || a1Calls.Load() != 0 || a3Calls.Load() != 0 {
		t.Fatalf("expected /v1/images/generations to route to a2 (skipping disabled a1 and team-b a3), got code=%d a1=%d a3=%d body=%s", recImg1.Code, a1Calls.Load(), a3Calls.Load(), recImg1.Body.String())
	}

	// Re-enable a1 and fail a2 on media generation: /v1/images/generations must fail over inside team-a to a1, never crossing to team-b a3.
	a1.mu.Lock()
	a1.Disabled = false
	a1.mu.Unlock()
	failA2Media.Store(true)
	recImgFailover := e2eServeRequest(srv, http.MethodPost, "/v1/images/generations", map[string]any{"prompt": "lighthouse"}, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer team-a-key"}, "")
	if recImgFailover.Code != 200 || a1Calls.Load() != 1 || a3Calls.Load() != 0 {
		t.Fatalf("expected /v1/images/generations failover from a2 to team-a a1 without crossing to team-b a3, got code=%d a1=%d a2=%d a3=%d body=%s", recImgFailover.Code, a1Calls.Load(), a2Calls.Load(), a3Calls.Load(), recImgFailover.Body.String())
	}
	imgBody := e2eDecodeJSONMap(t, recImgFailover.Body.Bytes())
	dataArr, _ := imgBody["data"].([]any)
	if len(dataArr) == 0 {
		t.Fatalf("expected non-empty image data after failover: %+v", imgBody)
	}
}

func TestE2E_T3_StickyRecoveryPlusQuota5hPlusFailover(t *testing.T) {
	srv := e2eNewServer(t, func(c *config.Config) {
		c.WebUI = true
		c.AccountStrategy = "fill-first"
		c.AccountPriority = []string{"a1", "a2"}
	})
	a1 := e2eNewAccount("a1", "a1@example.com", clientFromFrames([]gemini.Frame{makeFrame("a1-ok", "", true, testModel.ModelID)}))
	a2 := e2eNewAccount("a2", "a2@example.com", clientFromFrames([]gemini.Frame{makeFrame("a2-ok", "", true, testModel.ModelID)}))
	srv.SetAccounts([]*AccountState{a1, a2})

	// Mark a1 with sticky auth failure + 5h exhausted.
	srv.markAccountFailure(a1, zzReal1016Msg)
	a1.mu.Lock()
	a1.Quota = []gemini.QuotaBucket{{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 0, ResetAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}
	a1.mu.Unlock()

	rec1 := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Content-Type": "application/json"}, "")
	if rec1.Code != 200 || rec1.Header().Get("X-GSB-Account") != "a2" {
		t.Fatalf("expected failover to a2 while a1 expired+exhausted, got %d %q", rec1.Code, rec1.Header().Get("X-GSB-Account"))
	}

	// Probe a1 healthy + reset 5h quota -> fill-first reclaims a1!
	a1.mu.Lock()
	a1.Client = &mockUpstream{
		streamFn: clientFromFrames([]gemini.Frame{makeFrame("a1-reclaimed", "", true, testModel.ModelID)}).streamFn,
		quotaFn: func(ctx context.Context) []gemini.QuotaBucket {
			return []gemini.QuotaBucket{{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 2400}}
		},
	}
	a1.mu.Unlock()
	recProbe := e2eServeRequest(srv, http.MethodPost, "/v1/admin/accounts/a1/probe", nil, nil, "")
	if recProbe.Code != 200 {
		t.Fatalf("probe failed: %d", recProbe.Code)
	}
	rec2 := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Content-Type": "application/json"}, "")
	if rec2.Code != 200 || rec2.Header().Get("X-GSB-Account") != "a1" {
		t.Fatalf("expected fill-first to reclaim recovered a1, got %d %q", rec2.Code, rec2.Header().Get("X-GSB-Account"))
	}
}

func TestE2E_T3_PublicCSRFPlusLocalDownloadCmdPlusMediaCacheHeaders(t *testing.T) {
	pngBytes := e2ePNGBytes(24, 24)
	srv := e2eNewServer(t, func(c *config.Config) {
		c.APIKey = "public"
		c.RejectDowngrade = true
	})
	up := &e2eMediaUpstream{
		mockUpstream: clientFromFrames([]gemini.Frame{{
			RCID: "rc1", Done: true, AnsweredModelID: testModel.ModelID,
			GeneratedImages: []gemini.GeneratedImage{{URL: "https://lh3.googleusercontent.com/gg-dl/t3m=s2048", Title: "Chart"}},
		}}),
		downloadFn: func(ctx context.Context, rawURL, fallbackURL string) ([]byte, string, error) {
			return pngBytes, "image/png", nil
		},
	}
	a1 := e2eNewAccount("a1", "a1@example.com", up)
	srv.SetAccounts([]*AccountState{a1})

	// Cross-origin request under APIKey=public must be rejected with 403.
	recCSRF := e2eServeRequest(srv, http.MethodPost, "/v1/images/generations", map[string]any{"prompt": "chart"}, map[string]string{
		"Origin":       "https://evil.example",
		"Content-Type": "application/json",
	}, "")
	if recCSRF.Code != 403 {
		t.Fatalf("expected 403 CSRF rejection under APIKey=public, got %d", recCSRF.Code)
	}

	// Legitimate image generation with untrusted X-Forwarded-Host must ignore XFF in media URL.
	recImg := e2eServeRequest(srv, http.MethodPost, "/v1/images/generations", map[string]any{"prompt": "chart"}, map[string]string{
		"Host":              "127.0.0.1:20133",
		"X-Forwarded-Host":  "evil.attacker/$(id)",
		"X-Forwarded-Proto": "https",
		"Content-Type":      "application/json",
	}, "198.51.100.20:9999")
	if recImg.Code != 200 {
		t.Fatalf("expected 200 on image generation, got %d: %s", recImg.Code, recImg.Body.String())
	}
	mediaURL := e2eDecodeJSONMap(t, recImg.Body.Bytes())["data"].([]any)[0].(map[string]any)["url"].(string)
	if !strings.HasPrefix(mediaURL, "http://127.0.0.1:20133/v1/media/") {
		t.Fatalf("expected untrusted XFF ignored in mediaURL, got %q", mediaURL)
	}

	// GET /v1/media/<id> must return nosniff + sandbox CSP.
	mediaPath := mediaURL[strings.Index(mediaURL, "/v1/media/"):]
	recGet := e2eServeRequest(srv, http.MethodGet, mediaPath, nil, nil, "")
	if recGet.Code != 200 || recGet.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(recGet.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("missing security headers on /v1/media: code=%d headers=%v", recGet.Code, recGet.Header())
	}

	// Server-side media tool call with quote/newline injection produces safe POSIX single-quoted command.
	ctx := withRequestBaseURL(context.Background(), httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20133/v1/chat/completions", nil), 20133)
	add, _, err := srv.executeServerSideMediaCalls(ctx, []*AccountState{a1}, AuthScope{OK: true, AccountID: "a1"}, a1, &testModel, []toolbridge.Call{{
		Name:      toolbridge.BuiltinMediaToolName,
		Arguments: map[string]any{"media_type": "image", "prompt": "chart", "output_path": "out'$(id)\n.png"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	outObj := e2eExtractMediaToolResultJSON(t, add)
	e2eAssertPOSIXSingleQuotedDownloadCmd(t, outObj, `out'\''$(id)`)
}

func TestE2E_T3_ResponsesLimitsPlusToolGuardPlusUTF8AndQuota5h(t *testing.T) {
	now := time.Date(2026, 11, 21, 16, 0, 0, 0, time.UTC)
	srv := e2eNewServer(t, func(c *config.Config) {
		c.RejectDowngrade = true
		c.MaxAttempts = 1
	})
	srv.clock = func() time.Time { return now }

	// 1. /v1/responses stop + StripToolGuard + UTF-8 multi-byte safety.
	up := clientFromFrames([]gemini.Frame{
		makeFrame("🚀日本語 prefix STOP 🚀tail<prohibit_tool_calling>0 tool calls remaining</prohibit_tool_calling>", "", true, testModel.ModelID),
	})
	a1 := e2eNewAccount("a1", "a1@example.com", up)
	srv.SetAccounts([]*AccountState{a1})

	rec := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
		"model": testModel.ModelName,
		"input": "hi",
		"stop":  []string{"STOP"},
	}, map[string]string{"Content-Type": "application/json"}, "")
	if rec.Code != 200 || !utf8.Valid(rec.Body.Bytes()) {
		t.Fatalf("expected valid UTF-8 200 response, got %d: %s", rec.Code, rec.Body.String())
	}
	payload := e2eDecodeJSONMap(t, rec.Body.Bytes())
	item := payload["output"].([]any)[0].(map[string]any)
	text := item["content"].([]any)[0].(map[string]any)["text"]
	if text != "🚀日本語 prefix" {
		t.Fatalf("expected only the UTF-8 prefix before stop, got %q", text)
	}

	// 2. Empty upstream response on exhausted 5h bucket classifies as 429 rate_limit_error with reset headers.
	resetAt := now.Add(90 * time.Minute).UTC().Format(time.RFC3339)
	a1.mu.Lock()
	a1.Quota = []gemini.QuotaBucket{{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 0, ResetAt: resetAt}}
	a1.Client = clientFromFrames([]gemini.Frame{makeFrame("", "", true, testModel.ModelID)})
	a1.mu.Unlock()
	recEmpty := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
		"model": testModel.ModelName,
		"input": "hi",
	}, map[string]string{"Content-Type": "application/json"}, "")
	if recEmpty.Code != 429 || recEmpty.Header().Get("x-resets-at") != resetAt {
		t.Fatalf("expected 429 with x-resets-at=%q on exhausted 5h empty reply, got code=%d headers=%v body=%s", resetAt, recEmpty.Code, recEmpty.Header(), recEmpty.Body.String())
	}
}

// ---------------------------------------------------------------------------
// TIER 4 — Real-World Application Scenarios
// ---------------------------------------------------------------------------

func TestE2E_T4_Scenario_CodingAgentMultiTurnSession(t *testing.T) {
	srv, cfg := e2eNewServerWithCfg(t, func(c *config.Config) {
		c.APIKey = "agent-secret-key"
		c.RejectDowngrade = true
		c.StreamBufferMode = "verified"
		c.EarlySSEGrace = 15 * time.Millisecond
		c.LiveThoughts = true
		c.SessionCache = true
		c.ToolSchemaCompact = true
	})
	pngBytes := e2ePNGBytes(32, 18)
	var turn atomic.Int32
	var seenMetaOnRestart []any
	up := &e2eMediaUpstream{
		mockUpstream: &mockUpstream{
			streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
				n := turn.Add(1)
				switch n {
				case 1:
					_ = onFrame(&gemini.LineParseResult{CID: "c_agent", RID: "r_1", AnsweredModelID: testModel.ModelID, Frames: []gemini.Frame{makeFrame("", "Inspecting repo...", false, testModel.ModelID)}})
					_ = onFrame(&gemini.LineParseResult{CID: "c_agent", RID: "r_1", AnsweredModelID: testModel.ModelID, Frames: []gemini.Frame{makeFrame("```json\n{\"tool_calls\":[{\"name\":\"create_doc\",\"arguments\":{\"title\":\"Design\",\"examples\":\"ex1\"}}]}\n```", "Inspecting repo...", true, testModel.ModelID)}})
					return &gemini.StreamResponse{Status: 200, CID: "c_agent", RID: "r_1"}, nil
				case 2:
					_ = onFrame(&gemini.LineParseResult{
						CID: "c_agent", RID: "r_2", AnsweredModelID: testModel.ModelID,
						GroundingSources: []gemini.GroundingSource{{URL: "https://go.dev", Title: "Go"}},
						Frames:           []gemini.Frame{makeFrame("Go search summary", "", true, testModel.ModelID)},
					})
					return &gemini.StreamResponse{Status: 200, CID: "c_agent", RID: "r_2"}, nil
				case 3:
					_ = onFrame(&gemini.LineParseResult{
						CID: "c_agent", RID: "r_3", AnsweredModelID: testModel.ModelID,
						Frames: []gemini.Frame{{
							RCID: "rc3", Text: "Diagram ready", Done: true, AnsweredModelID: testModel.ModelID,
							GeneratedImages: []gemini.GeneratedImage{{URL: "https://lh3.googleusercontent.com/gg-dl/arch=s2048", Title: "Arch"}},
						}},
					})
					return &gemini.StreamResponse{Status: 200, CID: "c_agent", RID: "r_3"}, nil
				default:
					seenMetaOnRestart = opts.Metadata
					_ = onFrame(&gemini.LineParseResult{CID: "c_agent", RID: "r_4", AnsweredModelID: testModel.ModelID, Frames: []gemini.Frame{makeFrame("continued", "", true, testModel.ModelID)}})
					return &gemini.StreamResponse{Status: 200, CID: "c_agent", RID: "r_4"}, nil
				}
			},
			quotaFn: func(ctx context.Context) []gemini.QuotaBucket {
				return []gemini.QuotaBucket{{ID: "5h", Label: "5-hour", Total: 2400, Remaining: 2395}}
			},
		},
		downloadFn: func(ctx context.Context, rawURL, fallbackURL string) ([]byte, string, error) {
			return pngBytes, "image/png", nil
		},
	}
	srv.SetAccounts([]*AccountState{e2eNewAccount("agent1", "agent1@example.com", up)})

	agentTools := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "create_doc",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title":    map[string]any{"type": "string"},
					"examples": map[string]any{"type": "string"},
				},
				"required": []any{"title", "examples"},
			},
		},
	}}

	// Turn 1: tool call with "title"/"examples" + live thoughts
	rec1 := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    testModel.ModelName,
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "write design doc"}},
		"tools":    agentTools,
	}, map[string]string{"Authorization": "Bearer agent-secret-key", "Content-Type": "application/json"}, "")
	if rec1.Code != 200 || !strings.Contains(rec1.Body.String(), "Inspecting repo...") || !strings.Contains(rec1.Body.String(), "create_doc") {
		t.Fatalf("Turn 1 failed: %d %s", rec1.Code, rec1.Body.String())
	}
	var turn1CallID string
	for _, line := range strings.Split(rec1.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		tcs, _ := delta["tool_calls"].([]any)
		if len(tcs) > 0 {
			if id, _ := tcs[0].(map[string]any)["id"].(string); id != "" {
				turn1CallID = id
				break
			}
		}
	}
	if turn1CallID == "" {
		t.Fatalf("Turn 1 SSE did not emit tool call id: %s", rec1.Body.String())
	}

	// Turn 2: MCP web_search
	recMCP := postMCP(t, srv, "/mcp", "agent-secret-key", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "web_search", "arguments": map[string]any{"query": "go docs"}},
	})
	if recMCP.Code != 200 || recMCP.Header().Get("X-GSB-Sources") != "1" {
		t.Fatalf("Turn 2 MCP failed: %d %s", recMCP.Code, recMCP.Body.String())
	}

	// Turn 3: Image generation + GET /v1/media/<id> with nosniff/CSP
	recImg := e2eServeRequest(srv, http.MethodPost, "/v1/images/generations", map[string]any{"prompt": "arch diagram"}, map[string]string{"Authorization": "Bearer agent-secret-key", "Content-Type": "application/json"}, "")
	if recImg.Code != 200 {
		t.Fatalf("Turn 3 image gen failed: %d %s", recImg.Code, recImg.Body.String())
	}
	mediaURL := e2eDecodeJSONMap(t, recImg.Body.Bytes())["data"].([]any)[0].(map[string]any)["url"].(string)
	mediaPath := mediaURL[strings.Index(mediaURL, "/v1/media/"):]
	recMedia := e2eServeRequest(srv, http.MethodGet, mediaPath, nil, nil, "")
	if recMedia.Code != 200 || recMedia.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("GET media failed: %d", recMedia.Code)
	}

	// Turn 4: Shutdown flush + restart server reloads session cache
	srv.FlushSessionCacheNow()
	srv2 := New(cfg)
	t.Cleanup(func() { srv2.FlushSessionCacheNow() })
	srv2.SetAccounts([]*AccountState{e2eNewAccount("agent1", "agent1@example.com", up)})
	rec4 := e2eServeRequest(srv2, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": testModel.ModelName,
		"tools": agentTools,
		"messages": []any{
			map[string]any{"role": "user", "content": "write design doc"},
			map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":   turn1CallID,
					"type": "function",
					"function": map[string]any{
						"name":      "create_doc",
						"arguments": `{"title":"Design","examples":"ex1"}`,
					},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": turn1CallID, "name": "create_doc", "content": "created"},
		},
	}, map[string]string{"Authorization": "Bearer agent-secret-key", "Content-Type": "application/json"}, "")
	if rec4.Code != 200 || len(seenMetaOnRestart) == 0 {
		t.Fatalf("Turn 4 continuation cache after restart failed: code=%d meta=%v body=%s", rec4.Code, seenMetaOnRestart, rec4.Body.String())
	}
}

func TestE2E_T4_Scenario_MultiTenantVPSWithWebControlPlane(t *testing.T) {
	srv := e2eNewServer(t, func(c *config.Config) {
		c.WebUI = true
		c.APIKey = "pool-secret-vps"
		c.AdminKey = "admin-secret-vps"
		c.AccountStrategy = "fill-first"
		c.GroupTokens = map[string]string{"team-a-tok": "team-a", "team-b-tok": "team-b"}
	})
	up := clientFromFrames([]gemini.Frame{makeFrame("vps-ok", "", true, testModel.ModelID)})
	a1 := e2eNewAccount("acct-a1", "a1@example.com", up)
	a1.AccountGroup = "team-a"
	a2 := e2eNewAccount("acct-a2", "a2@example.com", up)
	a2.AccountGroup = "team-a"
	b1 := e2eNewAccount("acct-b1", "b1@example.com", up)
	b1.AccountGroup = "team-b"
	srv.SetAccounts([]*AccountState{a1, a2, b1})

	// Attacker probes: auto-token bypass, XFF spoof, DNS rebinding, CSRF
	if rec := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Authorization": "Bearer gsb-acct-a1"}, ""); rec.Code != 401 {
		t.Fatalf("auto-token must be 401, got %d", rec.Code)
	}
	if rec := e2eServeRequest(srv, http.MethodGet, "/v1/admin/config", nil, map[string]string{"X-Forwarded-For": "127.0.0.1, 203.0.113.5"}, "127.0.0.1:1234"); rec.Code != 403 && rec.Code != 401 {
		t.Fatalf("XFF spoof must be denied, got %d", rec.Code)
	}
	if rec := e2eServeRequest(srv, http.MethodGet, "/v1/admin/config", nil, map[string]string{"Host": "untrusted.example"}, "127.0.0.1:1234"); rec.Code != 403 && rec.Code != 401 {
		t.Fatalf("nonlocal authority must not grant admin access, got %d", rec.Code)
	}

	// Operator pauses acct-a1; team-a drains to acct-a2 across Chat/Responses/MCP.
	recPause := e2eServeRequest(srv, http.MethodPost, "/v1/admin/accounts/acct-a1/toggle", nil, map[string]string{"Authorization": "Bearer admin-secret-vps"}, "")
	if recPause.Code != 200 {
		t.Fatalf("toggle failed: %d", recPause.Code)
	}
	recChat := e2eServeRequest(srv, http.MethodPost, "/v1/chat/completions", e2eValidChatBody("", nil), map[string]string{"Authorization": "Bearer team-a-tok"}, "")
	if recChat.Code != 200 || recChat.Header().Get("X-GSB-Account") != "acct-a2" {
		t.Fatalf("expected team-a chat drained to acct-a2, got %d %q", recChat.Code, recChat.Header().Get("X-GSB-Account"))
	}
	recMCP := postMCP(t, srv, "/mcp", "team-a-tok", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "web_search", "arguments": map[string]any{"query": "vps"}},
	})
	if recMCP.Header().Get("X-GSB-Account") != "acct-a2" {
		t.Fatalf("expected team-a MCP routed to acct-a2, got %q", recMCP.Header().Get("X-GSB-Account"))
	}
}

func TestE2E_T4_Scenario_ResilientReasoningWorkloadUnderTurbulence(t *testing.T) {
	srv := e2eNewServer(t, func(c *config.Config) {
		c.RejectDowngrade = true
		c.StreamBufferMode = "verified"
		c.EarlySSEGrace = 15 * time.Millisecond
		c.LiveThoughts = true
		c.AccountStrategy = "rotate"
		c.MaxAttempts = 4
		c.MaxUpstreamAttempts = 4
	})
	m3 := gemini.ModelInfo{ModelName: "gemini-4-hydrogen", ModelID: "dddddddddddddddd", Capacity: 2, Aliases: []string{"gemini-pro"}}

	up1 := &mockUpstream{
		streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			return nil, transport.NewUpstreamError("transient 1013", 503, "upstream_transient", nil, nil)
		},
	}
	up2 := clientFromFrames([]gemini.Frame{
		makeFrame("downgraded", "", true, lowerModelID),
	})
	up3 := clientFromFrames([]gemini.Frame{
		makeFrame("", "Thinking 🚀日本語", false, m3.ModelID),
		makeFrame("```json\n{\"summary\":\"🚀日本語\"}\n```<prohibit_tool_calling>0 tool calls remaining</prohibit_tool_calling>", "Thinking 🚀日本語", true, m3.ModelID),
	})
	srv.SetAccounts([]*AccountState{
		e2eNewAccount("a1", "a1@example.com", up1, testModel),
		e2eNewAccount("a2", "a2@example.com", up2, testModel),
		e2eNewAccount("a3", "a3@example.com", up3, m3),
	})

	rec := e2eServeRequest(srv, http.MethodPost, "/v1/responses", map[string]any{
		"model":             "gemini-pro",
		"stream":            true,
		"thinking_level":    2,
		"max_output_tokens": 50,
		"text":              map[string]any{"format": map[string]any{"type": "json_object"}},
		"input":             "analyze 🚀日本語",
	}, map[string]string{"Content-Type": "application/json"}, "")
	if rec.Code != 200 {
		t.Fatalf("turbulence workload failed: %d %s", rec.Code, rec.Body.String())
	}
	bodyStr := rec.Body.String()
	if !utf8.Valid(rec.Body.Bytes()) || strings.Contains(bodyStr, "```json") || strings.Contains(bodyStr, "prohibit_tool_calling") || strings.Contains(bodyStr, "downgraded") {
		t.Fatalf("unexpected artifacts in turbulence response: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "response.completed") || rec.Header().Get("X-GSB-Model") != "gemini-4-hydrogen" {
		t.Fatalf("expected completed response on fallback model gemini-4-hydrogen, got headers=%v body=%s", rec.Header(), bodyStr)
	}
}
