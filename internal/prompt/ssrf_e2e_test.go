//go:build e2e

package prompt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

var (
	e2eMinimalPNG    = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R'}
	e2eMinimalJPEG   = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
	e2eMinimalGIF89a = []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00")
	e2eMinimalGIF87a = []byte("GIF87a\x01\x00\x01\x00\x80\x00\x00")
	e2eMinimalWEBP   = []byte("RIFF\x1a\x00\x00\x00WEBPVP8 \x0e\x00\x00\x00")
	e2eMinimalBMP    = []byte{'B', 'M', 0x1e, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x1a, 0x00, 0x00, 0x00}
	e2eMinimalTIFFLE = []byte{'I', 'I', '*', 0x00, 0x08, 0x00, 0x00, 0x00}
	e2eMinimalTIFFBE = []byte{'M', 'M', 0x00, '*', 0x00, 0x00, 0x00, 0x08}
	e2eMinimalAVIF   = []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f', 0x00, 0x00, 0x00, 0x00}
	e2eMinimalHEIC   = []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c', 0x00, 0x00, 0x00, 0x00}
)

func assertSSRFBlocked(t *testing.T, err error, target string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected SSRF block for %s, got nil error", target)
	}
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *transport.UpstreamError for %s, got %T: %v", target, err, err)
	}
	if ue.StatusCode != 400 || ue.Code != "invalid_request_error" {
		t.Fatalf("expected 400/invalid_request_error for %s, got status=%d code=%q msg=%q", target, ue.StatusCode, ue.Code, ue.Message)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected immediate dialer.Control block for %s, got DeadlineExceeded: %v", target, err)
	}
	var sysErr syscall.Errno
	if errors.As(err, &sysErr) {
		t.Fatalf("expected pre-connect dialer.Control rejection for %s, but reached OS syscall %v: %v", target, sysErr, err)
	}
	lower := strings.ToLower(err.Error())
	matched := false
	for _, kw := range []string{"blocked", "private", "reserved", "disallow", "loopback", "cgnat", "nat64", "benchmark"} {
		if strings.Contains(lower, kw) {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("expected SSRF block reason in error for %s, got %q", target, err.Error())
	}
}

func TestE2EFetchRemoteImage_Tier1_ValidMagicBytes(t *testing.T) {
	t.Setenv("BRIDGE_ALLOW_PRIVATE_IMAGE_URLS", "1")

	cases := []struct {
		path        string
		body        []byte
		headerMIME  string
		index       int
		wantMIME    string
		wantFileExt string
		wantIndex   int
	}{
		{"/png", e2eMinimalPNG, "application/octet-stream", 0, "image/png", "png", 1},
		{"/jpeg", e2eMinimalJPEG, "application/octet-stream", 2, "image/jpeg", "jpg", 3},
		{"/gif89a", e2eMinimalGIF89a, "image/gif", -1, "image/gif", "gif", 1},
		{"/gif87a", e2eMinimalGIF87a, "application/octet-stream", 1, "image/gif", "gif", 2},
		{"/webp", e2eMinimalWEBP, "application/octet-stream", 0, "image/webp", "webp", 1},
		{"/bmp", e2eMinimalBMP, "application/octet-stream", 0, "image/bmp", "bmp", 1},
		{"/tiff-le", e2eMinimalTIFFLE, "application/octet-stream", 0, "image/tiff", "tiff", 1},
		{"/tiff-be", e2eMinimalTIFFBE, "image/tiff", 0, "image/tiff", "tiff", 1},
		{"/avif", e2eMinimalAVIF, "application/octet-stream", 0, "image/avif", "avif", 1},
		{"/heic", e2eMinimalHEIC, "image/heic", 4, "image/heic", "heic", 5},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, tc := range cases {
			if r.URL.Path == tc.path {
				w.Header().Set("Content-Type", tc.headerMIME)
				_, _ = w.Write(tc.body)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	for _, tc := range cases {
		t.Run(strings.TrimPrefix(tc.path, "/"), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			att, err := FetchRemoteImage(ctx, srv.URL+tc.path, tc.index)
			if err != nil {
				t.Fatalf("FetchRemoteImage(%s) failed: %v", tc.path, err)
			}
			if att == nil {
				t.Fatal("expected non-nil AttachmentPart")
			}
			if att.ContentType != tc.wantMIME {
				t.Fatalf("got ContentType=%q, want %q", att.ContentType, tc.wantMIME)
			}
			wantName := fmt.Sprintf("image_%d.%s", tc.wantIndex, tc.wantFileExt)
			if att.Filename != wantName {
				t.Fatalf("got Filename=%q, want %q", att.Filename, wantName)
			}
			if !bytes.Equal(att.Data, tc.body) {
				t.Fatalf("downloaded bytes mismatch: got %x, want %x", att.Data, tc.body)
			}
		})
	}
}

func TestE2EFetchRemoteImage_Tier2_SSRFBlockedRanges(t *testing.T) {
	t.Setenv("BRIDGE_ALLOW_PRIVATE_IMAGE_URLS", "")

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(e2eMinimalPNG)
	}))
	defer srv.Close()

	ctxLive, cancelLive := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelLive()
	if _, err := FetchRemoteImage(ctxLive, srv.URL+"/secret.png", 0); err == nil {
		t.Fatal("expected loopback httptest server to be blocked")
	} else {
		assertSSRFBlocked(t, err, srv.URL)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("expected 0 HTTP requests to reach loopback server, got %d", got)
	}

	blockedTargets := []struct {
		name string
		url  string
	}{
		{"cgnat_start", "http://100.64.0.1/img.png"},
		{"cgnat_alibaba_metadata", "http://100.100.100.200/latest/meta-data"},
		{"cgnat_end", "http://100.127.255.254/img.png"},
		{"nat64_1", "http://[64:ff9b::1]/img.png"},
		{"nat64_loopback", "http://[64:ff9b::127.0.0.1]/img.png"},
		{"nat64_imds", "http://[64:ff9b::169.254.169.254]/img.png"},
		{"benchmark_start", "http://198.18.0.1/img.png"},
		{"benchmark_end", "http://198.19.255.254/img.png"},
		{"reserved_start", "http://240.0.0.1/img.png"},
		{"reserved_high", "http://254.0.0.1/img.png"},
		{"loopback_v4", "http://127.0.0.2/img.png"},
		{"loopback_v6", "http://[::1]/img.png"},
		{"v4mapped_loopback", "http://[::ffff:127.0.0.1]/img.png"},
		{"v4mapped_cgnat", "http://[::ffff:100.64.0.1]/img.png"},
		{"rfc1918_10", "http://10.0.0.1/img.png"},
		{"rfc1918_172", "http://172.16.0.1/img.png"},
		{"rfc1918_192", "http://192.168.1.1/img.png"},
		{"ula_fd00", "http://[fd00::1]/img.png"},
		{"linklocal_imds", "http://169.254.169.254/latest/meta-data"},
		{"linklocal_v6", "http://[fe80::1]/img.png"},
		{"unspecified_v4", "http://0.0.0.0/img.png"},
		{"unspecified_v6", "http://[::]/img.png"},
	}

	for _, tc := range blockedTargets {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			att, err := FetchRemoteImage(ctx, tc.url, 0)
			if att != nil {
				t.Fatalf("expected nil attachment for %s, got %+v", tc.url, att)
			}
			assertSSRFBlocked(t, err, tc.url)
		})
	}
}

func TestE2EFetchRemoteImage_Tier2_RejectsNonImageMagicBytes(t *testing.T) {
	t.Setenv("BRIDGE_ALLOW_PRIVATE_IMAGE_URLS", "1")

	cases := []struct {
		name        string
		contentType string
		body        []byte
	}{
		{"html", "text/html", []byte("<html>secret</html>")},
		{"spoofed_png_html", "image/png", []byte("<html>not png</html>")},
		{"json", "application/json", []byte(`{"key":"val"}`)},
		{"spoofed_jpeg_json", "image/jpeg", []byte(`{"key":"val"}`)},
		{"plain_text", "text/plain", []byte("hello world")},
		{"svg_xss", "image/svg+xml", []byte(`<svg><script>alert(1)</script></svg>`)},
		{"truncated_png", "image/png", []byte{0x89, 'P', 'N', 'G'}},
		{"wav_audio_riff", "image/webp", []byte("RIFF\x00\x00\x00\x00WAVEfmt ")},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		for _, tc := range cases {
			if tc.name == key {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write(tc.body)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			att, err := FetchRemoteImage(ctx, srv.URL+"/"+tc.name, 0)
			if err == nil {
				t.Fatalf("expected non-image payload %s (%s) to be rejected, got att=%+v", tc.name, tc.contentType, att)
			}
			if att != nil {
				t.Fatalf("expected nil AttachmentPart for rejected payload %s", tc.name)
			}
			var ue *transport.UpstreamError
			if !errors.As(err, &ue) || ue.StatusCode != 400 || ue.Code != "invalid_request_error" {
				t.Fatalf("expected 400/invalid_request_error for %s, got %+v (%v)", tc.name, ue, err)
			}
		})
	}
}

func TestE2EFetchRemoteImage_Tier2_SchemesRedirectsLimits(t *testing.T) {
	t.Setenv("BRIDGE_ALLOW_PRIVATE_IMAGE_URLS", "1")

	badSchemes := []struct {
		name string
		url  string
	}{
		{"file_scheme", "file:///etc/passwd"},
		{"ftp_scheme", "ftp://example.com/a.png"},
		{"data_scheme", "data:image/png;base64,AA=="},
		{"empty_url", ""},
		{"missing_host", "http:///nohost"},
	}
	for _, tc := range badSchemes {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			att, err := FetchRemoteImage(ctx, tc.url, 0)
			if att != nil || err == nil {
				t.Fatalf("expected error for %s (%q), got att=%+v err=%v", tc.name, tc.url, att, err)
			}
			var ue *transport.UpstreamError
			if !errors.As(err, &ue) || ue.StatusCode != 400 || ue.Code != "invalid_request_error" {
				t.Fatalf("expected 400/invalid_request_error for %s, got %+v (%v)", tc.name, ue, err)
			}
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/empty":
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
		case "/status404":
			w.WriteHeader(http.StatusNotFound)
		case "/status500":
			w.WriteHeader(http.StatusInternalServerError)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/redir-file":
			w.Header().Set("Location", "file:///etc/passwd")
			w.WriteHeader(http.StatusFound)
		case "/oversize":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(e2eMinimalPNG)
			zeros := make([]byte, 64*1024)
			remaining := (20*1024*1024 + 1) - len(e2eMinimalPNG)
			for remaining > 0 {
				n := len(zeros)
				if remaining < n {
					n = remaining
				}
				if _, err := w.Write(zeros[:n]); err != nil {
					return
				}
				remaining -= n
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	httpCases := []string{"/empty", "/status404", "/status500", "/loop", "/redir-file", "/oversize"}
	for _, p := range httpCases {
		t.Run("http_"+strings.TrimPrefix(p, "/"), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			att, err := FetchRemoteImage(ctx, srv.URL+p, 0)
			if att != nil || err == nil {
				t.Fatalf("expected error for %s, got att=%+v err=%v", p, att, err)
			}
			var ue *transport.UpstreamError
			if !errors.As(err, &ue) || ue.StatusCode != 400 || ue.Code != "invalid_request_error" {
				t.Fatalf("expected 400/invalid_request_error for %s, got %+v (%v)", p, ue, err)
			}
		})
	}

	// Verify HTTPS-to-HTTP redirect downgrade behavior with a local TLS server:
	// - Under default SSRF mode (BRIDGE_ALLOW_PRIVATE_IMAGE_URLS=""), the TLS loopback hop is
	//   blocked pre-connect before any TLS handshake or downgrade redirect can occur.
	// - Even when private URLs are explicitly enabled, FetchRemoteImage enforces strict TLS
	//   certificate verification (no InsecureSkipVerify) and never follows a downgrade to the
	//   plain-HTTP target.
	var httpTargetHits atomic.Int64
	httpTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpTargetHits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(e2eMinimalPNG)
	}))
	defer httpTarget.Close()

	var tlsHits atomic.Int64
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tlsHits.Add(1)
		http.Redirect(w, r, httpTarget.URL+"/secret.png", http.StatusFound)
	}))
	defer tlsSrv.Close()

	t.Run("https_to_http_redirect_blocked_by_default_ssrf", func(t *testing.T) {
		t.Setenv("BRIDGE_ALLOW_PRIVATE_IMAGE_URLS", "")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		att, err := FetchRemoteImage(ctx, tlsSrv.URL+"/downgrade.png", 0)
		if att != nil || err == nil {
			t.Fatalf("expected error for https->http loopback server under default SSRF mode, got att=%+v err=%v", att, err)
		}
		assertSSRFBlocked(t, err, tlsSrv.URL+"/downgrade.png")
		if tlsHits.Load() != 0 || httpTargetHits.Load() != 0 {
			t.Fatalf("expected 0 hits on TLS or HTTP downgrade target, got tls=%d http=%d", tlsHits.Load(), httpTargetHits.Load())
		}
	})

	t.Run("https_to_http_redirect_untrusted_tls_rejected_without_downgrade_fetch", func(t *testing.T) {
		t.Setenv("BRIDGE_ALLOW_PRIVATE_IMAGE_URLS", "1")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		att, err := FetchRemoteImage(ctx, tlsSrv.URL+"/downgrade.png", 0)
		if att != nil || err == nil {
			t.Fatalf("expected self-signed TLS server rejection (no InsecureSkipVerify), got att=%+v err=%v", att, err)
		}
		var ue *transport.UpstreamError
		if !errors.As(err, &ue) || ue.StatusCode != 400 || ue.Code != "invalid_request_error" {
			t.Fatalf("expected 400/invalid_request_error, got %+v (%v)", ue, err)
		}
		if httpTargetHits.Load() != 0 {
			t.Fatalf("expected 0 hits on HTTP downgrade target, got %d", httpTargetHits.Load())
		}
	})
}

func TestE2EFetchRemoteImage_Tier4_ConcurrentSSRFAndValidation(t *testing.T) {
	t.Run("concurrent_ssrf_blocks", func(t *testing.T) {
		t.Setenv("BRIDGE_ALLOW_PRIVATE_IMAGE_URLS", "")
		targets := []string{
			"http://100.64.0.1/a.png",
			"http://[64:ff9b::1]/b.png",
			"http://198.18.0.1/c.png",
			"http://240.0.0.1/d.png",
			"http://127.0.0.1/e.png",
			"http://10.0.0.1/f.png",
			"http://169.254.169.254/g.png",
			"http://[::1]/h.png",
		}
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for i := range 16 {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				target := targets[idx%len(targets)]
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer cancel()
				att, err := FetchRemoteImage(ctx, target, idx)
				if err == nil || att != nil {
					errs <- fmt.Errorf("target %s unexpectedly succeeded: att=%+v", target, att)
					return
				}
				var ue *transport.UpstreamError
				if !errors.As(err, &ue) || ue.StatusCode != 400 || ue.Code != "invalid_request_error" {
					errs <- fmt.Errorf("target %s wrong error %+v (%v)", target, ue, err)
					return
				}
				if errors.Is(err, context.DeadlineExceeded) {
					errs <- fmt.Errorf("target %s timed out instead of pre-connect SSRF block: %v", target, err)
					return
				}
				var sysErr syscall.Errno
				if errors.As(err, &sysErr) {
					errs <- fmt.Errorf("target %s reached OS syscall %v instead of dialer.Control block: %v", target, sysErr, err)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
	})

	t.Run("concurrent_magic_validation", func(t *testing.T) {
		t.Setenv("BRIDGE_ALLOW_PRIVATE_IMAGE_URLS", "1")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/png":
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write(e2eMinimalPNG)
			case "/jpeg":
				w.Header().Set("Content-Type", "image/jpeg")
				_, _ = w.Write(e2eMinimalJPEG)
			case "/html":
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write([]byte("<html>spoof</html>"))
			case "/json":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":false}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		paths := []struct {
			path      string
			wantValid bool
			wantMIME  string
		}{
			{"/png", true, "image/png"},
			{"/jpeg", true, "image/jpeg"},
			{"/html", false, ""},
			{"/json", false, ""},
		}

		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for i := range 16 {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				tc := paths[idx%len(paths)]
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				att, err := FetchRemoteImage(ctx, srv.URL+tc.path, idx)
				if tc.wantValid {
					if err != nil || att == nil || att.ContentType != tc.wantMIME {
						errs <- fmt.Errorf("%s expected valid %s, got att=%+v err=%v", tc.path, tc.wantMIME, att, err)
					}
				} else {
					if err == nil || att != nil {
						errs <- fmt.Errorf("%s expected rejection, got att=%+v err=%v", tc.path, att, err)
					}
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
	})
}

func TestE2EBoundOldToolText_Tier2_UTF8SafeSlicing(t *testing.T) {
	samples := []struct {
		name string
		text string
	}{
		{"emoji_4byte", strings.Repeat("🚀🧠🔥✅", 12)},
		{"cjk_3byte", strings.Repeat("思考過程分析問題", 10)},
		{"latin_2byte", strings.Repeat("éàüñ", 15)},
		{"mixed_utf8", "head-abc-" + strings.Repeat("思考🚀é", 14) + "-tail-xyz"},
	}

	for _, sample := range samples {
		t.Run(sample.name, func(t *testing.T) {
			for capVal := 8; capVal <= 45; capVal++ {
				out := BoundOldToolText(sample.text, capVal, "tool output")
				if !utf8.ValidString(out) {
					t.Fatalf("BoundOldToolText(%s, cap=%d) produced invalid UTF-8: %q", sample.name, capVal, out)
				}
				if strings.ContainsRune(out, utf8.RuneError) {
					t.Fatalf("BoundOldToolText(%s, cap=%d) produced replacement rune U+FFFD: %q", sample.name, capVal, out)
				}
				if len(sample.text) > capVal && !strings.Contains(out, "...[truncated") {
					t.Fatalf("expected truncation marker for cap=%d, got %q", capVal, out)
				}
			}
			for _, edgeCap := range []int{0, -1, len(sample.text), len(sample.text) + 10} {
				out := BoundOldToolText(sample.text, edgeCap, "")
				if out != sample.text {
					t.Fatalf("BoundOldToolText(%s, cap=%d) expected unchanged text, got %q", sample.name, edgeCap, out)
				}
			}
		})
	}

	// Cross-feature check via FlattenMessages with MaxOldToolChars=33 over multi-turn CJK/emoji tool outputs.
	messages := []map[string]any{
		{"role": "user", "content": "Analyze logs"},
		{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{
				map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "read_log",
						"arguments": `{"path":"app.log"}`,
					},
				},
			},
		},
		{
			"role":         "tool",
			"tool_call_id": "call_1",
			"name":         "read_log",
			"content":      strings.Repeat("🚀思考過程分析問題éà", 8),
		},
		{"role": "assistant", "content": "Checked app.log."},
		{"role": "user", "content": "Check config too"},
		{"role": "assistant", "content": "Checked config."},
		{"role": "user", "content": "Summarize now"},
	}
	flattened, _, err := FlattenMessages(messages, nil, nil, FlattenOptions{
		MaxPromptChars:  16000,
		MaxOldToolChars: 33,
	})
	if err != nil {
		t.Fatalf("FlattenMessages failed: %v", err)
	}
	if !utf8.ValidString(flattened) {
		t.Fatalf("FlattenMessages produced invalid UTF-8: %q", flattened)
	}
	if strings.ContainsRune(flattened, utf8.RuneError) {
		t.Fatalf("FlattenMessages produced replacement rune U+FFFD: %q", flattened)
	}
	if !strings.Contains(flattened, "...[truncated") {
		t.Fatalf("expected old tool output truncation marker in flattened prompt, got:\n%s", flattened)
	}
}

func TestE2EPrompt_Tier3_PairwiseSSRFMagicBytesAndUTF8Flatten(t *testing.T) {
	// Pairwise Tier 3 test combining 1.8 (ExtractImageParts + FetchRemoteImage SSRF/magic bytes)
	// and 5.4c (FlattenMessages + BoundOldToolText UTF-8 rune-safe slicing).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/valid.png":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(e2eMinimalPNG)
		case "/spoofed.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte(`{"evil":"html_or_json_not_image"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	messages := []map[string]any{
		{"role": "user", "content": "Inspect these attachments and logs"},
		{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{
				map[string]any{
					"id":   "call_utf8_1",
					"type": "function",
					"function": map[string]any{
						"name":      "inspect_bundle",
						"arguments": `{"target":"🚀思考"}`,
					},
				},
			},
		},
		{
			"role":         "tool",
			"tool_call_id": "call_utf8_1",
			"name":         "inspect_bundle",
			"content":      strings.Repeat("🚀思考過程分析問題éàü", 10),
		},
		{"role": "assistant", "content": "Bundle inspected."},
		{"role": "user", "content": "Summarize the first checkpoint."},
		{"role": "assistant", "content": "First checkpoint summarized."},
		{"role": "user", "content": "Summarize the second checkpoint."},
		{"role": "assistant", "content": "Second checkpoint summarized."},
		{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "Compare these images"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://100.64.0.1/internal.png"}},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": srv.URL + "/spoofed.png"}},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": srv.URL + "/valid.png"}},
			},
		},
	}

	parts := ExtractImageParts(messages)
	if len(parts) != 3 {
		t.Fatalf("expected 3 extracted image parts, got %d", len(parts))
	}

	// 1. Default SSRF protection blocks CGNAT even for extracted prompt images.
	t.Setenv("BRIDGE_ALLOW_PRIVATE_IMAGE_URLS", "")
	ctxSSRF, cancelSSRF := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelSSRF()
	if _, err := FetchRemoteImage(ctxSSRF, parts[0].RemoteURL, 0); err == nil {
		t.Fatalf("expected SSRF block on %s", parts[0].RemoteURL)
	} else {
		assertSSRFBlocked(t, err, parts[0].RemoteURL)
	}

	// 2. With local test server allowed, spoofed MIME is rejected while valid PNG passes magic-byte check.
	t.Setenv("BRIDGE_ALLOW_PRIVATE_IMAGE_URLS", "1")
	ctxSpoof, cancelSpoof := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelSpoof()
	if att, err := FetchRemoteImage(ctxSpoof, parts[1].RemoteURL, 1); err == nil || att != nil {
		t.Fatalf("expected spoofed image payload to be rejected, got att=%+v err=%v", att, err)
	}
	ctxValid, cancelValid := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelValid()
	attValid, err := FetchRemoteImage(ctxValid, parts[2].RemoteURL, 2)
	if err != nil || attValid == nil || attValid.ContentType != "image/png" {
		t.Fatalf("expected valid PNG attachment, got att=%+v err=%v", attValid, err)
	}

	// 3. FlattenMessages simultaneously bounds multi-byte UTF-8 tool history without corrupting runes.
	flattened, _, err := FlattenMessages(messages, nil, nil, FlattenOptions{
		MaxPromptChars:  16000,
		MaxOldToolChars: 29,
	})
	if err != nil {
		t.Fatalf("FlattenMessages failed: %v", err)
	}
	if !utf8.ValidString(flattened) || strings.ContainsRune(flattened, utf8.RuneError) {
		t.Fatalf("FlattenMessages produced invalid UTF-8 in multimodal+tool prompt: %q", flattened)
	}
	if !strings.Contains(flattened, "...[truncated") || !strings.Contains(flattened, "[Attached image #1]") {
		t.Fatalf("expected both truncated old tool text and image placeholder in flattened prompt:\n%s", flattened)
	}
}
