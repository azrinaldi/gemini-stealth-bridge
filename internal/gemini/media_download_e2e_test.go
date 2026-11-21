//go:build e2e

package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
	fhttp "github.com/bogdanfinn/fhttp"
)

// ============================================================================
// Self-contained E2E helpers (compile-safe across M1-M5)
// ============================================================================

type e2eRecordedReq struct {
	Method string
	URL    string
	Host   string
	Cookie string
	Body   string
	Query  url.Values
}

type e2eGeminiDoer struct {
	mu             sync.Mutex
	reqs           []e2eRecordedReq
	closeIdleCalls atomic.Int64
	fn             func(req *fhttp.Request, bodyStr string) (*fhttp.Response, error)
}

func e2eHeaderGet(h fhttp.Header, key string) string {
	for k, vals := range h {
		if strings.EqualFold(k, key) && len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

func (d *e2eGeminiDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}
	rec := e2eRecordedReq{
		Method: req.Method,
		URL:    req.URL.String(),
		Host:   req.URL.Host,
		Cookie: e2eHeaderGet(req.Header, "cookie"),
		Body:   string(bodyBytes),
		Query:  req.URL.Query(),
	}
	d.mu.Lock()
	d.reqs = append(d.reqs, rec)
	d.mu.Unlock()
	return d.fn(req, rec.Body)
}

func (d *e2eGeminiDoer) CloseIdleConnections() {
	d.closeIdleCalls.Add(1)
}

func (d *e2eGeminiDoer) Recorded() []e2eRecordedReq {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]e2eRecordedReq, len(d.reqs))
	copy(out, d.reqs)
	return out
}

type e2eHTTPTestDoer struct {
	client         *http.Client
	closeIdleCalls atomic.Int64
}

func newE2EHTTPTestDoer() *e2eHTTPTestDoer {
	return &e2eHTTPTestDoer{
		client: &http.Client{
			Transport: &http.Transport{
				DisableCompression: true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (d *e2eHTTPTestDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	var bodyReader io.Reader
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		bodyReader = bytes.NewReader(raw)
	}
	stdReq, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bodyReader)
	if err != nil {
		return nil, err
	}
	for k, vals := range req.Header {
		if strings.HasPrefix(k, ":") || strings.EqualFold(k, fhttp.HeaderOrderKey) || strings.EqualFold(k, fhttp.PHeaderOrderKey) {
			continue
		}
		for _, v := range vals {
			stdReq.Header.Add(k, v)
		}
	}
	resp, err := d.client.Do(stdReq)
	if err != nil {
		return nil, err
	}
	fh := make(fhttp.Header)
	for k, vals := range resp.Header {
		for _, v := range vals {
			fh.Add(k, v)
		}
	}
	return &fhttp.Response{
		StatusCode: resp.StatusCode,
		Header:     fh,
		Body:       resp.Body,
	}, nil
}

func (d *e2eHTTPTestDoer) CloseIdleConnections() {
	d.closeIdleCalls.Add(1)
	d.client.CloseIdleConnections()
}

type e2eChunk struct {
	data  []byte
	delay time.Duration
	ctx   context.Context
}

type e2eChunkedBody struct {
	chunks []e2eChunk
	idx    int
	off    int
	closed atomic.Bool
}

func (b *e2eChunkedBody) Read(p []byte) (int, error) {
	if b.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	for b.idx < len(b.chunks) {
		ch := &b.chunks[b.idx]
		if b.off == 0 && ch.delay > 0 {
			timer := time.NewTimer(ch.delay)
			if ch.ctx != nil {
				select {
				case <-timer.C:
				case <-ch.ctx.Done():
					timer.Stop()
					return 0, ch.ctx.Err()
				}
			} else {
				<-timer.C
			}
		}
		if b.off >= len(ch.data) {
			b.idx++
			b.off = 0
			continue
		}
		n := copy(p, ch.data[b.off:])
		b.off += n
		if b.off >= len(ch.data) {
			b.idx++
			b.off = 0
		}
		return n, nil
	}
	return 0, io.EOF
}

func (b *e2eChunkedBody) Close() error {
	b.closed.Store(true)
	return nil
}

type e2eRepeatingBody struct {
	totalBytes int64
	readBytes  atomic.Int64
	fillByte   byte
	closed     atomic.Bool
	once       sync.Once
	buf        []byte
}

func (r *e2eRepeatingBody) Read(p []byte) (int, error) {
	if r.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	r.once.Do(func() {
		b := r.fillByte
		if b == 0 {
			b = 'A'
		}
		r.buf = bytes.Repeat([]byte{b}, 64*1024)
	})
	cur := r.readBytes.Load()
	if cur >= r.totalBytes {
		return 0, io.EOF
	}
	rem := r.totalBytes - cur
	n := len(p)
	if n > len(r.buf) {
		n = len(r.buf)
	}
	if int64(n) > rem {
		n = int(rem)
	}
	n = copy(p[:n], r.buf[:n])
	r.readBytes.Add(int64(n))
	return n, nil
}

func (r *e2eRepeatingBody) Close() error {
	r.closed.Store(true)
	return nil
}

func e2eMarshalJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func e2eBuildStreamLine(cid, rid, rcid, text, thoughts string, done bool, ctxTok string) string {
	statusCode := 1
	if done {
		statusCode = 2
	}
	candMap := map[string]any{}
	if thoughts != "" {
		candMap["38"] = [][]any{{thoughts}}
	}
	cand := []any{rcid, []any{text}, nil, nil, nil, nil, nil, nil, []any{statusCode}, candMap}
	pjMap := map[string]any{
		"40": "954c0abcba24364",
		"43": "4 Argon",
	}
	if ctxTok != "" {
		pjMap["26"] = ctxTok
	}
	pj := []any{nil, []any{cid, rid}, nil, nil, []any{cand}, pjMap}
	inner := e2eMarshalJSON(pj)
	return e2eMarshalJSON([][]any{{"wrb.fr", nil, inner}})
}

func e2eNewAccountClient(id, cookieHeader, filePath string, doer transport.Doer) *AccountClient {
	pool := transport.NewPool(transport.PoolOptions{
		ClientConfig: transport.ClientConfig{
			CustomDoer:       doer,
			Timeout:          10 * time.Second,
			FirstByteTimeout: 2 * time.Second,
			StallTimeout:     5 * time.Second,
		},
		MaxSessions: 8,
		MaxQueue:    8,
		WaitTimeout: 2 * time.Second,
	})
	return NewAccountClient(id, cookieHeader, filePath, pool, cookies.NewRotator())
}

// ============================================================================
// Feature Area 1: Audit 1.3 — DownloadMedia, extractGeneratedVideos, Body Cap
// ============================================================================

func TestT1_1_3_DirectGoogleUserContentImageNoCookies(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(bytes.NewReader([]byte("PNGDATA"))),
			}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSID=psid1", "", doer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data, ct, err := ac.DownloadMedia(ctx, "https://lh3.googleusercontent.com/gg-dl/abc=s0-d", "")
	if err != nil {
		t.Fatalf("DownloadMedia failed: %v", err)
	}
	if string(data) != "PNGDATA" || ct != "image/png" {
		t.Fatalf("unexpected DownloadMedia result: data=%q ct=%q", string(data), ct)
	}
	reqs := doer.Recorded()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 outbound request, got %d", len(reqs))
	}
	if reqs[0].Cookie != "" {
		t.Fatalf("expected empty Cookie header to lh3.googleusercontent.com, got %q", reqs[0].Cookie)
	}
}

func TestT1_1_3_GoogleContributionVideoWithCookies(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{"Content-Type": []string{"video/mp4"}},
				Body:       io.NopCloser(bytes.NewReader([]byte("MP4BYTES"))),
			}, nil
		},
	}
	cookie := "SAPISID=sap1; __Secure-1PSID=psid1"
	ac := e2eNewAccountClient("a1", cookie, "", doer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data, ct, err := ac.DownloadMedia(ctx, "https://contribution.usercontent.google.com/download?c=xyz", "")
	if err != nil {
		t.Fatalf("DownloadMedia failed: %v", err)
	}
	if string(data) != "MP4BYTES" || ct != "video/mp4" {
		t.Fatalf("unexpected DownloadMedia result: data=%q ct=%q", string(data), ct)
	}
	reqs := doer.Recorded()
	if len(reqs) != 1 || reqs[0].Cookie != cookie {
		t.Fatalf("expected cookies sent to contribution.usercontent.google.com, got %+v", reqs)
	}
}

func TestT1_1_3_SameOriginRelativeRedirectResolved(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if req.URL.Path == "/v1/item" {
				return &fhttp.Response{
					StatusCode: 302,
					Header:     fhttp.Header{"Location": []string{"../signed/media.png?tok=1"}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			}
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(strings.NewReader("RESOLVED")),
			}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1", "", doer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data, ct, err := ac.DownloadMedia(ctx, "https://contribution.usercontent.google.com/v1/item", "")
	if err != nil {
		t.Fatalf("DownloadMedia failed: %v", err)
	}
	if string(data) != "RESOLVED" || ct != "image/png" {
		t.Fatalf("unexpected data=%q ct=%q", string(data), ct)
	}
	reqs := doer.Recorded()
	if len(reqs) != 2 || reqs[1].URL != "https://contribution.usercontent.google.com/signed/media.png?tok=1" {
		t.Fatalf("expected ResolveReference hop to /signed/media.png?tok=1, got %+v", reqs)
	}
}

func TestT1_1_3_TextPlainHopToGoogleHost(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if req.URL.Host == "contribution.usercontent.google.com" {
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
					Body:       io.NopCloser(strings.NewReader("https://lh3.googleusercontent.com/gg-dl/final=s0-d")),
				}, nil
			}
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(strings.NewReader("FINALIMG")),
			}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSID=psid1", "", doer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data, ct, err := ac.DownloadMedia(ctx, "https://contribution.usercontent.google.com/download?c=xyz", "")
	if err != nil {
		t.Fatalf("DownloadMedia failed: %v", err)
	}
	if string(data) != "FINALIMG" || ct != "image/png" {
		t.Fatalf("unexpected data=%q ct=%q", string(data), ct)
	}
	reqs := doer.Recorded()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 hops, got %d", len(reqs))
	}
	if reqs[0].Cookie == "" {
		t.Fatal("expected hop 1 to send cookies to *.google.com")
	}
	if reqs[1].Cookie != "" {
		t.Fatalf("expected hop 2 to lh3.googleusercontent.com to strip cookies, got %q", reqs[1].Cookie)
	}
}

func TestT1_1_3_ExtractGeneratedVideosValidAndStealthClientNormalBody(t *testing.T) {
	c12 := map[string]any{
		"60": []any{
			[]any{
				nil, nil, "clip.mp4", nil, nil, nil, nil,
				[]any{
					"https://lh3.googleusercontent.com/thumb=s100",
					"https://contribution.usercontent.google.com/download?c=1",
					"https://rr1---sn.googlevideo.com/videoplayback?id=1",
				},
				nil, nil, nil, "video/mp4",
			},
		},
	}
	cand := []any{"rc_1", []any{"Here is your video"}, nil, nil, nil, nil, nil, nil, []any{2}, map[string]any{"13": c12}}
	inner := e2eMarshalJSON([]any{nil, []any{"c_1", "r_1"}, nil, nil, []any{cand}})
	line := e2eMarshalJSON([][]any{{"wrb.fr", nil, inner}})

	parsed, err := ParseFrameLine(line)
	if err != nil {
		t.Fatalf("ParseFrameLine failed: %v", err)
	}
	if len(parsed.Frames) != 1 || len(parsed.Frames[0].GeneratedVideos) != 1 {
		t.Fatalf("expected 1 generated video, got %+v", parsed)
	}
	v := parsed.Frames[0].GeneratedVideos[0]
	if v.URL != "https://contribution.usercontent.google.com/download?c=1" ||
		v.StreamURL != "https://rr1---sn.googlevideo.com/videoplayback?id=1" ||
		v.ThumbnailURL != "https://lh3.googleusercontent.com/thumb=s1024" {
		t.Fatalf("unexpected GeneratedVideo fields: %+v", v)
	}

	// Normal 1 MiB StealthClient.Do non-streaming body succeeds cleanly.
	body1MB := &e2eRepeatingBody{totalBytes: 1 << 20, fillByte: 'Z'}
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{},
				Body:       body1MB,
			}, nil
		},
	}
	sc, err := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
	if err != nil {
		t.Fatalf("NewStealthClient: %v", err)
	}
	res, err := sc.Do(context.Background(), transport.RequestOptions{
		Method: "GET",
		URL:    "https://lh3.googleusercontent.com/ok",
	})
	if err != nil || len(res.Body) != 1<<20 {
		t.Fatalf("expected 1 MiB StealthClient.Do to succeed, got len=%d err=%v", len(res.Body), err)
	}
}

func TestT2_1_3_RejectPlainHTTPAndNonHTTPSURL(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("leak"))}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=secret", "", doer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, bad := range []string{"http://lh3.googleusercontent.com/gg-dl/abc", "file:///etc/passwd"} {
		if _, _, err := ac.DownloadMedia(ctx, bad, ""); err == nil {
			t.Fatalf("expected non-HTTPS URL %q to be rejected", bad)
		}
	}
	if reqs := doer.Recorded(); len(reqs) != 0 {
		t.Fatalf("expected 0 outbound requests for non-HTTPS URLs, got %+v", reqs)
	}
}

func TestT2_1_3_RejectNonAllowlistedHostAndCookieLeak(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("leak"))}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=secret_cookie", "", doer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, _, err := ac.DownloadMedia(ctx, "https://evil.example.com/img.png", ""); err == nil {
		t.Fatal("expected evil.example.com to be rejected")
	}
	if reqs := doer.Recorded(); len(reqs) != 0 {
		t.Fatalf("expected 0 outbound requests to evil.example.com, got %+v", reqs)
	}
}

func TestT2_1_3_Reject302RedirectToEvilOrPrivateHost(t *testing.T) {
	for _, target := range []string{"https://evil.example.com/steal", "http://169.254.169.254/latest/meta-data"} {
		t.Run(target, func(t *testing.T) {
			doer := &e2eGeminiDoer{
				fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
					if req.URL.Host == "contribution.usercontent.google.com" {
						return &fhttp.Response{
							StatusCode: 302,
							Header:     fhttp.Header{"Location": []string{target}},
							Body:       io.NopCloser(strings.NewReader("")),
						}, nil
					}
					return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("PWNED"))}, nil
				},
			}
			ac := e2eNewAccountClient("a1", "SAPISID=secret", "", doer)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if _, _, err := ac.DownloadMedia(ctx, "https://contribution.usercontent.google.com/download?c=1", ""); err == nil {
				t.Fatalf("expected redirect to %s to be rejected", target)
			}
			reqs := doer.Recorded()
			if len(reqs) != 1 {
				t.Fatalf("expected redirect target %s never to be requested (only 1 req), got %+v", target, reqs)
			}
		})
	}
}

func TestT2_1_3_StripCookiesOnCrossDomainGoogleVideoRedirect(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if req.URL.Host == "contribution.usercontent.google.com" {
				return &fhttp.Response{
					StatusCode: 302,
					Header:     fhttp.Header{"Location": []string{"https://rr1---sn.googlevideo.com/videoplayback?id=1"}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			}
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{"Content-Type": []string{"video/mp4"}},
				Body:       io.NopCloser(strings.NewReader("VIDEO")),
			}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSID=psid1", "", doer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data, _, err := ac.DownloadMedia(ctx, "https://contribution.usercontent.google.com/download?c=1", "")
	if err != nil {
		t.Fatalf("DownloadMedia failed: %v", err)
	}
	if string(data) != "VIDEO" {
		t.Fatalf("unexpected data=%q", string(data))
	}
	reqs := doer.Recorded()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(reqs))
	}
	if reqs[0].Cookie == "" {
		t.Fatal("expected hop 1 (*.google.com) to send cookies")
	}
	if reqs[1].Cookie != "" {
		t.Fatalf("expected hop 2 (*.googlevideo.com) to receive empty Cookie header, got %q", reqs[1].Cookie)
	}
}

func TestT2_1_3_RejectTextPlainHopToUnsafeHost(t *testing.T) {
	for _, bodyURL := range []string{"https://evil.example.com/pwn", "http://127.0.0.1:8080/admin"} {
		t.Run(bodyURL, func(t *testing.T) {
			doer := &e2eGeminiDoer{
				fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
					if req.URL.Host == "contribution.usercontent.google.com" {
						return &fhttp.Response{
							StatusCode: 200,
							Header:     fhttp.Header{"Content-Type": []string{"text/plain"}},
							Body:       io.NopCloser(strings.NewReader(bodyURL)),
						}, nil
					}
					return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("PWN"))}, nil
				},
			}
			ac := e2eNewAccountClient("a1", "SAPISID=sap1", "", doer)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if _, _, err := ac.DownloadMedia(ctx, "https://contribution.usercontent.google.com/download?c=1", ""); err == nil {
				t.Fatalf("expected text/plain redirect to %s to be rejected", bodyURL)
			}
			if reqs := doer.Recorded(); len(reqs) != 1 {
				t.Fatalf("expected hop 2 never to be fetched, got %+v", reqs)
			}
		})
	}
}

func TestT2_1_3_BodySizeCapOnDownloadMediaAndStealthClientDo(t *testing.T) {
	const oversize = int64(300 * 1024 * 1024)

	assertBodyLimitErr := func(t *testing.T, label string, err error, elapsed time.Duration) {
		t.Helper()
		if err == nil {
			t.Fatalf("expected %s to reject 300 MiB response body", label)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected %s to fail on body size cap, not context.DeadlineExceeded (elapsed=%s): %v", label, elapsed, err)
		}
		var ue *transport.UpstreamError
		if errors.As(err, &ue) {
			if ue.StatusCode == 499 {
				t.Fatalf("expected %s body size cap error, not 499 client_cancelled (elapsed=%s): %+v", label, elapsed, ue)
			}
		}
		if elapsed >= 5*time.Second {
			t.Fatalf("expected %s body size cap rejection to fail fast (<5s), took %s (err=%v)", label, elapsed, err)
		}
		lower := strings.ToLower(err.Error())
		if !strings.Contains(lower, "too large") && !strings.Contains(lower, "limit") && !strings.Contains(lower, "exceeded") && !strings.Contains(lower, "max") && !strings.Contains(lower, "size") {
			t.Fatalf("expected %s error to describe body size limit, got: %v", label, err)
		}
	}

	t.Run("stealth_client_body_cap", func(t *testing.T) {
		body := &e2eRepeatingBody{totalBytes: oversize, fillByte: 'A'}
		doer := &e2eGeminiDoer{
			fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
				return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: body}, nil
			},
		}
		sc, err := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
		if err != nil {
			t.Fatalf("NewStealthClient: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		start := time.Now()
		_, err = sc.Do(ctx, transport.RequestOptions{
			Method: "GET",
			URL:    "https://lh3.googleusercontent.com/huge",
		})
		elapsed := time.Since(start)
		assertBodyLimitErr(t, "StealthClient.Do", err, elapsed)
		var ue *transport.UpstreamError
		if !errors.As(err, &ue) || (ue.StatusCode != 502 && ue.StatusCode != 413) {
			t.Fatalf("expected StealthClient.Do body cap UpstreamError with status 502/413, got %+v (%v)", ue, err)
		}
		if body.readBytes.Load() >= oversize {
			t.Fatalf("expected body cap to abort before reading full 300 MiB, read=%d", body.readBytes.Load())
		}
	})

	t.Run("download_media_body_cap", func(t *testing.T) {
		body := &e2eRepeatingBody{totalBytes: oversize, fillByte: 'B'}
		doer := &e2eGeminiDoer{
			fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Content-Type": []string{"video/mp4"}},
					Body:       body,
				}, nil
			},
		}
		ac := e2eNewAccountClient("a1", "SAPISID=sap1", "", doer)
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		start := time.Now()
		_, _, err := ac.DownloadMedia(ctx, "https://lh3.googleusercontent.com/huge.mp4", "")
		elapsed := time.Since(start)
		assertBodyLimitErr(t, "DownloadMedia", err, elapsed)
		if body.readBytes.Load() >= oversize {
			t.Fatalf("expected DownloadMedia body cap to abort early, read=%d", body.readBytes.Load())
		}
	})

	t.Run("extract_generated_videos_rejects_unsafe_urls", func(t *testing.T) {
		for _, badURL := range []string{
			"http://127.0.0.1/v.mp4",
			"https://evil.example.com/v.mp4",
			"javascript:alert(1)",
		} {
			c12 := map[string]any{
				"60": []any{
					[]any{
						nil, nil, "bad.mp4", nil, nil, nil, nil,
						[]any{"", badURL, badURL},
						nil, nil, nil, "video/mp4",
					},
				},
			}
			cand := []any{"rc_1", []any{""}, nil, nil, nil, nil, nil, nil, []any{2}, map[string]any{"13": c12}}
			inner := e2eMarshalJSON([]any{nil, []any{"c_1", "r_1"}, nil, nil, []any{cand}})
			line := e2eMarshalJSON([][]any{{"wrb.fr", nil, inner}})

			parsed, err := ParseFrameLine(line)
			if err != nil {
				t.Fatalf("ParseFrameLine(%s): %v", badURL, err)
			}
			if len(parsed.Frames) != 1 {
				t.Fatalf("ParseFrameLine(%s): expected 1 frame, got %d (%+v)", badURL, len(parsed.Frames), parsed.Frames)
			}
			f := parsed.Frames[0]
			if len(f.GeneratedVideos) != 0 {
				t.Fatalf("expected unsafe video URL %q omitted from GeneratedVideos, got %+v", badURL, f.GeneratedVideos)
			}
			if strings.Contains(f.Text, badURL) {
				t.Fatalf("expected unsafe video URL %q omitted from Frame.Text, got %q", badURL, f.Text)
			}
		}
	})
}

// ============================================================================
// Feature Area 2: Audit 2.5 — Session.Lease Goroutine Leak & Queueing
// ============================================================================

func TestT1_2_5_SingleLeaseExecutesAndCleansState(t *testing.T) {
	sc, _ := transport.NewStealthClient(transport.ClientConfig{})
	sess := transport.NewSession("s1", sc, 1, "SAPISID=v1", 4, 200*time.Millisecond)
	var seen string
	if err := sess.Lease(context.Background(), func(cookie string) error {
		seen = cookie
		return nil
	}); err != nil {
		t.Fatalf("Lease failed: %v", err)
	}
	if seen != "SAPISID=v1" {
		t.Fatalf("got cookie %q, want SAPISID=v1", seen)
	}
	if sess.Active() != 0 || sess.Queued() != 0 || sess.Busy() {
		t.Fatalf("expected clean idle session, got active=%d queued=%d busy=%v", sess.Active(), sess.Queued(), sess.Busy())
	}
}

func TestT1_2_5_QueuedLeaseSucceedsWhenHolderReleases(t *testing.T) {
	sc, _ := transport.NewStealthClient(transport.ClientConfig{})
	sess := transport.NewSession("s1", sc, 1, "SAPISID=v1", 4, 500*time.Millisecond)
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = sess.Lease(context.Background(), func(string) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	time.AfterFunc(40*time.Millisecond, func() { close(release) })
	if err := sess.Lease(context.Background(), func(string) error { return nil }); err != nil {
		t.Fatalf("queued Lease failed: %v", err)
	}
	wg.Wait()
}

func TestT1_2_5_QueueCookieUpdateAppliedOnNextLease(t *testing.T) {
	sc, _ := transport.NewStealthClient(transport.ClientConfig{})
	sess := transport.NewSession("s1", sc, 1, "SAPISID=v1", 4, 500*time.Millisecond)
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = sess.Lease(context.Background(), func(string) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	sess.QueueCookieUpdate("SAPISID=v2")
	close(release)
	wg.Wait()

	var got string
	if err := sess.Lease(context.Background(), func(cookie string) error {
		got = cookie
		return nil
	}); err != nil {
		t.Fatalf("second Lease failed: %v", err)
	}
	if got != "SAPISID=v2" {
		t.Fatalf("expected queued cookie SAPISID=v2, got %q", got)
	}
}

func TestT1_2_5_RunStreamAllowsConcurrentStreamsWithinMaxQueue(t *testing.T) {
	sc, _ := transport.NewStealthClient(transport.ClientConfig{})
	sess := transport.NewSession("s1", sc, 1, "SAPISID=v1", 4, 500*time.Millisecond)
	var active atomic.Int32
	allEntered := make(chan struct{})
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := sess.RunStream(context.Background(), func(string) error {
				if active.Add(1) == 3 {
					close(allEntered)
				}
				<-allEntered
				return nil
			})
			if err != nil {
				t.Errorf("RunStream failed: %v", err)
			}
		}()
	}
	select {
	case <-allEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("3 concurrent RunStream calls deadlocked")
	}
	wg.Wait()
}

func TestT1_2_5_RepeatedSequentialLeasesZeroGoroutineGrowth(t *testing.T) {
	sc, _ := transport.NewStealthClient(transport.ClientConfig{})
	sess := transport.NewSession("s1", sc, 1, "SAPISID=v1", 4, 200*time.Millisecond)
	runtime.GC()
	before := runtime.NumGoroutine()
	for range 50 {
		if err := sess.Lease(context.Background(), func(string) error { return nil }); err != nil {
			t.Fatalf("Lease failed: %v", err)
		}
	}
	runtime.GC()
	after := runtime.NumGoroutine()
	if after-before > 2 {
		t.Fatalf("goroutine growth after sequential leases: before=%d after=%d", before, after)
	}
}

func TestT2_2_5_WaitTimeoutReturns429SessionBusyNoGoroutineLeak(t *testing.T) {
	sc, _ := transport.NewStealthClient(transport.ClientConfig{})
	sess := transport.NewSession("s1", sc, 1, "SAPISID=v1", 50, 25*time.Millisecond)
	started := make(chan struct{})
	release := make(chan struct{})
	var holderWG sync.WaitGroup
	holderWG.Add(1)
	go func() {
		defer holderWG.Done()
		_ = sess.Lease(context.Background(), func(string) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	runtime.GC()
	before := runtime.NumGoroutine()

	var waitWG sync.WaitGroup
	errs := make(chan error, 40)
	for range 40 {
		waitWG.Add(1)
		go func() {
			defer waitWG.Done()
			errs <- sess.Lease(context.Background(), func(string) error { return nil })
		}()
	}
	waitWG.Wait()
	close(errs)

	// Sample goroutine count BEFORE holder releases to prove timed-out waiters
	// didn't leave zombie goroutines blocked on execMu.
	runtime.GC()
	after := runtime.NumGoroutine()

	close(release)
	holderWG.Wait()

	for err := range errs {
		var ue *transport.UpstreamError
		if !errors.As(err, &ue) || ue.StatusCode != 429 || ue.Code != "session_busy" {
			t.Fatalf("expected 429 session_busy, got %v", err)
		}
	}
	if delta := after - before; delta > 4 {
		t.Fatalf("Session.Lease wait-timeout leaked goroutines before holder release: before=%d after=%d delta=%d", before, after, delta)
	}
}

func TestT2_2_5_ContextCancelWhileQueuedReturns499NoGoroutineLeak(t *testing.T) {
	sc, _ := transport.NewStealthClient(transport.ClientConfig{})
	sess := transport.NewSession("s1", sc, 1, "SAPISID=v1", 50, 5*time.Second)
	started := make(chan struct{})
	release := make(chan struct{})
	var holderWG sync.WaitGroup
	holderWG.Add(1)
	go func() {
		defer holderWG.Done()
		_ = sess.Lease(context.Background(), func(string) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	runtime.GC()
	before := runtime.NumGoroutine()

	var waitWG sync.WaitGroup
	errs := make(chan error, 40)
	for range 40 {
		waitWG.Add(1)
		go func() {
			defer waitWG.Done()
			cctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
			defer cancel()
			errs <- sess.Lease(cctx, func(string) error { return nil })
		}()
	}
	waitWG.Wait()
	close(errs)

	runtime.GC()
	after := runtime.NumGoroutine()

	close(release)
	holderWG.Wait()

	for err := range errs {
		var ue *transport.UpstreamError
		if !errors.As(err, &ue) || ue.StatusCode != 499 || ue.Code != "client_cancelled" {
			t.Fatalf("expected 499 client_cancelled, got %v", err)
		}
	}
	if delta := after - before; delta > 4 {
		t.Fatalf("Session.Lease cancel leaked goroutines before holder release: before=%d after=%d delta=%d", before, after, delta)
	}
}

func TestT2_2_5_PreCancelledContextFastReject(t *testing.T) {
	sc, _ := transport.NewStealthClient(transport.ClientConfig{})
	sess := transport.NewSession("s1", sc, 1, "SAPISID=v1", 4, 200*time.Millisecond)
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = sess.Lease(context.Background(), func(string) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sess.Lease(cctx, func(string) error { return nil })
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 499 || ue.Code != "client_cancelled" {
		t.Fatalf("expected 499 client_cancelled, got %v", err)
	}
	if sess.Active() != 1 {
		t.Fatalf("expected holder active count to remain 1, got %d", sess.Active())
	}
	close(release)
	wg.Wait()
}

func TestT2_2_5_QueueOverflowReturns429SessionBusy(t *testing.T) {
	sc, _ := transport.NewStealthClient(transport.ClientConfig{})
	sess := transport.NewSession("s1", sc, 1, "SAPISID=v1", 1, 2*time.Second)
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = sess.Lease(context.Background(), func(string) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	go func() {
		defer wg.Done()
		_ = sess.Lease(context.Background(), func(string) error { return nil })
	}()
	deadline := time.Now().Add(500 * time.Millisecond)
	for sess.Queued() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	err := sess.Lease(context.Background(), func(string) error { return nil })
	elapsed := time.Since(start)
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 429 || ue.Code != "session_busy" {
		t.Fatalf("expected immediate 429 session_busy, got %v", err)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("expected immediate queue overflow rejection, waited %s", elapsed)
	}
	close(release)
	wg.Wait()
}

func TestT2_2_5_PostTimeoutRecoveryClean(t *testing.T) {
	sc, _ := transport.NewStealthClient(transport.ClientConfig{})
	sess := transport.NewSession("s1", sc, 1, "SAPISID=v1", 10, 20*time.Millisecond)
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = sess.Lease(context.Background(), func(string) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	_ = sess.Lease(context.Background(), func(string) error { return nil })
	close(release)
	wg.Wait()

	if err := sess.Lease(context.Background(), func(string) error { return nil }); err != nil {
		t.Fatalf("expected fresh Lease after timeout recovery to succeed, got %v", err)
	}
	if sess.Active() != 0 {
		t.Fatalf("expected Active() == 0, got %d", sess.Active())
	}
}

// ============================================================================
// Feature Area 3: Audit 2.6 — frameWatchdog vs StreamGenerateLive
// ============================================================================

func newBootstrappedAccountWithStreamChunks(t *testing.T, chunks []e2eChunk) (*AccountClient, *e2eGeminiDoer) {
	t.Helper()
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "StreamGenerate") {
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       &e2eChunkedBody{chunks: chunks},
				}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	ac := e2eNewAccountClient("fw1", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=ts1", "", doer)
	ac.SetTokens("at_1", "bl_1", "fsid_1")
	return ac, doer
}

func TestT1_2_6_FastFrameSucceeds(t *testing.T) {
	line := e2eBuildStreamLine("c1", "r1", "rc1", "fast answer", "", true, "ctx1")
	ac, _ := newBootstrappedAccountWithStreamChunks(t, []e2eChunk{
		{data: []byte(")]}'\n\n" + line + "\n"), delay: 5 * time.Millisecond},
	})
	var got string
	res, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{NoFrameTimeout: 80 * time.Millisecond}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 {
			got = u.Frames[0].Text
		}
		return nil
	})
	if err != nil || res.Empty || got != "fast answer" {
		t.Fatalf("expected fast answer, got res=%+v got=%q err=%v", res, got, err)
	}
}

func TestT1_2_6_SlowSingleFrameWithinTimeoutSucceeds(t *testing.T) {
	line := e2eBuildStreamLine("c1", "r1", "rc1", "slow valid", "", true, "ctx1")
	ac, _ := newBootstrappedAccountWithStreamChunks(t, []e2eChunk{
		{data: []byte(")]}'\n\n[[\"wrb.fr\",null,null,null,null,[3]]]\n"), delay: 10 * time.Millisecond},
		{data: []byte(line + "\n"), delay: 25 * time.Millisecond},
	})
	res, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{NoFrameTimeout: 120 * time.Millisecond}, nil)
	if err != nil || res.Empty {
		t.Fatalf("expected valid frame within timeout to succeed, got res=%+v err=%v", res, err)
	}
}

func TestT1_2_6_MultiFrameStreamExceedingNoFrameTimeoutSucceeds(t *testing.T) {
	f1 := e2eBuildStreamLine("c1", "r1", "rc1", "part1", "", false, "")
	f2 := e2eBuildStreamLine("c1", "r1", "rc1", "part1 part2", "", true, "ctx1")
	ac, _ := newBootstrappedAccountWithStreamChunks(t, []e2eChunk{
		{data: []byte(")]}'\n\n" + f1 + "\n"), delay: 10 * time.Millisecond},
		{data: []byte(f2 + "\n"), delay: 80 * time.Millisecond},
	})
	var last string
	res, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{NoFrameTimeout: 45 * time.Millisecond}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 {
			last = u.Frames[0].Text
		}
		return nil
	})
	if err != nil || res.Empty || last != "part1 part2" {
		t.Fatalf("expected multi-frame stream past NoFrameTimeout to succeed, got res=%+v last=%q err=%v", res, last, err)
	}
}

func TestT1_2_6_FrameInFinalTailChunkSucceeds(t *testing.T) {
	line := e2eBuildStreamLine("c1", "r1", "rc1", "tail answer", "", true, "ctx1")
	ac, _ := newBootstrappedAccountWithStreamChunks(t, []e2eChunk{
		{data: []byte(")]}'\n\n" + line), delay: 5 * time.Millisecond}, // no trailing newline
	})
	var got string
	res, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{NoFrameTimeout: 80 * time.Millisecond}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 {
			got = u.Frames[0].Text
		}
		return nil
	})
	if err != nil || res.Empty || got != "tail answer" {
		t.Fatalf("expected tail flush answer, got res=%+v got=%q err=%v", res, got, err)
	}
}

func TestT1_2_6_ZeroNoFrameTimeoutUsesDefault(t *testing.T) {
	line := e2eBuildStreamLine("c1", "r1", "rc1", "ok", "", true, "ctx1")
	ac, _ := newBootstrappedAccountWithStreamChunks(t, []e2eChunk{
		{data: []byte(")]}'\n\n" + line + "\n"), delay: 5 * time.Millisecond},
	})
	res, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{NoFrameTimeout: 0}, nil)
	if err != nil || res.Empty {
		t.Fatalf("expected zero NoFrameTimeout to use default and succeed, got res=%+v err=%v", res, err)
	}
}

func TestT2_2_6_ValidFrameAlreadyReceivedThenStreamLongerThanTimeout(t *testing.T) {
	f1 := e2eBuildStreamLine("c1", "r1", "rc1", "first valid frame", "", false, "")
	f2 := e2eBuildStreamLine("c1", "r1", "rc1", "first valid frame complete", "", true, "ctx1")
	ac, _ := newBootstrappedAccountWithStreamChunks(t, []e2eChunk{
		{data: []byte(")]}'\n\n" + f1 + "\n"), delay: 5 * time.Millisecond},
		{data: []byte("[[\"wrb.fr\",null,null,null,null,[3]]]\n"), delay: 60 * time.Millisecond},
		{data: []byte(f2 + "\n"), delay: 10 * time.Millisecond},
	})
	res, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{NoFrameTimeout: 35 * time.Millisecond}, nil)
	if err != nil {
		t.Fatalf("stream already received valid frame, MUST NOT return stall error: %v", err)
	}
	if res.Empty {
		t.Fatal("expected non-empty StreamResponse")
	}
}

func TestT2_2_6_KeepaliveOnlyExceedingNoFrameTimeoutReturns504(t *testing.T) {
	keepalive := []byte("[[\"wrb.fr\",null,null,null,null,[3]]]\n")
	chunks := []e2eChunk{{data: []byte(")]}'\n\n"), delay: 5 * time.Millisecond}}
	for range 8 {
		chunks = append(chunks, e2eChunk{data: keepalive, delay: 15 * time.Millisecond})
	}
	ac, _ := newBootstrappedAccountWithStreamChunks(t, chunks)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := ac.StreamGenerateLive(ctx, "hi", StreamOptions{NoFrameTimeout: 40 * time.Millisecond}, nil)
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 504 || ue.Code != "upstream_stalled" {
		t.Fatalf("expected 504 upstream_stalled on keepalive-only stream, got %+v (%v)", ue, err)
	}
}

func TestT2_2_6_CallerCancelNotReportedAsWatchdogStall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	ac, _ := newBootstrappedAccountWithStreamChunks(t, []e2eChunk{
		{data: []byte(")]}'\n\n"), delay: 5 * time.Millisecond},
		{data: []byte("never"), delay: 500 * time.Millisecond, ctx: ctx},
	})
	_, err := ac.StreamGenerateLive(ctx, "hi", StreamOptions{NoFrameTimeout: 250 * time.Millisecond}, nil)
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 499 || ue.Code != "client_cancelled" {
		t.Fatalf("expected 499 client_cancelled on caller cancel, got %+v (%v)", ue, err)
	}
}

func TestT2_2_6_Empty200EOFBeforeTimeoutIsEmptyNotStall(t *testing.T) {
	ac, _ := newBootstrappedAccountWithStreamChunks(t, []e2eChunk{
		{data: []byte(")]}'\n\n"), delay: 5 * time.Millisecond},
	})
	res, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{NoFrameTimeout: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatalf("expected nil error on empty 200 EOF before timeout, got %v", err)
	}
	if !res.Empty {
		t.Fatal("expected res.Empty == true")
	}
}

func TestT2_2_6_ConcurrentDisarmAndReadRaceFree(t *testing.T) {
	for i := range 20 {
		t.Run(fmt.Sprintf("iter_%d", i), func(t *testing.T) {
			t.Parallel()
			line := e2eBuildStreamLine("c1", "r1", "rc1", "ok", "", true, "ctx1")
			ac, _ := newBootstrappedAccountWithStreamChunks(t, []e2eChunk{
				{data: []byte(")]}'\n\n"), delay: 5 * time.Millisecond},
				{data: []byte(line + "\n"), delay: 18 * time.Millisecond},
				{data: []byte("[[\"di\",10]]\n"), delay: 25 * time.Millisecond},
			})
			var frames atomic.Int32
			_, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{NoFrameTimeout: 25 * time.Millisecond}, func(u *LineParseResult) error {
				frames.Add(int32(len(u.Frames)))
				return nil
			})
			if frames.Load() > 0 && err != nil {
				t.Fatalf("valid frame already arrived (%d) yet returned error: %v", frames.Load(), err)
			}
		})
	}
}

// ============================================================================
// Feature Area 4: Audit 3.1 — Cookie Preservation
// ============================================================================

func TestT1_3_1_BootstrappedTokensUsedInStream(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.HasSuffix(u, "/app"):
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_boot_1","cfb2h":"boq_b1","FdrFJe":"fsid_b1","oPEP7c":"u@example.com"}`)),
				}, nil
			case strings.Contains(u, "StreamGenerate"):
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader(")]}'\n\n" + e2eBuildStreamLine("c1", "r1", "rc1", "ok", "", true, "ctx1") + "\n")),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=ts1", "", doer)
	if err := ac.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if _, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, nil); err != nil {
		t.Fatalf("StreamGenerateLive: %v", err)
	}
	reqs := doer.Recorded()
	last := reqs[len(reqs)-1]
	form, _ := url.ParseQuery(last.Body)
	if form.Get("at") != "at_boot_1" || !strings.Contains(last.Cookie, "__Secure-1PSIDTS=ts1") {
		t.Fatalf("expected bootstrapped at/cookie in StreamGenerate, got at=%q cookie=%q", form.Get("at"), last.Cookie)
	}
}

func TestT1_3_1_SelfHealRotationPropagatesToStreamGenerate(t *testing.T) {
	var rotated atomic.Bool
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.HasSuffix(u, "/app"):
				if !rotated.Load() {
					return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`<html>no token</html>`))}, nil
				}
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_fresh","cfb2h":"bl_fresh","FdrFJe":"99","oPEP7c":"u@example.com"}`)),
				}, nil
			case strings.Contains(u, "RotateCookies"):
				rotated.Store(true)
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=fresh_ts; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			case strings.Contains(u, "StreamGenerate"):
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(")]}'\n\n" + e2eBuildStreamLine("c1", "r1", "rc1", "ok", "", true, "ctx1") + "\n")),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=stale_ts", "", doer)
	if _, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, nil); err != nil {
		t.Fatalf("StreamGenerateLive: %v", err)
	}
	for _, r := range doer.Recorded() {
		if strings.Contains(r.URL, "StreamGenerate") {
			if !strings.Contains(r.Cookie, "__Secure-1PSIDTS=fresh_ts") || strings.Contains(r.Cookie, "stale_ts") {
				t.Fatalf("expected StreamGenerate to send fresh_ts (not stale_ts), got %q", r.Cookie)
			}
		}
	}
}

func TestT1_3_1_UploadFileSucceedsWithLatestCookie(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "content-push.googleapis.com") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("/contrib_id/abc"))}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSIDTS=latest_ts", "", doer)
	up, err := ac.UploadFile(context.Background(), []byte("imgbytes"), "pic.png", "image/png")
	if err != nil || up.Path != "/contrib_id/abc" {
		t.Fatalf("UploadFile failed: up=%+v err=%v", up, err)
	}
	reqs := doer.Recorded()
	if len(reqs) != 1 || !strings.Contains(reqs[0].Cookie, "latest_ts") {
		t.Fatalf("expected UploadFile to send latest_ts cookie, got %+v", reqs)
	}
}

func TestT1_3_1_StreamSetCookieUpdatesAccountAndSession(t *testing.T) {
	var call atomic.Int32
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "StreamGenerate") {
				n := call.Add(1)
				hdr := fhttp.Header{}
				if n == 1 {
					hdr.Add("Set-Cookie", "__Secure-1PSIDTS=stream_ts_2; Path=/; Secure")
				}
				return &fhttp.Response{
					StatusCode: 200,
					Header:     hdr,
					Body:       io.NopCloser(strings.NewReader(")]}'\n\n" + e2eBuildStreamLine("c1", "r1", "rc1", "ok", "", true, "ctx1") + "\n")),
				}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=ts1", "", doer)
	ac.SetTokens("at1", "bl1", "fs1")
	if _, err := ac.StreamGenerateLive(context.Background(), "turn1", StreamOptions{}, nil); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if _, err := ac.StreamGenerateLive(context.Background(), "turn2", StreamOptions{}, nil); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	reqs := doer.Recorded()
	if len(reqs) < 2 || !strings.Contains(reqs[1].Cookie, "__Secure-1PSIDTS=stream_ts_2") {
		t.Fatalf("expected turn 2 StreamGenerate to carry stream_ts_2, got %+v", reqs)
	}
}

func TestT1_3_1_SessionQueueCookieUpdateVisibleToNextLease(t *testing.T) {
	sc, _ := transport.NewStealthClient(transport.ClientConfig{})
	sess := transport.NewSession("s1", sc, 1, "SAPISID=sap1; __Secure-1PSIDTS=v1", 4, time.Second)
	sess.QueueCookieUpdate("SAPISID=sap1; __Secure-1PSIDTS=v2")
	var got string
	if err := sess.Lease(context.Background(), func(c string) error {
		got = c
		return nil
	}); err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if got != "SAPISID=sap1; __Secure-1PSIDTS=v2" {
		t.Fatalf("expected v2, got %q", got)
	}
}

func TestT2_3_1_EnsureBootstrappedSelfHealNotOverwrittenByLeasedCookie(t *testing.T) {
	var rotated atomic.Bool
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.HasSuffix(u, "/app"):
				if !rotated.Load() {
					return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`<html>expired</html>`))}, nil
				}
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_fresh","cfb2h":"bl1","FdrFJe":"fs1","oPEP7c":"u@example.com"}`)),
				}, nil
			case strings.Contains(u, "RotateCookies"):
				rotated.Store(true)
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=fresh_ts; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			case strings.Contains(u, "StreamGenerate"):
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(")]}'\n\n" + e2eBuildStreamLine("c1", "r1", "rc1", "ok", "", true, "ctx1") + "\n")),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=stale_ts", "", doer)
	// Pre-populate Session in Pool with stale_ts so RunStream passes leasedCookie="...stale_ts".
	if _, err := ac.Pool.Acquire(context.Background(), "a1", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=stale_ts"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, nil); err != nil {
		t.Fatalf("StreamGenerateLive: %v", err)
	}
	if !strings.Contains(ac.CookieHeader(), "fresh_ts") {
		t.Fatalf("expected ac.CookieHeader() to retain fresh_ts, got %q", ac.CookieHeader())
	}
	for _, r := range doer.Recorded() {
		if strings.Contains(r.URL, "StreamGenerate") {
			if !strings.Contains(r.Cookie, "fresh_ts") || strings.Contains(r.Cookie, "stale_ts") {
				t.Fatalf("StreamGenerate overwrote self-healed fresh_ts with stale leasedCookie: %q", r.Cookie)
			}
		}
	}
}

func TestT2_3_1_UploadFileDoesNotOverwriteNewerRotatedCookie(t *testing.T) {
	holderStarted := make(chan struct{})
	releaseHolder := make(chan struct{})
	uploadLeaseEntered := make(chan struct{})

	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.Contains(u, "RotateCookies"):
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=newer_ts; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			case strings.Contains(u, "content-push.googleapis.com"):
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("/upload/p1"))}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=stale_ts", "", doer)
	sess, err := ac.Pool.Acquire(context.Background(), "a1", ac.CookieHeader())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	var holderWG sync.WaitGroup
	holderWG.Add(1)
	go func() {
		defer holderWG.Done()
		_ = sess.Lease(context.Background(), func(string) error {
			close(holderStarted)
			<-releaseHolder
			return nil
		})
	}()
	<-holderStarted

	// Start UploadFile while holder blocks on Session.Lease; UploadFile acquires sess with stale_ts.
	var uploadWG sync.WaitGroup
	var uploadErr error
	uploadWG.Add(1)
	go func() {
		defer uploadWG.Done()
		close(uploadLeaseEntered)
		_, uploadErr = ac.UploadFile(context.Background(), []byte("abc"), "f.png", "image/png")
	}()
	<-uploadLeaseEntered
	queueDeadline := time.Now().Add(500 * time.Millisecond)
	for sess.Queued() == 0 {
		if time.Now().After(queueDeadline) {
			close(releaseHolder)
			t.Fatal("timed out waiting for UploadFile to queue on sess.Lease")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// RotateCookies runs concurrently and updates ac.cookieHeader -> newer_ts.
	if _, rErr := ac.RotateCookies(context.Background()); rErr != nil {
		t.Fatalf("RotateCookies: %v", rErr)
	}
	close(releaseHolder)
	holderWG.Wait()
	uploadWG.Wait()
	if uploadErr != nil {
		t.Fatalf("UploadFile: %v", uploadErr)
	}
	if !strings.Contains(ac.CookieHeader(), "newer_ts") {
		t.Fatalf("UploadFile regressed ac.CookieHeader() back to stale_ts: got %q", ac.CookieHeader())
	}
}

func TestT2_3_1_PoolAcquireWithStaleCookieDoesNotClobberPendingCookie(t *testing.T) {
	pool := transport.NewPool(transport.PoolOptions{MaxSessions: 4, MaxQueue: 4, WaitTimeout: time.Second})
	sess, err := pool.Acquire(context.Background(), "a1", "SAPISID=sap1; __Secure-1PSIDTS=v1")
	if err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	sess.QueueCookieUpdate("SAPISID=sap1; __Secure-1PSIDTS=v2")
	// Stale concurrent caller acquires with v1; must NOT clobber queued v2!
	sess2, err := pool.Acquire(context.Background(), "a1", "SAPISID=sap1; __Secure-1PSIDTS=v1")
	if err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}
	var got string
	if err := sess2.Lease(context.Background(), func(c string) error {
		got = c
		return nil
	}); err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if got != "SAPISID=sap1; __Secure-1PSIDTS=v2" {
		t.Fatalf("stale Acquire clobbered pendingCookie: got %q, want v2", got)
	}
}

func TestT2_3_1_StaleStreamSetCookieDoesNotOverwriteNewerRotation(t *testing.T) {
	streamStarted := make(chan struct{})
	releaseStream := make(chan struct{})

	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.Contains(u, "StreamGenerate"):
				close(streamStarted)
				<-releaseStream
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=old_g0; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader(")]}'\n\n" + e2eBuildStreamLine("c1", "r1", "rc1", "ok", "", true, "ctx1") + "\n")),
				}, nil
			case strings.Contains(u, "RotateCookies"):
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=new_g1; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=g0", "", doer)
	ac.SetTokens("at1", "bl1", "fs1")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, nil)
	}()
	<-streamStarted

	if _, err := ac.RotateCookies(context.Background()); err != nil {
		t.Fatalf("RotateCookies: %v", err)
	}
	close(releaseStream)
	wg.Wait()

	if !strings.Contains(ac.CookieHeader(), "new_g1") {
		t.Fatalf("expected ac.CookieHeader() to keep new_g1, got %q", ac.CookieHeader())
	}
}

func TestT2_3_1_FallbackUploadPreservesRotatedCookie(t *testing.T) {
	scotrupEntered := make(chan struct{})
	releaseScotrup := make(chan struct{})

	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.Contains(u, "content-push.googleapis.com"):
				return &fhttp.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("err"))}, nil
			case strings.Contains(u, "push.clients6.google.com/upload/"):
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"X-Goog-Upload-Url": []string{"https://push.clients6.google.com/put/1"}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			case strings.Contains(u, "push.clients6.google.com/put/1"):
				close(scotrupEntered)
				<-releaseScotrup
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("/contrib_id/fallback1"))}, nil
			case strings.Contains(u, "RotateCookies"):
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=rotated_fb; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=init_ts", "", doer)

	var wg sync.WaitGroup
	var upErr error
	var up UploadedFile
	wg.Add(1)
	go func() {
		defer wg.Done()
		up, upErr = ac.UploadFile(context.Background(), []byte("data"), "pic.png", "image/png")
	}()
	<-scotrupEntered
	if _, err := ac.RotateCookies(context.Background()); err != nil {
		t.Fatalf("RotateCookies: %v", err)
	}
	close(releaseScotrup)
	wg.Wait()
	if upErr != nil || up.Path != "/contrib_id/fallback1" {
		t.Fatalf("UploadFile fallback failed: up=%+v err=%v", up, upErr)
	}
	if !strings.Contains(ac.CookieHeader(), "rotated_fb") {
		t.Fatalf("expected ac.CookieHeader() to keep rotated_fb, got %q", ac.CookieHeader())
	}
}

// ============================================================================
// Feature Area 5: Audit 3.2 — persistState, ReloadFromDiskIfNewer, rotateOnClient
// ============================================================================

func TestT1_3_2_LoadAccountFromFilePopulatesAllFields(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "acc1.json")
	_, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:         "acc1",
		Email:        "acc1@example.com",
		Cookies:      "SAPISID=sap1; __Secure-1PSID=psid1",
		AT:           "at_disk",
		AuthUser:     2,
		Disabled:     true,
		AccountGroup: "team-a",
		ProxyURL:     "http://127.0.0.1:9999",
		Surfaces:     map[string]string{"geminiWeb": "ok"},
	})
	if err != nil {
		t.Fatalf("SaveCredentialFile: %v", err)
	}
	pool := transport.NewPool(transport.PoolOptions{MaxSessions: 2})
	ac, err := LoadAccountFromFile(acctPath, pool, cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile: %v", err)
	}
	if !ac.disabled || ac.proxyURL != "http://127.0.0.1:9999" || ac.accountGroup != "team-a" ||
		ac.surfaces["geminiWeb"] != "ok" || ac.authUser != 2 || ac.at != "at_disk" {
		t.Fatalf("unexpected loaded AccountClient fields: disabled=%v proxy=%q group=%q surf=%+v authUser=%d at=%q",
			ac.disabled, ac.proxyURL, ac.accountGroup, ac.surfaces, ac.authUser, ac.at)
	}
}

func TestT1_3_2_ReloadFromDiskIfNewerUpdatesCookiesAndTokens(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "acc1.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:    "acc1",
		Email:   "old@example.com",
		Cookies: "SAPISID=old_sap",
		AT:      "at_old",
	}); err != nil {
		t.Fatalf("SaveCredentialFile 1: %v", err)
	}
	ac, err := LoadAccountFromFile(acctPath, transport.NewPool(transport.PoolOptions{}), cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile: %v", err)
	}
	time.Sleep(15 * time.Millisecond)
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:    "acc1",
		Email:   "new@example.com",
		Cookies: "SAPISID=new_sap",
		AT:      "at_new",
	}); err != nil {
		t.Fatalf("SaveCredentialFile 2: %v", err)
	}
	if !ac.ReloadFromDiskIfNewer() {
		t.Fatal("expected ReloadFromDiskIfNewer to return true")
	}
	if ac.CookieHeader() != "SAPISID=new_sap" || ac.Email() != "new@example.com" || ac.at != "at_new" {
		t.Fatalf("unexpected reloaded fields: cookie=%q email=%q at=%q", ac.CookieHeader(), ac.Email(), ac.at)
	}
}

func TestT1_3_2_ReloadFromDiskIfNewerUpdatesDisabledProxyGroupSurfaces(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "acc1.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:    "acc1",
		Cookies: "SAPISID=sap1",
	}); err != nil {
		t.Fatalf("SaveCredentialFile 1: %v", err)
	}
	ac, err := LoadAccountFromFile(acctPath, transport.NewPool(transport.PoolOptions{}), cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile: %v", err)
	}
	time.Sleep(15 * time.Millisecond)
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:         "acc1",
		Cookies:      "SAPISID=sap1",
		Disabled:     true,
		ProxyURL:     "http://127.0.0.1:8888",
		AccountGroup: "team-b",
		Surfaces:     map[string]string{"geminiWeb": "ok"},
	}); err != nil {
		t.Fatalf("SaveCredentialFile 2: %v", err)
	}
	_ = ac.ReloadFromDiskIfNewer()
	ac.mu.RLock()
	disabled, proxy, grp, surf := ac.disabled, ac.proxyURL, ac.accountGroup, ac.surfaces["geminiWeb"]
	ac.mu.RUnlock()
	if !disabled || proxy != "http://127.0.0.1:8888" || grp != "team-b" || surf != "ok" {
		t.Fatalf("ReloadFromDiskIfNewer did not reload operational fields: disabled=%v proxy=%q grp=%q surf=%q", disabled, proxy, grp, surf)
	}
}

func TestT1_3_2_ReloadWhenMtimeUnchangedReturnsFalse(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "acc1.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:    "acc1",
		Cookies: "SAPISID=sap1",
	}); err != nil {
		t.Fatalf("SaveCredentialFile: %v", err)
	}
	ac, err := LoadAccountFromFile(acctPath, transport.NewPool(transport.PoolOptions{}), cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile: %v", err)
	}
	ac.mu.Lock()
	ac.modelsCache = &AvailableModelsResult{Models: []ModelInfo{{ModelName: "gemini-pro"}}}
	ac.mu.Unlock()

	if ac.ReloadFromDiskIfNewer() {
		t.Fatal("expected ReloadFromDiskIfNewer with unchanged mtime to return false")
	}
	ac.mu.RLock()
	cached := ac.modelsCache
	ac.mu.RUnlock()
	if cached == nil {
		t.Fatal("expected modelsCache to remain intact when mtime unchanged")
	}
}

func TestT1_3_2_PersistStateWritesFileMode0600(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "acc1.json")
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=persisted_ts; Path=/; Secure"}},
				Body:       io.NopCloser(strings.NewReader(`[]`)),
			}, nil
		},
	}
	ac := e2eNewAccountClient("acc1", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=old_ts", acctPath, doer)
	if _, err := ac.RotateCookies(context.Background()); err != nil {
		t.Fatalf("RotateCookies: %v", err)
	}
	ac.SetSurfaceStatus("geminiWeb", "ok")

	st, err := os.Stat(acctPath)
	if err != nil {
		t.Fatalf("stat account file: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("expected file mode 0600, got %o", st.Mode().Perm())
	}
	cf, _, err := cookies.LoadAccountFile(acctPath)
	if err != nil {
		t.Fatalf("LoadAccountFile: %v", err)
	}
	if !strings.Contains(cf.Cookies, "persisted_ts") || cf.Surfaces["geminiWeb"] != "ok" {
		t.Fatalf("unexpected persisted CredentialFile: %+v", cf)
	}
}

func TestT2_3_2_ConcurrentPersistStateDoesNotClobberNewerCookie(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "acc1.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:    "acc1",
		Cookies: "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=init_ts",
	}); err != nil {
		t.Fatalf("SaveCredentialFile: %v", err)
	}
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=fresh_ts; Path=/; Secure"}},
				Body:       io.NopCloser(strings.NewReader(`[]`)),
			}, nil
		},
	}
	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 4})
	ac, err := LoadAccountFromFile(acctPath, pool, cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = ac.RotateCookies(context.Background())
	}()
	for i := range 10 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ac.SetSurfaceStatus("geminiWeb", "ok")
			ac.SetDisabled(idx%2 == 0)
		}(i)
	}
	wg.Wait()

	cf, _, err := cookies.LoadAccountFile(acctPath)
	if err != nil {
		t.Fatalf("LoadAccountFile: %v", err)
	}
	if !strings.Contains(cf.Cookies, "fresh_ts") {
		t.Fatalf("concurrent persistState clobbered fresh_ts on disk: got %q", cf.Cookies)
	}
}

func TestT2_3_2_RotateOnClientInFlightDoesNotOverwriteNewerDiskReload(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "acc1.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:    "acc1",
		Cookies: "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=init_ts",
	}); err != nil {
		t.Fatalf("SaveCredentialFile: %v", err)
	}
	rotStarted := make(chan struct{})
	releaseRot := make(chan struct{})
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			close(rotStarted)
			<-releaseRot
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=rot_older_ts; Path=/; Secure"}},
				Body:       io.NopCloser(strings.NewReader(`[]`)),
			}, nil
		},
	}
	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 4})
	ac, err := LoadAccountFromFile(acctPath, pool, cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = ac.RotateCookies(context.Background())
	}()
	<-rotStarted

	time.Sleep(15 * time.Millisecond)
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:    "acc1",
		Cookies: "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=disk_newest_ts",
	}); err != nil {
		t.Fatalf("SaveCredentialFile newer: %v", err)
	}
	if !ac.ReloadFromDiskIfNewer() {
		t.Fatal("expected ReloadFromDiskIfNewer to pick up disk_newest_ts")
	}

	close(releaseRot)
	wg.Wait()

	if !strings.Contains(ac.CookieHeader(), "disk_newest_ts") {
		t.Fatalf("in-flight RotateCookies overwrote newer disk reload: got %q", ac.CookieHeader())
	}
}

func TestT2_3_2_ReloadIgnoresEmptyCookiesOrInvalidJSON(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "acc1.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:    "acc1",
		Cookies: "SAPISID=good_cookie",
	}); err != nil {
		t.Fatalf("SaveCredentialFile: %v", err)
	}
	ac, err := LoadAccountFromFile(acctPath, transport.NewPool(transport.PoolOptions{}), cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile: %v", err)
	}

	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(acctPath, []byte("{corrupt json"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if ac.ReloadFromDiskIfNewer() {
		t.Fatal("expected ReloadFromDiskIfNewer to return false on corrupt JSON")
	}
	if ac.CookieHeader() != "SAPISID=good_cookie" {
		t.Fatalf("cookies corrupted: %q", ac.CookieHeader())
	}

	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(acctPath, []byte(`{"name":"acc1","cookies":""}`), 0o600); err != nil {
		t.Fatalf("write empty cookies: %v", err)
	}
	if ac.ReloadFromDiskIfNewer() {
		t.Fatal("expected ReloadFromDiskIfNewer to return false on empty cookies")
	}
	if ac.CookieHeader() != "SAPISID=good_cookie" {
		t.Fatalf("cookies overwritten with empty: %q", ac.CookieHeader())
	}
}

func TestT2_3_2_ConcurrentReloadAndPersistStateRaceFree(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "acc1.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:    "acc1",
		Cookies: "SAPISID=init",
	}); err != nil {
		t.Fatalf("SaveCredentialFile: %v", err)
	}
	ac, err := LoadAccountFromFile(acctPath, transport.NewPool(transport.PoolOptions{}), cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile: %v", err)
	}

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 10 {
				if id%2 == 0 {
					ac.SetDisabled(i%2 == 0)
					ac.SetSurfaceStatus("geminiWeb", "ok")
				} else {
					_ = ac.ReloadFromDiskIfNewer()
				}
			}
		}(g)
	}
	wg.Wait()

	if _, _, err := cookies.LoadAccountFile(acctPath); err != nil {
		t.Fatalf("expected valid JSON on disk after concurrent reload/persist, got %v", err)
	}
}

func TestT2_3_2_ReloadUpdatesSessionProxyOnNextAcquire(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "acc1.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:     "acc1",
		Cookies:  "SAPISID=sap1",
		ProxyURL: "http://127.0.0.1:7001",
	}); err != nil {
		t.Fatalf("SaveCredentialFile 1: %v", err)
	}
	pool := transport.NewPool(transport.PoolOptions{MaxSessions: 4})
	defer pool.Reset()
	ac, err := LoadAccountFromFile(acctPath, pool, cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile: %v", err)
	}
	s1, err := ac.acquireSession(context.Background())
	if err != nil || s1.ProxyURL() != "http://127.0.0.1:7001" {
		t.Fatalf("expected initial proxy 7001, got s=%+v err=%v", s1, err)
	}

	time.Sleep(15 * time.Millisecond)
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:     "acc1",
		Cookies:  "SAPISID=sap1",
		ProxyURL: "http://127.0.0.1:7002",
	}); err != nil {
		t.Fatalf("SaveCredentialFile 2: %v", err)
	}
	s2, err := ac.acquireSession(context.Background())
	if err != nil || s2.ProxyURL() != "http://127.0.0.1:7002" {
		t.Fatalf("expected reloaded proxy 7002 on next acquire, got proxy=%q err=%v", s2.ProxyURL(), err)
	}
}

// ============================================================================
// Feature Area 6: Audit 3.3 — RotateCookies Single-POST Behavior
// ============================================================================

func TestT1_3_3_Primary200WithSetCookieSinglePost(t *testing.T) {
	var postCount, pageGetCount atomic.Int32
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.Path, "RotateCookiesPage") {
				pageGetCount.Add(1)
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
			postCount.Add(1)
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=new_ts; Path=/; Secure"}},
				Body:       io.NopCloser(strings.NewReader(`[]`)),
			}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=old_ts", "", doer)
	res, err := ac.RotateCookies(context.Background())
	if err != nil {
		t.Fatalf("RotateCookies: %v", err)
	}
	if !res.Rotated || res.Status != 200 || postCount.Load() != 1 || pageGetCount.Load() != 0 {
		t.Fatalf("unexpected rotate result: res=%+v post=%d page=%d", res, postCount.Load(), pageGetCount.Load())
	}
}

func TestT1_3_3_Primary200UpdatesDiskAndSession(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "a1.json")
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=disk_sess_ts; Path=/; Secure"}},
				Body:       io.NopCloser(strings.NewReader(`[]`)),
			}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=old_ts", acctPath, doer)
	if _, err := ac.Pool.Acquire(context.Background(), "a1", ac.CookieHeader()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := ac.RotateCookies(context.Background()); err != nil {
		t.Fatalf("RotateCookies: %v", err)
	}
	cf, _, err := cookies.LoadAccountFile(acctPath)
	if err != nil || !strings.Contains(cf.Cookies, "disk_sess_ts") {
		t.Fatalf("expected rotated cookie on disk, got cf=%+v err=%v", cf, err)
	}
	sess := ac.Pool.Existing("a1")
	if sess == nil || !strings.Contains(sess.CookieHeader(), "disk_sess_ts") {
		t.Fatalf("expected rotated cookie queued to Pool.Existing session, got %v", sess)
	}
}

func TestT1_3_3_Fallback658WhenPrimaryInCooldown(t *testing.T) {
	var seen658 atomic.Bool
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, body string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.Path, "RotateCookiesPage") {
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(`<script>init('rotABC1', 1, 300)</script>`)),
				}, nil
			}
			if body == `[658,"rotABC1"]` {
				seen658.Store(true)
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"SIDCC=sidcc_658; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			}
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=ts_1; Path=/; Secure"}},
				Body:       io.NopCloser(strings.NewReader(`[]`)),
			}, nil
		},
	}
	sc, _ := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
	rot := cookies.NewRotator()
	if _, err := rot.RotateAccount(context.Background(), "a1", "SAPISID=sap1; __Secure-1PSIDTS=ts0", sc); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	res2, err := rot.RotateAccount(context.Background(), "a1", "SAPISID=sap1; __Secure-1PSIDTS=ts_1", sc)
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if !seen658.Load() || !strings.Contains(res2.CookieHeader, "SIDCC=sidcc_658") {
		t.Fatalf("expected cooldown fallback 658 to rotate SIDCC, got seen658=%v res2=%+v", seen658.Load(), res2)
	}
}

func TestT1_3_3_FallbackParsesAndClampsNextInterval(t *testing.T) {
	for _, tc := range []struct {
		sec  int
		want time.Duration
	}{
		{30, 60 * time.Second},
		{7200, 3600 * time.Second},
	} {
		t.Run(strconv.Itoa(tc.sec), func(t *testing.T) {
			doer := &e2eGeminiDoer{
				fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
					if strings.Contains(req.URL.Path, "RotateCookiesPage") {
						return &fhttp.Response{
							StatusCode: 200,
							Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`init('id1234', 1, %d)`, tc.sec))),
						}, nil
					}
					return &fhttp.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(""))}, nil
				},
			}
			sc, _ := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
			rot := cookies.NewRotator()
			res, err := rot.RotateAccount(context.Background(), "a1", "SAPISID=sap1", sc)
			if err != nil {
				t.Fatalf("RotateAccount: %v", err)
			}
			if res.NextInterval != tc.want {
				t.Fatalf("sec=%d: got NextInterval=%s, want %s", tc.sec, res.NextInterval, tc.want)
			}
		})
	}
}

func TestT1_3_3_PrimarySendsSubsetAndSentinelBody(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, body string) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=new; Path=/; Secure"}},
				Body:       io.NopCloser(strings.NewReader(`[]`)),
			}, nil
		},
	}
	sc, _ := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
	rot := cookies.NewRotator()
	if _, err := rot.RotateAccount(context.Background(), "a1", "SAPISID=sap1; __Secure-1PSIDTS=ts0", sc); err != nil {
		t.Fatalf("RotateAccount: %v", err)
	}
	reqs := doer.Recorded()
	if len(reqs) != 1 || reqs[0].Body != `[000,"-0000000000000000000"]` {
		t.Fatalf("expected exact sentinel body, got %+v", reqs)
	}
}

func TestT2_3_3_NoDoublePOSTMaskingOn401_403_429_And_200NoDelta(t *testing.T) {
	for _, status := range []int{401, 403, 429, 200} {
		t.Run(fmt.Sprintf("primary_%d", status), func(t *testing.T) {
			var postCount, pageGetCount atomic.Int32
			doer := &e2eGeminiDoer{
				fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
					if strings.Contains(req.URL.Path, "RotateCookiesPage") {
						pageGetCount.Add(1)
						return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`init('id1234', 1, 300)`))}, nil
					}
					n := postCount.Add(1)
					if n > 1 {
						return &fhttp.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader("rate limited"))}, nil
					}
					return &fhttp.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`[]`))}, nil
				},
			}
			ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=ts0", "", doer)
			res, err := ac.RotateCookies(context.Background())
			if err != nil {
				t.Fatalf("RotateCookies: %v", err)
			}
			if res.Status != status {
				t.Fatalf("expected primary status %d preserved (NOT masked by 429), got %d", status, res.Status)
			}
			if postCount.Load() != 1 || pageGetCount.Load() != 0 {
				t.Fatalf("expected single POST without fallback GET/POST, got post=%d page=%d", postCount.Load(), pageGetCount.Load())
			}
			if status == 200 && res.Rotated {
				t.Fatal("expected Rotated=false on 200 without Set-Cookie delta")
			}
		})
	}
}

func TestT2_3_3_Primary500OrNetworkErrorFallsBackToIframe658(t *testing.T) {
	for _, networkErr := range []bool{false, true} {
		t.Run(fmt.Sprintf("neterr_%v", networkErr), func(t *testing.T) {
			doer := &e2eGeminiDoer{
				fn: func(req *fhttp.Request, body string) (*fhttp.Response, error) {
					if strings.Contains(req.URL.Path, "RotateCookiesPage") {
						return &fhttp.Response{
							StatusCode: 200,
							Body:       io.NopCloser(strings.NewReader(`<script>init('rotFALL', 1, 300)</script>`)),
						}, nil
					}
					if body == `[000,"-0000000000000000000"]` {
						if networkErr {
							return nil, errors.New("dial tcp reset")
						}
						return &fhttp.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("err"))}, nil
					}
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{"Set-Cookie": []string{"SIDCC=recovered_sidcc; Path=/; Secure"}},
						Body:       io.NopCloser(strings.NewReader(`[]`)),
					}, nil
				},
			}
			ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSIDTS=ts0", "", doer)
			res, err := ac.RotateCookies(context.Background())
			if err != nil {
				t.Fatalf("RotateCookies fallback failed: %v", err)
			}
			if !res.Rotated || !strings.Contains(res.CookieHeader, "SIDCC=recovered_sidcc") {
				t.Fatalf("expected 658 fallback recovery of SIDCC, got %+v", res)
			}
		})
	}
}

// ============================================================================
// Feature Area 7: Audit 3.5 — Bootstrap Redirects & AvailableModels Retry
// ============================================================================

func TestT1_3_5_Direct200ExtractsTokensAndEmail(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_ok","cfb2h":"bl_ok","FdrFJe":"fsid_ok","oPEP7c":"user@example.com"}`)),
			}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1", "", doer)
	if err := ac.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if ac.Email() != "user@example.com" || ac.at != "at_ok" {
		t.Fatalf("unexpected email=%q at=%q", ac.Email(), ac.at)
	}
}

func TestT1_3_5_SafeSameOriginRedirectsFollowed(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		location string
		wantURL  string
	}{
		{"locale_302", 302, "https://gemini.google.com/app?hl=id", "https://gemini.google.com/app?hl=id"},
		{"regional_307", 307, "https://gemini.google.com/app?gl=ID", "https://gemini.google.com/app?gl=ID"},
		{"relative_302", 302, "/u/1/app", "https://gemini.google.com/u/1/app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &e2eGeminiDoer{
				fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
					if req.URL.String() == "https://gemini.google.com/app" {
						return &fhttp.Response{
							StatusCode: tc.status,
							Header:     fhttp.Header{"Location": []string{tc.location}},
							Body:       io.NopCloser(strings.NewReader("")),
						}, nil
					}
					if req.URL.String() == tc.wantURL {
						return &fhttp.Response{
							StatusCode: 200,
							Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_redir","cfb2h":"bl1","FdrFJe":"fs1","oPEP7c":"r@example.com"}`)),
						}, nil
					}
					return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
				},
			}
			ac := e2eNewAccountClient("a1", "SAPISID=sap1", "", doer)
			if err := ac.Bootstrap(context.Background()); err != nil {
				t.Fatalf("expected safe same-origin redirect %s -> %s to succeed, got: %v", tc.location, tc.wantURL, err)
			}
			if ac.at != "at_redir" {
				t.Fatalf("expected at_redir, got %q", ac.at)
			}
		})
	}
}

func TestT1_3_5_AvailableModels1016Retry200Succeeds(t *testing.T) {
	var otCalls atomic.Int32
	validModelInner := make([]any, 18)
	validModelInner[15] = []any{[]any{"954c0abcba24364", "Pro", "desc", nil, nil, nil, nil, nil, nil, 4, "Pro", "Gemini Pro"}}
	validBatchLine := e2eMarshalJSON([][]any{{"wrb.fr", "otAQ7b", e2eMarshalJSON(validModelInner)}})
	err1016Payload := make([]any, 15)
	err1016Payload[14] = 1016
	err1016Line := e2eMarshalJSON([][]any{{"wrb.fr", "otAQ7b", e2eMarshalJSON(err1016Payload)}})

	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			if strings.HasSuffix(u, "/app") {
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_reboot","cfb2h":"bl1","FdrFJe":"fs1"}`)),
				}, nil
			}
			if strings.Contains(u, "otAQ7b") {
				if otCalls.Add(1) == 1 {
					return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + err1016Line + "\n"))}, nil
				}
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + validBatchLine + "\n"))}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1", "", doer)
	ac.SetTokens("at_stale", "bl1", "fs1")
	res, err := ac.AvailableModels(context.Background(), true)
	if err != nil || len(res.Models) != 1 {
		t.Fatalf("expected 1016 re-bootstrap retry to succeed, got res=%+v err=%v", res, err)
	}
}

func TestT2_3_5_ServiceLoginRedirectReturns401AuthRedirectWithSanitizedLocation(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "RotateCookies") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[]`))}, nil
			}
			return &fhttp.Response{
				StatusCode: 302,
				Header:     fhttp.Header{"Location": []string{"https://accounts.google.com/ServiceLogin?continue=https://gemini.google.com/app&secret=MY_SECRET"}},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSIDTS=ts0", "", doer)
	err := ac.Bootstrap(context.Background())
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 401 || ue.Code != "auth_redirect" {
		t.Fatalf("expected 401 auth_redirect, got %+v (%v)", ue, err)
	}
	if !strings.Contains(err.Error(), "accounts.google.com/ServiceLogin") {
		t.Fatalf("expected sanitized ServiceLogin path in error, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "MY_SECRET") {
		t.Fatalf("expected query secret redacted from auth_redirect error, got %q", err.Error())
	}
}

func TestT2_3_5_NonSignInRedirectNotMisclassifiedAsAuthRedirect(t *testing.T) {
	for _, tc := range []struct {
		name string
		loc  string
	}{
		{"consent_redirect", "https://consent.google.com/ml?continue=x"},
		{"empty_location", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doer := &e2eGeminiDoer{
				fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
					if strings.Contains(req.URL.String(), "RotateCookies") {
						return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[]`))}, nil
					}
					hdr := fhttp.Header{}
					if tc.loc != "" {
						hdr.Set("Location", tc.loc)
					}
					return &fhttp.Response{
						StatusCode: 302,
						Header:     hdr,
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				},
			}
			ac := e2eNewAccountClient("a1", "SAPISID=sap1; __Secure-1PSIDTS=ts0", "", doer)
			err := ac.Bootstrap(context.Background())
			var ue *transport.UpstreamError
			if !errors.As(err, &ue) {
				t.Fatalf("expected UpstreamError, got %v", err)
			}
			if ue.Code == "auth_redirect" {
				t.Fatalf("non-sign-in 302 (%s) misclassified as auth_redirect: %+v", tc.name, ue)
			}
		})
	}
}

func TestT2_3_5_RedirectLoopTerminatesBounded(t *testing.T) {
	var hops atomic.Int32
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "RotateCookies") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[]`))}, nil
			}
			n := hops.Add(1)
			return &fhttp.Response{
				StatusCode: 302,
				Header:     fhttp.Header{"Location": []string{fmt.Sprintf("https://gemini.google.com/app?hop=%d", n)}},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1", "", doer)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ac.Bootstrap(ctx); err == nil {
		t.Fatal("expected bounded error on redirect loop")
	}
	if hops.Load() > 12 {
		t.Fatalf("redirect loop did not terminate within bounded hops: hops=%d", hops.Load())
	}
}

func TestT2_3_5_AvailableModelsRetry429And503AndTOSViolation(t *testing.T) {
	err1016Payload := make([]any, 15)
	err1016Payload[14] = 1016
	err1016Line := e2eMarshalJSON([][]any{{"wrb.fr", "otAQ7b", e2eMarshalJSON(err1016Payload)}})
	for _, retryStatus := range []int{429, 503} {
		t.Run(fmt.Sprintf("retry_%d", retryStatus), func(t *testing.T) {
			var otCalls atomic.Int32
			doer := &e2eGeminiDoer{
				fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
					u := req.URL.String()
					if strings.HasSuffix(u, "/app") {
						return &fhttp.Response{
							StatusCode: 200,
							Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_reboot","cfb2h":"bl1","FdrFJe":"fs1"}`)),
						}, nil
					}
					if strings.Contains(u, "otAQ7b") {
						if otCalls.Add(1) == 1 {
							return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + err1016Line + "\n"))}, nil
						}
						return &fhttp.Response{StatusCode: retryStatus, Body: io.NopCloser(strings.NewReader("busy"))}, nil
					}
					return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
				},
			}
			ac := e2eNewAccountClient("a1", "SAPISID=sap1", "", doer)
			ac.SetTokens("at_init", "bl1", "fs1")
			_, err := ac.AvailableModels(context.Background(), true)
			var ue *transport.UpstreamError
			if !errors.As(err, &ue) || ue.StatusCode != retryStatus {
				t.Fatalf("expected status %d on retry, got %+v (%v)", retryStatus, ue, err)
			}
			if retryStatus == 429 && ue.Code != "rate_limit_error" {
				t.Fatalf("expected rate_limit_error on 429 retry, got %q", ue.Code)
			}
		})
	}

	t.Run("tos_violation_403", func(t *testing.T) {
		doer := &e2eGeminiDoer{
			fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
				if strings.Contains(req.URL.String(), "RotateCookies") {
					return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[]`))}, nil
				}
				return &fhttp.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("TOS_VIOLATION"))}, nil
			},
		}
		ac := e2eNewAccountClient("a1", "SAPISID=sap1", "", doer)
		err := ac.Bootstrap(context.Background())
		var ue *transport.UpstreamError
		if !errors.As(err, &ue) || ue.StatusCode != 403 || ue.Code != "account_banned" {
			t.Fatalf("expected 403 account_banned on TOS_VIOLATION, got %+v (%v)", ue, err)
		}
	})
}

// ============================================================================
// Feature Area 8: Audit 3.6 — Secondary Account (authUser > 0) Isolation
// ============================================================================

func TestT1_3_6_AuthUser2BootstrapsViaU2App(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "u2.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:     "u2",
		Cookies:  "SAPISID=sap2; __Secure-1PSID=psid2; __Secure-1PSIDTS=ts2",
		AuthUser: 2,
	}); err != nil {
		t.Fatalf("SaveCredentialFile: %v", err)
	}
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if req.URL.Path == "/u/2/app" {
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_u2","cfb2h":"bl2","FdrFJe":"fs2","oPEP7c":"u2@example.com"}`)),
				}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 4})
	ac, err := LoadAccountFromFile(acctPath, pool, cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile: %v", err)
	}
	if err := ac.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	reqs := doer.Recorded()
	if len(reqs) != 1 || reqs[0].URL != "https://gemini.google.com/u/2/app" {
		t.Fatalf("expected single request to /u/2/app, got %+v", reqs)
	}
}

func TestT1_3_6_AuthUser2StreamGenerateIncludesAuthUserParam(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "StreamGenerate") {
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(")]}'\n\n" + e2eBuildStreamLine("c2", "r2", "rc2", "u2 reply", "", true, "ctx2") + "\n")),
				}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	ac := e2eNewAccountClient("u2", "SAPISID=sap2", "", doer)
	ac.authUser = 2
	ac.SetTokens("at2", "bl2", "fs2")
	if _, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, nil); err != nil {
		t.Fatalf("StreamGenerateLive: %v", err)
	}
	reqs := doer.Recorded()
	if len(reqs) != 1 || reqs[0].Query.Get("authuser") != "2" {
		t.Fatalf("expected authuser=2 in StreamGenerate query, got %+v", reqs)
	}
}

func TestT1_3_6_AuthUser2GetQuotaIncludesAuthUserOnBatchAndGraphQL(t *testing.T) {
	qpBody := ")]}'\n\n" + e2eMarshalJSON([][]any{{"wrb.fr", "qpEbW", e2eMarshalJSON([]any{[]any{[]any{[]any{1, 11}, 0, 0.2, []any{1900000000}, 2400, 1920}}})}}) + "\n"
	gqlBody := ")]}'\n" + e2eMarshalJSON([]any{map[string]any{
		"data": map[string]any{
			"viewer": map[string]any{
				"rpcs": map[string]any{
					"getUsageInfo": map[string]any{
						"pools": []any{
							map[string]any{"poolType": "GXU_FIVE_HOURLY", "percentageUsed": 0.2, "gxu": map[string]any{"windowRefreshesAt": "2030-01-01T00:00:00Z"}},
							map[string]any{"poolType": "GXU_WEEKLY", "percentageUsed": 0.1, "gxu": map[string]any{"windowRefreshesAt": "2030-01-07T00:00:00Z"}},
						},
					},
				},
			},
		},
	}})

	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			if strings.Contains(u, "batchexecute") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(qpBody))}, nil
			}
			if strings.Contains(u, "GEMINI_WEB_GRAPHQL") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(gqlBody))}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	ac := e2eNewAccountClient("u2", "SAPISID=sap2", "", doer)
	ac.authUser = 2
	ac.SetTokens("at2", "bl2", "fs2")
	buckets := ac.GetQuota(context.Background())
	if len(buckets) == 0 {
		t.Fatal("expected non-empty quota buckets")
	}
	var sawBatchAuth, sawGQLAuth bool
	for _, r := range doer.Recorded() {
		if strings.Contains(r.URL, "batchexecute") && r.Query.Get("authuser") == "2" {
			sawBatchAuth = true
		}
		if strings.Contains(r.URL, "GEMINI_WEB_GRAPHQL") && r.Query.Get("authuser") == "2" {
			sawGQLAuth = true
		}
	}
	if !sawBatchAuth || !sawGQLAuth {
		t.Fatalf("expected authuser=2 on both batchexecute (%v) and GraphQL (%v), reqs=%+v", sawBatchAuth, sawGQLAuth, doer.Recorded())
	}
}

func TestT1_3_6_AuthUser2VideoPollIncludesAuthUserAndU2SourcePath(t *testing.T) {
	t.Parallel()
	streamInner := e2eMarshalJSON([]any{
		nil, []any{"c_vid_u2", "r_1"}, nil, nil, nil,
		map[string]any{"7": []any{"Generating", []any{"video_generation"}}},
	})
	streamLine := e2eMarshalJSON([][]any{{"wrb.fr", nil, streamInner}})

	c12 := map[string]any{
		"60": []any{
			[]any{nil, nil, "vid.mp4", nil, nil, nil, nil, []any{"", "https://contribution.usercontent.google.com/v2", ""}, nil, nil, nil, "video/mp4"},
		},
	}
	pollInner := e2eMarshalJSON([]any{[]any{[]any{[]any{"c_vid_u2", "r_1"}, nil, nil, []any{[]any{[]any{"rc_1", []any{""}, nil, nil, nil, nil, nil, nil, []any{2}, map[string]any{"13": c12}}}}}}})
	pollBody := ")]}'\n\n" + e2eMarshalJSON([][]any{{"wrb.fr", "hNvQHb", pollInner}}) + "\n"

	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			if strings.Contains(u, "StreamGenerate") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + streamLine + "\n"))}, nil
			}
			if strings.Contains(u, "hNvQHb") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(pollBody))}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	ac := e2eNewAccountClient("u2", "SAPISID=sap2", "", doer)
	ac.authUser = 2
	ac.SetTokens("at2", "bl2", "fs2")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := ac.StreamGenerateLive(ctx, "generate a video", StreamOptions{}, nil)
	if err != nil || res.Empty {
		t.Fatalf("StreamGenerateLive video poll failed: res=%+v err=%v", res, err)
	}
	var pollReq *e2eRecordedReq
	for _, r := range doer.Recorded() {
		if strings.Contains(r.URL, "hNvQHb") {
			cp := r
			pollReq = &cp
		}
	}
	if pollReq == nil || pollReq.Query.Get("authuser") != "2" || pollReq.Query.Get("source-path") != "/u/2/app/vid_u2" {
		t.Fatalf("expected authuser=2 and source-path=/u/2/app/vid_u2 on hNvQHb poll, got %+v", pollReq)
	}
}

func TestT1_3_6_SaveCredentialFilePreservesAuthUser(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "u2.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:     "u2",
		Cookies:  "SAPISID=sap2",
		AuthUser: 2,
	}); err != nil {
		t.Fatalf("SaveCredentialFile: %v", err)
	}
	cf, _, err := cookies.LoadAccountFile(acctPath)
	if err != nil || cf.AuthUser != 2 {
		t.Fatalf("expected AuthUser=2 preserved, got %+v err=%v", cf, err)
	}
}

func TestT2_3_6_AuthUser2StaleCookieRotatesWithinSlot(t *testing.T) {
	var rotated atomic.Bool
	var u0Calls atomic.Int32
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			switch req.URL.Path {
			case "/app":
				u0Calls.Add(1)
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_u0","cfb2h":"bl0","FdrFJe":"fs0"}`)),
				}, nil
			case "/u/2/app":
				if !rotated.Load() {
					return &fhttp.Response{
						StatusCode: 302,
						Header:     fhttp.Header{"Location": []string{"https://gemini.google.com/app"}},
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				}
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_u2_ok","cfb2h":"bl2","FdrFJe":"fs2"}`)),
				}, nil
			case "/RotateCookies":
				rotated.Store(true)
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=fresh_u2; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}
	ac := e2eNewAccountClient("u2", "SAPISID=sap2; __Secure-1PSID=psid2; __Secure-1PSIDTS=stale_u2", "", doer)
	ac.authUser = 2
	if err := ac.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if u0Calls.Load() != 0 || ac.authUser != 2 || ac.at != "at_u2_ok" {
		t.Fatalf("expected RotateCookies to heal /u/2/app without hijacking to /app: u0Calls=%d authUser=%d at=%q", u0Calls.Load(), ac.authUser, ac.at)
	}
}

func TestT2_3_6_AuthUser2NeverFallsBackToU0(t *testing.T) {
	var rotated atomic.Bool
	var primaryCalls atomic.Int32
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			switch req.URL.Path {
			case "/u/2/app":
				return &fhttp.Response{
					StatusCode: 302,
					Header:     fhttp.Header{"Location": []string{"https://gemini.google.com/app"}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			case "/RotateCookies":
				rotated.Store(true)
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=fresh; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			case "/app":
				primaryCalls.Add(1)
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_fallback","cfb2h":"bl0","FdrFJe":"fs0"}`)),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}
	ac := e2eNewAccountClient("u2", "SAPISID=sap2; __Secure-1PSID=psid2; __Secure-1PSIDTS=stale", "", doer)
	ac.authUser = 2
	if err := ac.Bootstrap(context.Background()); err == nil {
		t.Fatal("expected an unresolved secondary slot to fail bootstrap")
	}
	if !rotated.Load() || primaryCalls.Load() != 0 || ac.at != "" || ac.authUser != 2 {
		t.Fatalf("expected retry confined to secondary slot: rotated=%v primaryCalls=%d authUser=%d", rotated.Load(), primaryCalls.Load(), ac.authUser)
	}
}

func TestT2_3_6_SaveCredentialFileZeroAuthUserDoesNotWipeExistingAuthUser(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "u2.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:     "u2",
		Cookies:  "SAPISID=old",
		AuthUser: 2,
	}); err != nil {
		t.Fatalf("SaveCredentialFile 1: %v", err)
	}
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:    "u2",
		Cookies: "SAPISID=new",
	}); err != nil {
		t.Fatalf("SaveCredentialFile 2: %v", err)
	}
	cf, _, err := cookies.LoadAccountFile(acctPath)
	if err != nil {
		t.Fatalf("LoadAccountFile: %v", err)
	}
	if cf.AuthUser != 2 {
		t.Fatalf("expected existing AuthUser=2 preserved when saving AuthUser=0, got %d", cf.AuthUser)
	}
}

func TestT2_3_6_TwoAccountsU0AndU2SharePoolWithoutCrossContamination(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "StreamGenerate") {
				au := req.URL.Query().Get("authuser")
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(")]}'\n\n" + e2eBuildStreamLine("c_"+au, "r", "rc", "ok_"+au, "", true, "ctx") + "\n")),
				}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 4})
	rot := cookies.NewRotator()
	u0 := NewAccountClient("u0", "SAPISID=sap_u0", "", pool, rot)
	u0.SetTokens("at_u0", "bl0", "fs0")
	u2 := NewAccountClient("u2", "SAPISID=sap_u2", "", pool, rot)
	u2.authUser = 2
	u2.SetTokens("at_u2", "bl2", "fs2")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := u0.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, nil); err != nil {
			t.Errorf("u0 stream: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := u2.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, nil); err != nil {
			t.Errorf("u2 stream: %v", err)
		}
	}()
	wg.Wait()

	for _, r := range doer.Recorded() {
		if strings.Contains(r.Cookie, "sap_u0") && r.Query.Get("authuser") != "" {
			t.Fatalf("u0 sent authuser: %+v", r)
		}
		if strings.Contains(r.Cookie, "sap_u2") && r.Query.Get("authuser") != "2" {
			t.Fatalf("u2 missing authuser=2: %+v", r)
		}
	}
}

func TestT2_3_6_AuthUser2GraphQLQuotaRespectsAuthUserWhenBatchFails(t *testing.T) {
	gqlBody := ")]}'\n" + e2eMarshalJSON([]any{map[string]any{
		"data": map[string]any{
			"viewer": map[string]any{
				"rpcs": map[string]any{
					"getUsageInfo": map[string]any{
						"pools": []any{
							map[string]any{"poolType": "GXU_FIVE_HOURLY", "percentageUsed": 0.5, "gxu": map[string]any{"windowRefreshesAt": "2030-01-01T00:00:00Z"}},
						},
					},
				},
			},
		},
	}})
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "batchexecute") {
				return &fhttp.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
			return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(gqlBody))}, nil
		},
	}
	ac := e2eNewAccountClient("u2", "SAPISID=sap2", "", doer)
	ac.authUser = 2
	ac.SetTokens("at2", "bl2", "fs2")
	buckets := ac.GetQuota(context.Background())
	if len(buckets) == 0 {
		t.Fatal("expected GraphQL fallback quota buckets")
	}
	var sawGQLAuth2 bool
	for _, r := range doer.Recorded() {
		if strings.Contains(r.URL, "GEMINI_WEB_GRAPHQL") && r.Query.Get("authuser") == "2" {
			sawGQLAuth2 = true
		}
	}
	if !sawGQLAuth2 {
		t.Fatalf("expected GraphQL fallback to carry authuser=2, got %+v", doer.Recorded())
	}
}

// ============================================================================
// Feature Area 9: Audit 4.1 — FirstByteTimeout, Cancel Isolation, Session.Close
// ============================================================================

func TestT1_4_1_HeadersFastThenThinkingPausePastFirstByteTimeoutSucceeds(t *testing.T) {
	line := e2eBuildStreamLine("c1", "r1", "rc1", "deep thought answer", "thought", true, "ctx1")
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{},
				Body: &e2eChunkedBody{chunks: []e2eChunk{
					{data: []byte(")]}'\n\n[[\"wrb.fr\",null,null,null,null,[3]]]\n"), delay: 10 * time.Millisecond},
					{data: []byte(line + "\n"), delay: 95 * time.Millisecond}, // > 2x FirstByteTimeout (45ms)
				}},
			}, nil
		},
	}
	sc, err := transport.NewStealthClient(transport.ClientConfig{
		CustomDoer:       doer,
		Timeout:          5 * time.Second,
		FirstByteTimeout: 45 * time.Millisecond,
		StallTimeout:     300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewStealthClient: %v", err)
	}
	var chunks []string
	_, err = sc.Do(context.Background(), transport.RequestOptions{
		Method: "POST",
		URL:    "https://gemini.google.com/StreamGenerate",
		OnChunk: func(c string) error {
			chunks = append(chunks, c)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("expected stream whose HTTP 200 headers arrived immediately to stop fbTimer and survive 95ms thinking pause (> 45ms FirstByteTimeout), got: %v", err)
	}
	if !strings.Contains(strings.Join(chunks, ""), "deep thought answer") {
		t.Fatalf("missing answer chunk: %v", chunks)
	}
}

func TestT1_4_1_MultiChunkReasoningStreamLongerThanFirstByteTimeout(t *testing.T) {
	chunks := []e2eChunk{{data: []byte(")]}'\n\n"), delay: 5 * time.Millisecond}}
	for i := range 4 {
		line := e2eBuildStreamLine("c1", "r1", "rc1", "", fmt.Sprintf("step %d", i), i == 3, "")
		chunks = append(chunks, e2eChunk{data: []byte(line + "\n"), delay: 40 * time.Millisecond})
	}
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: &e2eChunkedBody{chunks: chunks}}, nil
		},
	}
	sc, _ := transport.NewStealthClient(transport.ClientConfig{
		CustomDoer:       doer,
		Timeout:          5 * time.Second,
		FirstByteTimeout: 60 * time.Millisecond,
		StallTimeout:     120 * time.Millisecond,
	})
	_, err := sc.Do(context.Background(), transport.RequestOptions{
		Method:  "POST",
		URL:     "https://gemini.google.com/StreamGenerate",
		OnChunk: func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("expected multi-chunk reasoning stream (> FirstByteTimeout) to succeed, got %v", err)
	}
}

func TestT1_4_1_NonStreamingFastResponseSucceeds(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("fast"))}, nil
		},
	}
	sc, _ := transport.NewStealthClient(transport.ClientConfig{
		CustomDoer:       doer,
		FirstByteTimeout: 200 * time.Millisecond,
	})
	res, err := sc.Do(context.Background(), transport.RequestOptions{Method: "GET", URL: "https://gemini.google.com/app"})
	if err != nil || res.Body != "fast" || res.TTFBMs < 0 {
		t.Fatalf("unexpected non-streaming response: res=%+v err=%v", res, err)
	}
}

func TestT1_4_1_SessionCloseReleasesIdleWithoutError(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		},
	}
	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 4})
	sess, err := pool.Acquire(context.Background(), "a1", "SAPISID=1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	sess.Close()
	if pool.Existing("a1") != nil {
		t.Fatal("expected Pool.Existing to return nil after Close")
	}
	if doer.closeIdleCalls.Load() == 0 {
		t.Fatal("expected CloseIdleConnections to be invoked")
	}
}

func TestT1_4_1_PoolResetCleansAllSessions(t *testing.T) {
	pool := transport.NewPool(transport.PoolOptions{MaxSessions: 4})
	if _, err := pool.Acquire(context.Background(), "a1", "SAPISID=1"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	pool.Reset()
	if st := pool.Status(); len(st) != 0 {
		t.Fatalf("expected empty Pool.Status() after Reset, got %+v", st)
	}
}

func TestT2_4_1_SlowHeadersExceedingFirstByteTimeoutReturns504ConnectTimeout(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			select {
			case <-time.After(150 * time.Millisecond):
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("late"))}, nil
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		},
	}
	sc, _ := transport.NewStealthClient(transport.ClientConfig{
		CustomDoer:       doer,
		Timeout:          5 * time.Second,
		FirstByteTimeout: 40 * time.Millisecond,
		StallTimeout:     300 * time.Millisecond,
	})
	_, err := sc.Do(context.Background(), transport.RequestOptions{
		Method: "POST",
		URL:    "https://gemini.google.com/StreamGenerate",
		OnChunk: func(string) error {
			return nil
		},
	})
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 504 || ue.Code != "upstream_connect_timeout" {
		t.Fatalf("expected 504 upstream_connect_timeout when headers stall, got %+v (%v)", ue, err)
	}
}

func TestT2_4_1_HeadersFastThenSilentBodyExceedsStallTimeoutReturns504Stalled(t *testing.T) {
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{},
				Body: &e2eChunkedBody{chunks: []e2eChunk{
					{data: []byte(")]}'\n\n"), delay: 5 * time.Millisecond},
					{data: []byte("late"), delay: 200 * time.Millisecond, ctx: req.Context()},
				}},
			}, nil
		},
	}
	sc, _ := transport.NewStealthClient(transport.ClientConfig{
		CustomDoer:       doer,
		Timeout:          5 * time.Second,
		FirstByteTimeout: 40 * time.Millisecond,
		StallTimeout:     65 * time.Millisecond,
	})
	_, err := sc.Do(context.Background(), transport.RequestOptions{
		Method: "POST",
		URL:    "https://gemini.google.com/StreamGenerate",
		OnChunk: func(string) error {
			return nil
		},
	})
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 504 || ue.Code != "upstream_stalled" {
		t.Fatalf("expected 504 upstream_stalled (NOT upstream_connect_timeout) after headers arrived, got %+v (%v)", ue, err)
	}
}

func TestT2_4_1_ClientCancelDuringHeaderWaitDoesNotResetConnections(t *testing.T) {
	entered := make(chan struct{})
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			close(entered)
			<-req.Context().Done()
			return nil, req.Context().Err()
		},
	}
	sc, _ := transport.NewStealthClient(transport.ClientConfig{
		CustomDoer:       doer,
		FirstByteTimeout: 2 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-entered
		cancel()
	}()
	_, err := sc.Do(ctx, transport.RequestOptions{Method: "GET", URL: "https://gemini.google.com/app"})
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 499 || ue.Code != "client_cancelled" {
		t.Fatalf("expected 499 client_cancelled, got %+v (%v)", ue, err)
	}
	if got := doer.closeIdleCalls.Load(); got != 0 {
		t.Fatalf("client cancel MUST NOT call ResetConnections/CloseIdleConnections, got %d calls", got)
	}
}

func TestT2_4_1_ClientCancelOnStream1DoesNotDisruptPeerStream2(t *testing.T) {
	stream1Ready := make(chan struct{})
	stream2Line := e2eBuildStreamLine("c2", "r2", "rc2", "peer ok", "", true, "ctx2")

	var reqNum atomic.Int32
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "StreamGenerate") {
				n := reqNum.Add(1)
				if n == 1 {
					close(stream1Ready)
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{},
						Body: &e2eChunkedBody{chunks: []e2eChunk{
							{data: []byte(")]}'\n\n"), delay: 2 * time.Millisecond},
							{data: []byte("blocked"), delay: 2 * time.Second, ctx: req.Context()},
						}},
					}, nil
				}
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body: &e2eChunkedBody{chunks: []e2eChunk{
						{data: []byte(")]}'\n\n"), delay: 2 * time.Millisecond},
						{data: []byte(stream2Line + "\n"), delay: 60 * time.Millisecond},
					}},
				}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	ac := e2eNewAccountClient("a1", "SAPISID=sap1", "", doer)
	ac.SetTokens("at1", "bl1", "fs1")

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()

	var wg sync.WaitGroup
	var err1, err2 error
	var text2 string

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err1 = ac.StreamGenerateLive(ctx1, "p1", StreamOptions{}, nil)
	}()
	<-stream1Ready
	go func() {
		defer wg.Done()
		_, err2 = ac.StreamGenerateLive(context.Background(), "p2", StreamOptions{}, func(u *LineParseResult) error {
			if len(u.Frames) > 0 {
				text2 = u.Frames[0].Text
			}
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond)
	cancel1()
	wg.Wait()

	var ue1 *transport.UpstreamError
	if !errors.As(err1, &ue1) || ue1.StatusCode != 499 {
		t.Fatalf("expected stream 1 499 cancel, got %v", err1)
	}
	if err2 != nil || text2 != "peer ok" {
		t.Fatalf("expected peer stream 2 to succeed, got text=%q err=%v", text2, err2)
	}
	if doer.closeIdleCalls.Load() != 0 {
		t.Fatalf("expected 0 ResetConnections/CloseIdleConnections calls on client cancel, got %d", doer.closeIdleCalls.Load())
	}
}

func TestT2_4_1_SessionCloseDoesNotAbortActiveInFlightStream(t *testing.T) {
	firstChunkSent := make(chan struct{})
	releaseSecondChunk := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("chunk1\n"))
		if fl != nil {
			fl.Flush()
		}
		select {
		case <-releaseSecondChunk:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("chunk2\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	sc, err := transport.NewStealthClient(transport.ClientConfig{
		Timeout:          10 * time.Second,
		FirstByteTimeout: 2 * time.Second,
		StallTimeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewStealthClient: %v", err)
	}
	sess := transport.NewSession("live", sc, 1, "SAPISID=1", 4, 2*time.Second)

	var bodyOut strings.Builder
	var mu sync.Mutex
	var streamErr error
	var wg sync.WaitGroup
	var notified atomic.Bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		streamErr = sess.RunStream(context.Background(), func(string) error {
			_, dErr := sc.Do(context.Background(), transport.RequestOptions{
				Method: "GET",
				URL:    srv.URL,
				OnChunk: func(ch string) error {
					mu.Lock()
					bodyOut.WriteString(ch)
					mu.Unlock()
					if notified.CompareAndSwap(false, true) {
						close(firstChunkSent)
					}
					return nil
				},
			})
			return dErr
		})
	}()

	select {
	case <-firstChunkSent:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first chunk")
	}
	sess.Close()
	close(releaseSecondChunk)
	wg.Wait()

	if streamErr != nil {
		t.Fatalf("expected active in-flight stream to complete after Session.Close, got %v", streamErr)
	}
	if !strings.Contains(bodyOut.String(), "chunk1") || !strings.Contains(bodyOut.String(), "chunk2") {
		t.Fatalf("expected both chunks received, got %q", bodyOut.String())
	}
}

// ============================================================================
// Feature Area 10: Audit 4.4 — Async Video Polling
// ============================================================================

func e2eVideoPollBody(cid, videoURL, text string, done bool) string {
	var c12 map[string]any
	if videoURL != "" {
		c12 = map[string]any{
			"60": []any{
				[]any{nil, nil, "out.mp4", nil, nil, nil, nil, []any{"", videoURL, ""}, nil, nil, nil, "video/mp4"},
			},
		}
	}
	status := 1
	if done {
		status = 2
	}
	cand := []any{"rc_v1", []any{text}, nil, nil, nil, nil, nil, nil, []any{status}}
	if c12 != nil {
		cand = append(cand, map[string]any{"13": c12})
	}
	inner := e2eMarshalJSON([]any{[]any{[]any{[]any{cid, "r_v1"}, nil, nil, []any{[]any{cand}}}}})
	return ")]}'\n\n" + e2eMarshalJSON([][]any{{"wrb.fr", "hNvQHb", inner}}) + "\n"
}

func TestT1_4_4_SingleFrameVideoToolTriggersPollAndReturnsVideo(t *testing.T) {
	t.Parallel()
	streamInner := e2eMarshalJSON([]any{
		nil, []any{"c_vid1", "r_1"}, nil, nil, nil,
		map[string]any{"7": []any{"Generating", []any{"video_generation"}}},
	})
	streamLine := e2eMarshalJSON([][]any{{"wrb.fr", nil, streamInner}})
	pollBody := e2eVideoPollBody("c_vid1", "https://contribution.usercontent.google.com/v1.mp4", "", true)

	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "StreamGenerate") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + streamLine + "\n"))}, nil
			}
			if strings.Contains(req.URL.String(), "hNvQHb") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(pollBody))}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	ac := e2eNewAccountClient("v1", "SAPISID=sap1", "", doer)
	ac.SetTokens("at1", "bl1", "fs1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var gotVideoURL string
	res, err := ac.StreamGenerateLive(ctx, "generate a video", StreamOptions{}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 && len(u.Frames[0].GeneratedVideos) > 0 {
			gotVideoURL = u.Frames[0].GeneratedVideos[0].URL
		}
		return nil
	})
	if err != nil || res.Empty || gotVideoURL != "https://contribution.usercontent.google.com/v1.mp4" {
		t.Fatalf("unexpected video poll result: res=%+v url=%q err=%v", res, gotVideoURL, err)
	}
}

func TestT1_4_4_MultiPollStillGeneratingThenReady(t *testing.T) {
	t.Parallel()
	streamInner := e2eMarshalJSON([]any{
		nil, []any{"c_vid2", "r_1"}, nil, nil, nil,
		map[string]any{"7": []any{"Generating", []any{"video_generation"}}},
	})
	streamLine := e2eMarshalJSON([][]any{{"wrb.fr", nil, streamInner}})

	var polls atomic.Int32
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "StreamGenerate") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + streamLine + "\n"))}, nil
			}
			if strings.Contains(req.URL.String(), "hNvQHb") {
				if polls.Add(1) == 1 {
					return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(e2eVideoPollBody("c_vid2", "", "Generating your video...", false)))}, nil
				}
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(e2eVideoPollBody("c_vid2", "https://contribution.usercontent.google.com/v2.mp4", "", true)))}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	ac := e2eNewAccountClient("v2", "SAPISID=sap1", "", doer)
	ac.SetTokens("at1", "bl1", "fs1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var gotURL string
	res, err := ac.StreamGenerateLive(ctx, "generate a video", StreamOptions{}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 && len(u.Frames[0].GeneratedVideos) > 0 {
			gotURL = u.Frames[0].GeneratedVideos[0].URL
		}
		return nil
	})
	if err != nil || res.Empty || polls.Load() != 2 || gotURL == "" {
		t.Fatalf("expected poll 2 completion, got polls=%d url=%q res=%+v err=%v", polls.Load(), gotURL, res, err)
	}
}

func TestT1_4_4_TemporaryTrueWithVideoPromptPollsDirectly(t *testing.T) {
	t.Parallel()
	streamInner := e2eMarshalJSON([]any{
		nil, []any{"c_vid3", "r_1"}, nil, nil, nil,
		map[string]any{"7": []any{"Generating", []any{"video_generation"}}},
	})
	streamLine := e2eMarshalJSON([][]any{{"wrb.fr", nil, streamInner}})
	pollBody := e2eVideoPollBody("c_vid3", "https://contribution.usercontent.google.com/v3.mp4", "", true)

	var streamCalls atomic.Int32
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "StreamGenerate") {
				streamCalls.Add(1)
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + streamLine + "\n"))}, nil
			}
			return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(pollBody))}, nil
		},
	}
	ac := e2eNewAccountClient("v3", "SAPISID=sap1", "", doer)
	ac.SetTokens("at1", "bl1", "fs1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := ac.StreamGenerateLive(ctx, "generate a video of a cat", StreamOptions{Temporary: true}, nil)
	if err != nil || streamCalls.Load() != 1 {
		t.Fatalf("expected single StreamGenerate + direct poll, got streamCalls=%d err=%v", streamCalls.Load(), err)
	}
}

func TestT1_4_4_PollReturnsNonGeneratingTextAnswer(t *testing.T) {
	t.Parallel()
	streamInner := e2eMarshalJSON([]any{
		nil, []any{"c_vid4", "r_1"}, nil, nil, nil,
		map[string]any{"7": []any{"Generating", []any{"video_generation"}}},
	})
	streamLine := e2eMarshalJSON([][]any{{"wrb.fr", nil, streamInner}})
	pollBody := e2eVideoPollBody("c_vid4", "", "Unable to generate this policy-restricted video.", true)

	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "StreamGenerate") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + streamLine + "\n"))}, nil
			}
			return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(pollBody))}, nil
		},
	}
	ac := e2eNewAccountClient("v4", "SAPISID=sap1", "", doer)
	ac.SetTokens("at1", "bl1", "fs1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var text string
	res, err := ac.StreamGenerateLive(ctx, "generate a video", StreamOptions{}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 {
			text = u.Frames[0].Text
		}
		return nil
	})
	if err != nil || res.Empty || !strings.Contains(text, "Unable to generate") {
		t.Fatalf("expected non-generating text answer from poll, got res=%+v text=%q err=%v", res, text, err)
	}
}

func TestT1_4_4_TemporaryFalseCleansNoDeleteCall(t *testing.T) {
	t.Parallel()
	streamInner := e2eMarshalJSON([]any{
		nil, []any{"c_vid5", "r_1"}, nil, nil, nil,
		map[string]any{"7": []any{"Generating", []any{"video_generation"}}},
	})
	streamLine := e2eMarshalJSON([][]any{{"wrb.fr", nil, streamInner}})
	pollBody := e2eVideoPollBody("c_vid5", "https://contribution.usercontent.google.com/v5.mp4", "", true)

	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "StreamGenerate") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + streamLine + "\n"))}, nil
			}
			return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(pollBody))}, nil
		},
	}
	ac := e2eNewAccountClient("v5", "SAPISID=sap1", "", doer)
	ac.SetTokens("at1", "bl1", "fs1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := ac.StreamGenerateLive(ctx, "generate a video", StreamOptions{Temporary: false}, nil)
	if err != nil || res.Empty {
		t.Fatalf("StreamGenerateLive failed: res=%+v err=%v", res, err)
	}
	for _, r := range doer.Recorded() {
		if strings.Contains(r.URL, "GzXR5e") {
			t.Fatalf("unexpected deleteConversation call for Temporary=false: %+v", r)
		}
	}
}

func TestT2_4_4_MultiFrameVideoToolInFrame1CIDInFrame2(t *testing.T) {
	t.Parallel()
	frame1 := e2eMarshalJSON([][]any{{"wrb.fr", nil, e2eMarshalJSON([]any{
		nil, nil, nil, nil, nil,
		map[string]any{"7": []any{"Generating", []any{"video_generation"}}},
	})}})
	frame2 := e2eMarshalJSON([][]any{{"wrb.fr", nil, e2eMarshalJSON([]any{
		nil, []any{"c_split2", "r_split2"},
	})}})
	pollBody := e2eVideoPollBody("c_split2", "https://contribution.usercontent.google.com/split.mp4", "", true)

	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "StreamGenerate") {
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(")]}'\n\n" + frame1 + "\n" + frame2 + "\n")),
				}, nil
			}
			if strings.Contains(req.URL.String(), "hNvQHb") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(pollBody))}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	ac := e2eNewAccountClient("vsplit", "SAPISID=sap1", "", doer)
	ac.SetTokens("at1", "bl1", "fs1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var gotURL string
	res, err := ac.StreamGenerateLive(ctx, "generate a video", StreamOptions{}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 && len(u.Frames[0].GeneratedVideos) > 0 {
			gotURL = u.Frames[0].GeneratedVideos[0].URL
		}
		return nil
	})
	if err != nil || res.Empty || gotURL != "https://contribution.usercontent.google.com/split.mp4" {
		t.Fatalf("expected multi-frame VideoToolActive + split CID to trigger hNvQHb poll, got res=%+v url=%q err=%v", res, gotURL, err)
	}
}

func TestT2_4_4_UsedTempWithDisableToolsRetriesAsNonTemporary(t *testing.T) {
	t.Parallel()
	streamInner := e2eMarshalJSON([]any{
		nil, []any{"c_utemp", "r_1"}, nil, nil, nil,
		map[string]any{"7": []any{"Generating", []any{"video_generation"}}},
	})
	streamLine := e2eMarshalJSON([][]any{{"wrb.fr", nil, streamInner}})
	pollBody := e2eVideoPollBody("c_utemp", "https://contribution.usercontent.google.com/utemp.mp4", "", true)

	var streamInner45 []any
	var mu sync.Mutex
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, body string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "StreamGenerate") {
				form, _ := url.ParseQuery(body)
				var freq []any
				_ = json.Unmarshal([]byte(form.Get("f.req")), &freq)
				if len(freq) > 1 {
					if innerStr, ok := freq[1].(string); ok {
						var inner []any
						_ = json.Unmarshal([]byte(innerStr), &inner)
						if len(inner) > 45 {
							mu.Lock()
							streamInner45 = append(streamInner45, inner[45])
							mu.Unlock()
						}
					}
				}
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + streamLine + "\n"))}, nil
			}
			if strings.Contains(req.URL.String(), "hNvQHb") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(pollBody))}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	ac := e2eNewAccountClient("vutemp", "SAPISID=sap1", "", doer)
	ac.SetTokens("at1", "bl1", "fs1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := ac.StreamGenerateLive(ctx, "generate a video", StreamOptions{Temporary: true, DisableTools: true}, nil)
	if err != nil || res.Empty {
		t.Fatalf("StreamGenerateLive failed: res=%+v err=%v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(streamInner45) != 2 || streamInner45[0] == nil || streamInner45[1] != nil {
		t.Fatalf("expected 1st StreamGenerate with inner[45]=1 and retry with inner[45]=nil, got %+v", streamInner45)
	}
}

func TestT2_4_4_LiveCookieHeaderPickedUpDuringPoll(t *testing.T) {
	t.Parallel()
	streamInner := e2eMarshalJSON([]any{
		nil, []any{"c_liveck", "r_1"}, nil, nil, nil,
		map[string]any{"7": []any{"Generating", []any{"video_generation"}}},
	})
	streamLine := e2eMarshalJSON([][]any{{"wrb.fr", nil, streamInner}})
	pollBody := e2eVideoPollBody("c_liveck", "https://contribution.usercontent.google.com/live.mp4", "", true)

	streamSent := make(chan struct{})
	var pollCookie atomic.Value
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			if strings.Contains(u, "StreamGenerate") {
				close(streamSent)
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + streamLine + "\n"))}, nil
			}
			if strings.Contains(u, "RotateCookies") {
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=rotated_during_poll; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			}
			if strings.Contains(u, "hNvQHb") {
				pollCookie.Store(e2eHeaderGet(req.Header, "cookie"))
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(pollBody))}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	ac := e2eNewAccountClient("vlive", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=init_ts", "", doer)
	ac.SetTokens("at1", "bl1", "fs1")

	go func() {
		<-streamSent
		time.Sleep(100 * time.Millisecond)
		_, _ = ac.RotateCookies(context.Background())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := ac.StreamGenerateLive(ctx, "generate a video", StreamOptions{}, nil); err != nil {
		t.Fatalf("StreamGenerateLive: %v", err)
	}
	got, _ := pollCookie.Load().(string)
	if !strings.Contains(got, "rotated_during_poll") {
		t.Fatalf("expected hNvQHb poll to pick up rotated_during_poll from live ac.CookieHeader(), got %q", got)
	}
}

func TestT2_4_4_AuthUser2PropagatedToPoll(t *testing.T) {
	t.Parallel()
	streamInner := e2eMarshalJSON([]any{
		nil, []any{"c_vid42", "r_1"}, nil, nil, nil,
		map[string]any{"7": []any{"Generating", []any{"video_generation"}}},
	})
	streamLine := e2eMarshalJSON([][]any{{"wrb.fr", nil, streamInner}})
	pollBody := e2eVideoPollBody("c_vid42", "https://contribution.usercontent.google.com/v42.mp4", "", true)

	var gotAU, gotSP atomic.Value
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "StreamGenerate") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + streamLine + "\n"))}, nil
			}
			if strings.Contains(req.URL.String(), "hNvQHb") {
				gotAU.Store(req.URL.Query().Get("authuser"))
				gotSP.Store(req.URL.Query().Get("source-path"))
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(pollBody))}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	ac := e2eNewAccountClient("vau2", "SAPISID=sap2", "", doer)
	ac.authUser = 2
	ac.SetTokens("at2", "bl2", "fs2")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := ac.StreamGenerateLive(ctx, "generate a video", StreamOptions{}, nil); err != nil {
		t.Fatalf("StreamGenerateLive: %v", err)
	}
	if gotAU.Load() != "2" || gotSP.Load() != "/u/2/app/vid42" {
		t.Fatalf("expected authuser=2 and source-path=/u/2/app/vid42, got authuser=%v source-path=%v", gotAU.Load(), gotSP.Load())
	}
}

func TestT2_4_4_FailFastOn401And403And429DuringPoll(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			t.Parallel()
			streamInner := e2eMarshalJSON([]any{
				nil, []any{"c_ff", "r_1"}, nil, nil, nil,
				map[string]any{"7": []any{"Generating", []any{"video_generation"}}},
			})
			streamLine := e2eMarshalJSON([][]any{{"wrb.fr", nil, streamInner}})

			doer := &e2eGeminiDoer{
				fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
					if strings.Contains(req.URL.String(), "StreamGenerate") {
						return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + streamLine + "\n"))}, nil
					}
					return &fhttp.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("denied"))}, nil
				},
			}
			ac := e2eNewAccountClient("vff", "SAPISID=sap1", "", doer)
			ac.SetTokens("at1", "bl1", "fs1")
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()

			start := time.Now()
			_, err := ac.StreamGenerateLive(ctx, "generate a video", StreamOptions{}, nil)
			elapsed := time.Since(start)
			if elapsed >= 10*time.Second {
				t.Fatalf("poll HTTP %d did not fail fast (took %s)", status, elapsed)
			}
			var ue *transport.UpstreamError
			if !errors.As(err, &ue) || ue.StatusCode != status {
				t.Fatalf("expected UpstreamError status %d, got %+v (%v)", status, ue, err)
			}
		})
	}
}

// ============================================================================
// Feature Area 11: Audit 6.1 — Large / Chunked JSPB Stream Envelope Parsing
// ============================================================================

func splitBytesIntoNChunks(data []byte, chunkSize int) []e2eChunk {
	var out []e2eChunk
	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}
		out = append(out, e2eChunk{data: data[i:end]})
	}
	return out
}

func TestT1_6_1_SingleOuterArrayNoNewlinesMultiChunk(t *testing.T) {
	f1 := e2eBuildStreamLine("c1", "r1", "rc1", "first", "", false, "")
	f2 := e2eBuildStreamLine("c1", "r1", "rc1", "second", "", true, "")
	// Strip outer [] from each single-element array and wrap into one outer array without newlines.
	e1 := strings.TrimSuffix(strings.TrimPrefix(f1, "["), "]")
	e2 := strings.TrimSuffix(strings.TrimPrefix(f2, "["), "]")
	payload := []byte(")]}'\n\n[" + e1 + "," + e2 + `,["di",123]]`)

	chunks := splitBytesIntoNChunks(payload, len(payload)/30+1)
	ac, _ := newBootstrappedAccountWithStreamChunks(t, chunks)
	var texts []string
	res, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 {
			texts = append(texts, u.Frames[0].Text)
		}
		return nil
	})
	if err != nil || res.Empty || len(texts) != 2 || texts[1] != "second" {
		t.Fatalf("unexpected single-outer-array parse: texts=%v res=%+v err=%v", texts, res, err)
	}
}

func TestT1_6_1_NewlineAndLengthDelimitedChunks(t *testing.T) {
	f1 := e2eBuildStreamLine("c1", "r1", "rc1", "len_one", "", false, "")
	f2 := e2eBuildStreamLine("c1", "r1", "rc1", "len_two", "", true, "ctx1")
	body := fmt.Sprintf(")]}'\n%d\n%s\n%d\n%s\n", len(f1), f1, len(f2), f2)

	chunks := splitBytesIntoNChunks([]byte(body), 7)
	ac, _ := newBootstrappedAccountWithStreamChunks(t, chunks)
	var last string
	res, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 {
			last = u.Frames[0].Text
		}
		return nil
	})
	if err != nil || res.Empty || last != "len_two" {
		t.Fatalf("unexpected length-delimited parse: last=%q res=%+v err=%v", last, res, err)
	}
}

func TestT1_6_1_EscapedQuotesAndBracketsInsideStrings(t *testing.T) {
	tricky := `prefix \"[not,an,array]\" and backslash \\ done`
	line := e2eBuildStreamLine("c1", "r1", "rc1", tricky, "", true, "ctx1")
	payload := []byte(")]}'\n\n[" + strings.TrimSuffix(strings.TrimPrefix(line, "["), "]") + "]")
	chunks := splitBytesIntoNChunks(payload, 5)
	ac, _ := newBootstrappedAccountWithStreamChunks(t, chunks)
	var got string
	if _, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 {
			got = u.Frames[0].Text
		}
		return nil
	}); err != nil || got != tricky {
		t.Fatalf("expected %q, got %q (err=%v)", tricky, got, err)
	}
}

func TestT1_6_1_MultiFrameIncrementalThoughtsAndText(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(")]}'\n\n")
	for i := 1; i <= 10; i++ {
		line := e2eBuildStreamLine("c1", "r1", "rc1", fmt.Sprintf("ans %d", i), fmt.Sprintf("thought %d", i), i == 10, fmt.Sprintf("ctx%d", i))
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	ac, _ := newBootstrappedAccountWithStreamChunks(t, splitBytesIntoNChunks(buf.Bytes(), 37))
	var count int
	var lastText, lastThought string
	_, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 {
			count++
			lastText = u.Frames[0].Text
			lastThought = u.Frames[0].Thoughts
		}
		return nil
	})
	if err != nil || count != 10 || lastText != "ans 10" || lastThought != "thought 10" {
		t.Fatalf("unexpected progressive frames: count=%d text=%q thought=%q err=%v", count, lastText, lastThought, err)
	}
}

func TestT1_6_1_Large128KBFrameChunked(t *testing.T) {
	bigText := strings.Repeat("A", 128*1024)
	line := e2eBuildStreamLine("c1", "r1", "rc1", bigText, "", true, "ctx1")
	payload := []byte(")]}'\n\n" + line + "\n")
	ac, _ := newBootstrappedAccountWithStreamChunks(t, splitBytesIntoNChunks(payload, 512))
	var gotLen int
	_, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 {
			gotLen = len(u.Frames[0].Text)
		}
		return nil
	})
	if err != nil || gotLen != len(bigText) {
		t.Fatalf("expected 128KB frame intact, gotLen=%d want=%d err=%v", gotLen, len(bigText), err)
	}
}

func TestT2_6_1_LinearScaling1536KBIn256ByteChunks(t *testing.T) {
	bigText := strings.Repeat("L", 1536*1024)
	line := e2eBuildStreamLine("c1", "r1", "rc1", bigText, "", true, "ctx1")
	payload := []byte(")]}'\n\n[" + strings.TrimSuffix(strings.TrimPrefix(line, "["), "]") + "]")
	chunks := splitBytesIntoNChunks(payload, 256)
	ac, _ := newBootstrappedAccountWithStreamChunks(t, chunks)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	var got string
	_, err := ac.StreamGenerateLive(ctx, "hi", StreamOptions{}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 {
			got = u.Frames[0].Text
		}
		return nil
	})
	elapsed := time.Since(start)
	if err != nil || got != bigText {
		t.Fatalf("1.5 MiB chunked stream failed (elapsed=%s): len=%d err=%v", elapsed, len(got), err)
	}
	if elapsed >= 4*time.Second {
		t.Fatalf("1.5 MiB in 256B chunks took too long (%s), indicating O(N^2) rescanning", elapsed)
	}
}

func TestT2_6_1_UTF8MultiByteSplitAcrossChunks(t *testing.T) {
	utf8Text := strings.Repeat("🚀中文", 20)
	line := e2eBuildStreamLine("c1", "r1", "rc1", utf8Text, "", true, "ctx1")
	payload := []byte(")]}'\n\n" + line + "\n")
	// 3-byte chunks split multi-byte UTF-8 sequences inside JSON strings.
	ac, _ := newBootstrappedAccountWithStreamChunks(t, splitBytesIntoNChunks(payload, 3))
	var got string
	_, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 {
			got = u.Frames[0].Text
		}
		return nil
	})
	if err != nil || got != utf8Text || strings.ContainsRune(got, '\ufffd') {
		t.Fatalf("UTF-8 corruption across chunk split: got=%q err=%v", got, err)
	}
}

func TestT2_6_1_TrailingUnclosedFragmentFlushedOnEOF(t *testing.T) {
	line := e2eBuildStreamLine("c1", "r1", "rc1", "eof fragment", "", true, "ctx1")
	payload := []byte(")]}'\n\n" + line) // no trailing \n
	ac, _ := newBootstrappedAccountWithStreamChunks(t, splitBytesIntoNChunks(payload, 19))
	var got string
	_, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 {
			got = u.Frames[0].Text
		}
		return nil
	})
	if err != nil || got != "eof fragment" {
		t.Fatalf("expected EOF fragment flush, got=%q err=%v", got, err)
	}
}

func TestT2_6_1_MalformedEnvelopeReturns502UpstreamParseError(t *testing.T) {
	payload := []byte(")]}'\n\n[[\"wrb.fr\",null,\"{broken json\"]]\n")
	ac, _ := newBootstrappedAccountWithStreamChunks(t, splitBytesIntoNChunks(payload, 7))
	_, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, nil)
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 502 || ue.Code != "upstream_parse_error" {
		t.Fatalf("expected 502 upstream_parse_error, got %+v (%v)", ue, err)
	}
}

func TestT2_6_1_ChunkedErrorExtension1013And1037(t *testing.T) {
	for _, tc := range []struct {
		code       int
		wantStatus int
		wantCode   string
	}{
		{1013, 503, "upstream_transient"},
		{1037, 429, "rate_limit_error"},
	} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			line := fmt.Sprintf(`[["wrb.fr",null,null,null,null,[8,null,[["type.googleapis.com/assistant.lamda.BardFrontendService",[[%d]]]]]]]`, tc.code)
			payload := []byte(")]}'\n\n" + line + "\n")
			ac, _ := newBootstrappedAccountWithStreamChunks(t, splitBytesIntoNChunks(payload, 5))
			_, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, nil)
			var ue *transport.UpstreamError
			if !errors.As(err, &ue) || ue.StatusCode != tc.wantStatus || ue.Code != tc.wantCode {
				t.Fatalf("code %d: expected %d/%s, got %+v (%v)", tc.code, tc.wantStatus, tc.wantCode, ue, err)
			}
		})
	}
}

// ============================================================================
// Tier 3 — Pairwise Interaction Combinations (6 Tests)
// ============================================================================

func TestT3_DownloadMediaAndVideoPollIntegration(t *testing.T) {
	t.Parallel()
	streamInner := e2eMarshalJSON([]any{
		nil, []any{"c_t3v", "r_1"}, nil, nil, nil,
		map[string]any{"7": []any{"Generating", []any{"video_generation"}}},
	})
	streamLine := e2eMarshalJSON([][]any{{"wrb.fr", nil, streamInner}})
	pollBody := e2eVideoPollBody("c_t3v", "https://contribution.usercontent.google.com/dl?c=t3", "", true)

	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.Contains(u, "StreamGenerate"):
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + streamLine + "\n"))}, nil
			case strings.Contains(u, "hNvQHb"):
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(pollBody))}, nil
			case req.URL.Host == "contribution.usercontent.google.com":
				return &fhttp.Response{
					StatusCode: 302,
					Header:     fhttp.Header{"Location": []string{"https://lh3.googleusercontent.com/final.mp4"}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			case req.URL.Host == "lh3.googleusercontent.com":
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Content-Type": []string{"video/mp4"}},
					Body:       io.NopCloser(strings.NewReader("T3_MP4")),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}
	ac := e2eNewAccountClient("t3_1", "SAPISID=sap1; __Secure-1PSID=psid1", "", doer)
	ac.SetTokens("at1", "bl1", "fs1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var vidURL string
	_, err := ac.StreamGenerateLive(ctx, "generate a video", StreamOptions{}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 && len(u.Frames[0].GeneratedVideos) > 0 {
			vidURL = u.Frames[0].GeneratedVideos[0].URL
		}
		return nil
	})
	if err != nil || vidURL == "" {
		t.Fatalf("StreamGenerateLive failed: vidURL=%q err=%v", vidURL, err)
	}
	data, ct, err := ac.DownloadMedia(ctx, vidURL, "")
	if err != nil || string(data) != "T3_MP4" || ct != "video/mp4" {
		t.Fatalf("DownloadMedia failed: data=%q ct=%q err=%v", string(data), ct, err)
	}
	for _, r := range doer.Recorded() {
		if r.Host == "lh3.googleusercontent.com" && r.Cookie != "" {
			t.Fatalf("cookie leaked to lh3.googleusercontent.com: %+v", r)
		}
		if (strings.Contains(r.URL, "hNvQHb") || r.Host == "contribution.usercontent.google.com") && r.Cookie == "" {
			t.Fatalf("expected cookie on %s, got empty", r.URL)
		}
	}
}

func TestT3_CookieLifecycleAndRotationUnderConcurrency(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "t3_2.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:    "t3_2",
		Cookies: "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=init_ts",
	}); err != nil {
		t.Fatalf("SaveCredentialFile: %v", err)
	}
	var rotPostCount atomic.Int32
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.Contains(u, "RotateCookiesPage"):
				return &fhttp.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(""))}, nil
			case strings.Contains(u, "RotateCookies"):
				rotPostCount.Add(1)
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=fresh_t3; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			case strings.HasSuffix(u, "/app"):
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_t3","cfb2h":"bl1","FdrFJe":"fs1"}`)),
				}, nil
			case strings.Contains(u, "StreamGenerate"):
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(")]}'\n\n" + e2eBuildStreamLine("c1", "r1", "rc1", "ok", "", true, "ctx1") + "\n")),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}
	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 4})
	ac, err := LoadAccountFromFile(acctPath, pool, cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		_, _ = ac.RotateCookies(context.Background())
	}()
	go func() {
		defer wg.Done()
		_, _ = ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{}, nil)
	}()
	go func() {
		defer wg.Done()
		_ = ac.ReloadFromDiskIfNewer()
	}()
	wg.Wait()

	if rotPostCount.Load() != 1 {
		t.Fatalf("expected single-POST RotateCookies, got %d", rotPostCount.Load())
	}
	if !strings.Contains(ac.CookieHeader(), "fresh_t3") {
		t.Fatalf("expected fresh_t3 in memory, got %q", ac.CookieHeader())
	}
	cf, _, err := cookies.LoadAccountFile(acctPath)
	if err != nil || !strings.Contains(cf.Cookies, "fresh_t3") {
		t.Fatalf("expected fresh_t3 persisted on disk, got %+v err=%v", cf, err)
	}
}

func TestT3_SecondaryAccountRedirectAndVideoPoll(t *testing.T) {
	t.Parallel()
	var rotated atomic.Bool
	var u0Calls atomic.Int32
	streamInner := e2eMarshalJSON([]any{
		nil, []any{"c_t3v2", "r_1"}, nil, nil, nil,
		map[string]any{"7": []any{"Generating", []any{"video_generation"}}},
	})
	streamLine := e2eMarshalJSON([][]any{{"wrb.fr", nil, streamInner}})
	pollBody := e2eVideoPollBody("c_t3v2", "https://contribution.usercontent.google.com/u2v.mp4", "", true)

	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case req.URL.Path == "/app":
				u0Calls.Add(1)
				return &fhttp.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(""))}, nil
			case req.URL.Path == "/u/2/app" && req.URL.RawQuery == "":
				if !rotated.Load() {
					return &fhttp.Response{
						StatusCode: 302,
						Header:     fhttp.Header{"Location": []string{"https://gemini.google.com/app"}},
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				}
				return &fhttp.Response{
					StatusCode: 302,
					Header:     fhttp.Header{"Location": []string{"https://gemini.google.com/u/2/app?hl=id"}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			case u == "https://gemini.google.com/u/2/app?hl=id":
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_u2","cfb2h":"bl2","FdrFJe":"fs2"}`)),
				}, nil
			case strings.Contains(u, "RotateCookies"):
				rotated.Store(true)
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=fresh_u2; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			case strings.Contains(u, "StreamGenerate"):
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + streamLine + "\n"))}, nil
			case strings.Contains(u, "hNvQHb"):
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(pollBody))}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}
	ac := e2eNewAccountClient("u2", "SAPISID=sap2; __Secure-1PSID=psid2; __Secure-1PSIDTS=stale", "", doer)
	ac.authUser = 2
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := ac.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	res, err := ac.StreamGenerateLive(ctx, "generate a video", StreamOptions{}, nil)
	if err != nil || res.Empty {
		t.Fatalf("StreamGenerateLive failed: res=%+v err=%v", res, err)
	}
	if u0Calls.Load() != 0 {
		t.Fatalf("expected /app (u/0) never called, got %d calls", u0Calls.Load())
	}
	var sawPollAuth2 bool
	for _, r := range doer.Recorded() {
		if strings.Contains(r.URL, "hNvQHb") && r.Query.Get("authuser") == "2" && r.Query.Get("source-path") == "/u/2/app/t3v2" {
			sawPollAuth2 = true
		}
	}
	if !sawPollAuth2 {
		t.Fatalf("expected hNvQHb poll with authuser=2 and /u/2/app/t3v2, reqs=%+v", doer.Recorded())
	}
}

func TestT3_TimeoutWatchdogAndLargeChunkedReasoning(t *testing.T) {
	bigText := strings.Repeat("R", 256*1024)
	line := e2eBuildStreamLine("c1", "r1", "rc1", bigText, "thinking", true, "ctx1")
	bigChunks := splitBytesIntoNChunks([]byte(line+"\n"), 512)
	allChunks := append([]e2eChunk{
		{data: []byte(")]}'\n\n[[\"wrb.fr\",null,null,null,null,[3]]]\n"), delay: 5 * time.Millisecond},
	}, bigChunks...)
	// 70ms reasoning pause on the first data chunk (> 40ms FirstByteTimeout, < 120ms NoFrameTimeout).
	allChunks[1].delay = 70 * time.Millisecond

	stream1Ready := make(chan struct{})
	stream2Ready := make(chan struct{})
	var close1Once, close2Once sync.Once
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, bodyStr string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "StreamGenerate") {
				if strings.Contains(bodyStr, "cancel-p2") {
					close2Once.Do(func() { close(stream2Ready) })
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{},
						Body: &e2eChunkedBody{chunks: []e2eChunk{
							{data: []byte(")]}'\n\n"), delay: 2 * time.Millisecond},
							{data: []byte("late"), delay: 2 * time.Second, ctx: req.Context()},
						}},
					}, nil
				}
				if strings.Contains(bodyStr, "reason-p1") {
					close1Once.Do(func() { close(stream1Ready) })
					return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: &e2eChunkedBody{chunks: allChunks}}, nil
				}
				return &fhttp.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader("unexpected prompt"))}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	pool := transport.NewPool(transport.PoolOptions{
		ClientConfig: transport.ClientConfig{
			CustomDoer:       doer,
			Timeout:          5 * time.Second,
			FirstByteTimeout: 40 * time.Millisecond,
			StallTimeout:     350 * time.Millisecond,
		},
		MaxSessions: 4,
		MaxQueue:    4,
		WaitTimeout: time.Second,
	})
	ac := NewAccountClient("a1", "SAPISID=sap1", "", pool, cookies.NewRotator())
	ac.SetTokens("at1", "bl1", "fs1")

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	var wg sync.WaitGroup
	var gotText string
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err1 = ac.StreamGenerateLive(context.Background(), "reason-p1", StreamOptions{NoFrameTimeout: 300 * time.Millisecond}, func(u *LineParseResult) error {
			if len(u.Frames) > 0 {
				gotText = u.Frames[0].Text
			}
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		_, err2 = ac.StreamGenerateLive(ctx2, "cancel-p2", StreamOptions{}, nil)
	}()
	select {
	case <-stream1Ready:
	case <-time.After(3 * time.Second):
		t.Fatal("stream 1 did not reach doer")
	}
	select {
	case <-stream2Ready:
	case <-time.After(3 * time.Second):
		t.Fatal("stream 2 did not reach doer")
	}
	cancel2()
	wg.Wait()

	if err1 != nil || len(gotText) != len(bigText) {
		t.Fatalf("reasoning stream failed: len=%d err=%v", len(gotText), err1)
	}
	var ue2 *transport.UpstreamError
	if !errors.As(err2, &ue2) || ue2.StatusCode != 499 {
		t.Fatalf("expected peer stream 2 499 cancel, got %v", err2)
	}
}

func TestT3_SessionLeaseSaturationWithPeerRunStreamAndClose(t *testing.T) {
	firstChunk := make(chan struct{})
	releaseStream := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("part1\n"))
		if fl != nil {
			fl.Flush()
		}
		select {
		case <-releaseStream:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("part2\n"))
	}))
	defer srv.Close()

	sc, err := transport.NewStealthClient(transport.ClientConfig{
		Timeout:          10 * time.Second,
		FirstByteTimeout: 2 * time.Second,
		StallTimeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewStealthClient: %v", err)
	}
	sess := transport.NewSession("sat", sc, 1, "SAPISID=1", 30, 20*time.Millisecond)

	holderStarted := make(chan struct{})
	releaseHolder := make(chan struct{})
	var holderWG sync.WaitGroup
	holderWG.Add(1)
	go func() {
		defer holderWG.Done()
		_ = sess.Lease(context.Background(), func(string) error {
			close(holderStarted)
			<-releaseHolder
			return nil
		})
	}()
	<-holderStarted

	var streamBody strings.Builder
	var streamErr error
	var streamWG sync.WaitGroup
	var notified atomic.Bool
	streamWG.Add(1)
	go func() {
		defer streamWG.Done()
		streamErr = sess.RunStream(context.Background(), func(string) error {
			_, err := sc.Do(context.Background(), transport.RequestOptions{
				Method: "GET",
				URL:    srv.URL,
				OnChunk: func(ch string) error {
					streamBody.WriteString(ch)
					if notified.CompareAndSwap(false, true) {
						close(firstChunk)
					}
					return nil
				},
			})
			return err
		})
	}()
	<-firstChunk

	runtime.GC()
	before := runtime.NumGoroutine()

	var leaseWG sync.WaitGroup
	for range 20 {
		leaseWG.Add(1)
		go func() {
			defer leaseWG.Done()
			_ = sess.Lease(context.Background(), func(string) error { return nil })
		}()
	}
	leaseWG.Wait()

	runtime.GC()
	after := runtime.NumGoroutine()

	sess.Close()
	close(releaseStream)
	close(releaseHolder)
	streamWG.Wait()
	holderWG.Wait()

	if streamErr != nil || !strings.Contains(streamBody.String(), "part2") {
		t.Fatalf("expected active RunStream to finish cleanly after Session.Close, body=%q err=%v", streamBody.String(), streamErr)
	}
	if after-before > 4 {
		t.Fatalf("expected zero leaked goroutines on saturated Lease, before=%d after=%d", before, after)
	}
}

func TestT3_DiskProxyReloadWithUploadAndDownload(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "t3_6.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:     "t3_6",
		Cookies:  "SAPISID=sap1; __Secure-1PSIDTS=ts1",
		ProxyURL: "http://127.0.0.1:7101",
	}); err != nil {
		t.Fatalf("SaveCredentialFile 1: %v", err)
	}
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "content-push.googleapis.com") {
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("/up/1"))}, nil
			}
			if req.URL.Host == "lh3.googleusercontent.com" {
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Content-Type": []string{"image/png"}},
					Body:       io.NopCloser(strings.NewReader("PNG")),
				}, nil
			}
			return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 4})
	defer pool.Reset()
	ac, err := LoadAccountFromFile(acctPath, pool, cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile: %v", err)
	}
	s1, err := ac.acquireSession(context.Background())
	if err != nil || s1.ProxyURL() != "http://127.0.0.1:7101" {
		t.Fatalf("expected initial proxy 7101, got %v %v", s1, err)
	}

	time.Sleep(15 * time.Millisecond)
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:     "t3_6",
		Cookies:  "SAPISID=sap1; __Secure-1PSIDTS=ts2_newest",
		ProxyURL: "http://127.0.0.1:7102",
	}); err != nil {
		t.Fatalf("SaveCredentialFile 2: %v", err)
	}

	if _, err := ac.UploadFile(context.Background(), []byte("bytes"), "a.png", "image/png"); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	data, _, err := ac.DownloadMedia(context.Background(), "https://lh3.googleusercontent.com/img.png", "")
	if err != nil || string(data) != "PNG" {
		t.Fatalf("DownloadMedia: data=%q err=%v", string(data), err)
	}
	s2 := pool.Existing("t3_6")
	if s2 == nil || s2.ProxyURL() != "http://127.0.0.1:7102" {
		t.Fatalf("expected session proxy updated to 7102, got %v", s2)
	}
	if !strings.Contains(ac.CookieHeader(), "ts2_newest") {
		t.Fatalf("expected ac.CookieHeader() to retain ts2_newest, got %q", ac.CookieHeader())
	}
}

// ============================================================================
// Tier 4 — Realistic End-to-End Scenarios (4 Tests)
// ============================================================================

func TestT4_E2E_RealHTTPWireReasoningStreamAndCancelIsolation(t *testing.T) {
	chunk64KB := strings.Repeat("T", 64*1024)
	answerLine := e2eBuildStreamLine("c_wire", "r_wire", "rc_wire", chunk64KB, "deep reasoning", true, "ctx_wire")

	streamAReady := make(chan struct{})
	streamBReady := make(chan struct{})
	var closeAOnce, closeBOnce sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		isCancelStream := strings.Contains(string(bodyBytes), "cancel")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(")]}'\n\n"))
		if fl != nil {
			fl.Flush()
		}
		if !isCancelStream {
			closeAOnce.Do(func() { close(streamAReady) })
			// Stream A pauses 100ms (> 40ms FirstByteTimeout) then flushes 64KB chunked JSPB.
			time.Sleep(100 * time.Millisecond)
			payload := []byte(answerLine + "\n")
			for i := 0; i < len(payload); i += 1024 {
				end := i + 1024
				if end > len(payload) {
					end = len(payload)
				}
				_, _ = w.Write(payload[i:end])
				if fl != nil {
					fl.Flush()
				}
			}
			return
		}
		closeBOnce.Do(func() { close(streamBReady) })
		<-r.Context().Done()
	}))
	defer srv.Close()

	httpDoer := newE2EHTTPTestDoer()
	rewritingDoer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			cloned := req.Clone(req.Context())
			target, _ := url.Parse(srv.URL + req.URL.RequestURI())
			cloned.URL = target
			cloned.Host = target.Host
			return httpDoer.Do(cloned)
		},
	}

	pool := transport.NewPool(transport.PoolOptions{
		ClientConfig: transport.ClientConfig{
			CustomDoer:       rewritingDoer,
			Timeout:          10 * time.Second,
			FirstByteTimeout: 40 * time.Millisecond,
			StallTimeout:     300 * time.Millisecond,
		},
		MaxSessions: 4,
		MaxQueue:    4,
		WaitTimeout: 2 * time.Second,
	})
	defer pool.Reset()

	ac := NewAccountClient("wire1", "SAPISID=sap1", "", pool, cookies.NewRotator())
	ac.SetTokens("at1", "bl1", "fs1")

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	var wg sync.WaitGroup
	var gotText, gotThought string
	var errA, errB error

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errA = ac.StreamGenerateLive(context.Background(), "reason deeply", StreamOptions{NoFrameTimeout: 300 * time.Millisecond}, func(u *LineParseResult) error {
			if len(u.Frames) > 0 {
				gotText = u.Frames[0].Text
				gotThought = u.Frames[0].Thoughts
			}
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		_, errB = ac.StreamGenerateLive(ctxB, "cancel me", StreamOptions{}, nil)
	}()

	select {
	case <-streamAReady:
	case <-time.After(3 * time.Second):
		t.Fatal("stream A did not reach server")
	}
	select {
	case <-streamBReady:
	case <-time.After(3 * time.Second):
		t.Fatal("stream B did not reach server")
	}
	cancelB()
	wg.Wait()

	if errA != nil || gotText != chunk64KB || gotThought != "deep reasoning" {
		t.Fatalf("Stream A failed: len=%d thought=%q err=%v", len(gotText), gotThought, errA)
	}
	var ueB *transport.UpstreamError
	if !errors.As(errB, &ueB) || ueB.StatusCode != 499 {
		t.Fatalf("expected Stream B 499 cancel, got %v", errB)
	}
	if httpDoer.closeIdleCalls.Load() != 0 || rewritingDoer.closeIdleCalls.Load() != 0 {
		t.Fatalf("expected 0 CloseIdleConnections/ResetConnections on cancel, got http=%d wrapper=%d",
			httpDoer.closeIdleCalls.Load(), rewritingDoer.closeIdleCalls.Load())
	}
}

func TestT4_E2E_FullMultiTurnSelfHealUploadStreamAndMediaDownload(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "u1.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:     "u1",
		Cookies:  "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=expired_ts",
		AuthUser: 1,
	}); err != nil {
		t.Fatalf("SaveCredentialFile: %v", err)
	}

	var rotated atomic.Bool
	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.Contains(u, "content-push.googleapis.com"):
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("/contrib/up_t4"))}, nil
			case req.URL.Path == "/u/1/app" && req.URL.RawQuery == "":
				if !rotated.Load() {
					return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`<html>expired</html>`))}, nil
				}
				return &fhttp.Response{
					StatusCode: 302,
					Header:     fhttp.Header{"Location": []string{"https://gemini.google.com/u/1/app?hl=en"}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			case u == "https://gemini.google.com/u/1/app?hl=en":
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_t4","cfb2h":"bl_t4","FdrFJe":"fs_t4","oPEP7c":"u1@example.com"}`)),
				}, nil
			case strings.Contains(u, "RotateCookies"):
				rotated.Store(true)
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=fresh_t4; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			case strings.Contains(u, "StreamGenerate"):
				c12 := map[string]any{
					"60": []any{
						[]any{nil, nil, "res.mp4", nil, nil, nil, nil, []any{"", "https://contribution.usercontent.google.com/dl?c=t4", ""}, nil, nil, nil, "video/mp4"},
					},
				}
				cand := []any{"rc1", []any{"Done"}, nil, nil, nil, nil, nil, nil, []any{2}, map[string]any{"13": c12}}
				inner := e2eMarshalJSON([]any{nil, []any{"c_t4", "r_t4"}, nil, nil, []any{cand}})
				line := e2eMarshalJSON([][]any{{"wrb.fr", nil, inner}})
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + line + "\n"))}, nil
			case req.URL.Host == "contribution.usercontent.google.com":
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Content-Type": []string{"text/plain"}},
					Body:       io.NopCloser(strings.NewReader("https://lh3.googleusercontent.com/final_t4")),
				}, nil
			case req.URL.Host == "lh3.googleusercontent.com":
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Content-Type": []string{"video/mp4"}},
					Body:       io.NopCloser(strings.NewReader("FINAL_MEDIA_T4")),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}

	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 4})
	ac, err := LoadAccountFromFile(acctPath, pool, cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile: %v", err)
	}

	up, err := ac.UploadFile(context.Background(), []byte("input-img"), "in.png", "image/png")
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}

	var mediaURL string
	res, err := ac.StreamGenerateLive(context.Background(), "animate", StreamOptions{Files: []UploadedFile{up}}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 && len(u.Frames[0].GeneratedVideos) > 0 {
			mediaURL = u.Frames[0].GeneratedVideos[0].URL
		}
		return nil
	})
	if err != nil || res.Empty || mediaURL == "" {
		t.Fatalf("StreamGenerateLive: res=%+v mediaURL=%q err=%v", res, mediaURL, err)
	}

	data, ct, err := ac.DownloadMedia(context.Background(), mediaURL, "")
	if err != nil || string(data) != "FINAL_MEDIA_T4" || ct != "video/mp4" {
		t.Fatalf("DownloadMedia: data=%q ct=%q err=%v", string(data), ct, err)
	}

	for _, r := range doer.Recorded() {
		if r.Host == "lh3.googleusercontent.com" && r.Cookie != "" {
			t.Fatalf("leaked cookies to lh3.googleusercontent.com: %q", r.Cookie)
		}
	}
	st, err := os.Stat(acctPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 file mode, got %v %v", st, err)
	}
	cf, _, err := cookies.LoadAccountFile(acctPath)
	if err != nil || cf.AuthUser != 1 || !strings.Contains(cf.Cookies, "fresh_t4") {
		t.Fatalf("unexpected disk CredentialFile: %+v err=%v", cf, err)
	}
}

func TestT4_E2E_AsyncVideoPollingWithLiveRotationAndCancelFallback(t *testing.T) {
	t.Parallel()
	frame1 := e2eMarshalJSON([][]any{{"wrb.fr", nil, e2eMarshalJSON([]any{
		nil, nil, nil, nil, nil,
		map[string]any{"7": []any{"Generating", []any{"video_generation"}}},
	})}})
	frame2 := e2eMarshalJSON([][]any{{"wrb.fr", nil, e2eMarshalJSON([]any{
		nil, []any{"c_t4v", "r_1"},
	})}})

	var streamCount, pollCount atomic.Int32
	poll1Started := make(chan struct{})
	var poll2Cookie atomic.Value

	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.Contains(u, "StreamGenerate"):
				streamCount.Add(1)
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(")]}'\n\n" + frame1 + "\n" + frame2 + "\n")),
				}, nil
			case strings.Contains(u, "RotateCookies"):
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=rotated_t4v; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			case strings.Contains(u, "hNvQHb"):
				if pollCount.Add(1) == 1 {
					close(poll1Started)
					return &fhttp.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(e2eVideoPollBody("c_t4v", "", "Generating your video...", false))),
					}, nil
				}
				poll2Cookie.Store(e2eHeaderGet(req.Header, "cookie"))
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(e2eVideoPollBody("c_t4v", "https://rr2---sn.googlevideo.com/videoplayback?id=t4v", "", true))),
				}, nil
			case req.URL.Host == "rr2---sn.googlevideo.com":
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Content-Type": []string{"video/mp4"}},
					Body:       io.NopCloser(strings.NewReader("GV_MP4")),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}

	ac := e2eNewAccountClient("t4v", "SAPISID=sap1; __Secure-1PSID=psid1; __Secure-1PSIDTS=init_t4v", "", doer)
	ac.SetTokens("at1", "bl1", "fs1")

	go func() {
		<-poll1Started
		_, _ = ac.RotateCookies(context.Background())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var vidURL string
	_, err := ac.StreamGenerateLive(ctx, "generate a video", StreamOptions{Temporary: true, DisableTools: true}, func(u *LineParseResult) error {
		if len(u.Frames) > 0 && len(u.Frames[0].GeneratedVideos) > 0 {
			vidURL = u.Frames[0].GeneratedVideos[0].URL
		}
		return nil
	})
	if err != nil || streamCount.Load() != 2 || vidURL == "" {
		t.Fatalf("expected usedTemp retry (2 streams) + poll completion, got streams=%d url=%q err=%v", streamCount.Load(), vidURL, err)
	}
	if c, _ := poll2Cookie.Load().(string); !strings.Contains(c, "rotated_t4v") {
		t.Fatalf("expected poll 2 to send rotated_t4v, got %q", c)
	}
	data, _, err := ac.DownloadMedia(ctx, vidURL, "")
	if err != nil || string(data) != "GV_MP4" {
		t.Fatalf("DownloadMedia from googlevideo.com failed: data=%q err=%v", string(data), err)
	}
	for _, r := range doer.Recorded() {
		if r.Host == "rr2---sn.googlevideo.com" && r.Cookie != "" {
			t.Fatalf("cookies leaked to googlevideo.com: %q", r.Cookie)
		}
	}
}

func TestT4_E2E_PoolFailoverRecoveryRedirectAndQuotaAudit(t *testing.T) {
	acctPath := filepath.Join(t.TempDir(), "u2.json")
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:     "u2",
		Cookies:  "SAPISID=sap2; __Secure-1PSID=psid2; __Secure-1PSIDTS=dead_ts",
		AuthUser: 2,
		Disabled: true,
	}); err != nil {
		t.Fatalf("SaveCredentialFile: %v", err)
	}

	var rotPosts atomic.Int32
	var otCalls atomic.Int32
	validModelInner := make([]any, 18)
	validModelInner[15] = []any{[]any{"954c0abcba24364", "Pro", "desc", nil, nil, nil, nil, nil, nil, 4, "Pro", "Gemini Pro"}}
	validBatchLine := e2eMarshalJSON([][]any{{"wrb.fr", "otAQ7b", e2eMarshalJSON(validModelInner)}})
	err1016Payload := make([]any, 15)
	err1016Payload[14] = 1016
	err1016Line := e2eMarshalJSON([][]any{{"wrb.fr", "otAQ7b", e2eMarshalJSON(err1016Payload)}})
	qpBody := ")]}'\n\n" + e2eMarshalJSON([][]any{{"wrb.fr", "qpEbW", e2eMarshalJSON([]any{[]any{[]any{[]any{1, 11}, 0, 0.1, []any{1900000000}, 2400, 2160}}})}}) + "\n"
	gqlBody := ")]}'\n" + e2eMarshalJSON([]any{map[string]any{
		"data": map[string]any{"viewer": map[string]any{"rpcs": map[string]any{"getUsageInfo": map[string]any{
			"pools": []any{
				map[string]any{"poolType": "GXU_FIVE_HOURLY", "percentageUsed": 0.1, "gxu": map[string]any{"windowRefreshesAt": "2030-01-01T00:00:00Z"}},
			},
		}}}},
	}})

	doer := &e2eGeminiDoer{
		fn: func(req *fhttp.Request, _ string) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.Contains(u, "RotateCookies"):
				if rotPosts.Add(1) > 1 {
					return &fhttp.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				return &fhttp.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("unauthorized"))}, nil
			case req.URL.Path == "/u/2/app":
				return &fhttp.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_u2_rec","cfb2h":"bl2","FdrFJe":"fs2","oPEP7c":"u2@example.com"}`)),
				}, nil
			case strings.Contains(u, "otAQ7b"):
				if otCalls.Add(1) == 1 {
					return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + err1016Line + "\n"))}, nil
				}
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + validBatchLine + "\n"))}, nil
			case strings.Contains(u, "qpEbW"):
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(qpBody))}, nil
			case strings.Contains(u, "GEMINI_WEB_GRAPHQL"):
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(gqlBody))}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}
	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 4})
	ac, err := LoadAccountFromFile(acctPath, pool, cookies.NewRotator())
	if err != nil {
		t.Fatalf("LoadAccountFromFile: %v", err)
	}

	rotRes, err := ac.RotateCookies(context.Background())
	if err != nil || rotRes.Status != 401 || rotPosts.Load() != 1 {
		t.Fatalf("expected 401 on single-POST RotateCookies without 429 mask, got res=%+v posts=%d err=%v", rotRes, rotPosts.Load(), err)
	}

	// External re-login writes fresh credentials and clears Disabled.
	time.Sleep(15 * time.Millisecond)
	if _, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:     "u2",
		Cookies:  "SAPISID=sap2; __Secure-1PSID=psid2; __Secure-1PSIDTS=relogin_ts",
		AuthUser: 2,
		Disabled: false,
	}); err != nil {
		t.Fatalf("SaveCredentialFile relogin: %v", err)
	}
	if !ac.ReloadFromDiskIfNewer() {
		t.Fatal("expected ReloadFromDiskIfNewer to pick up relogin_ts")
	}
	ac.mu.RLock()
	stillDisabled := ac.disabled
	ac.mu.RUnlock()
	if stillDisabled {
		t.Fatal("expected ReloadFromDiskIfNewer to clear disabled=false from disk")
	}

	if err := ac.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	mods, err := ac.AvailableModels(context.Background(), true)
	if err != nil || len(mods.Models) != 1 {
		t.Fatalf("AvailableModels: %+v %v", mods, err)
	}
	buckets := ac.GetQuota(context.Background())
	if len(buckets) == 0 {
		t.Fatal("expected non-empty GetQuota")
	}
	var sawRPCAuth2, sawGQLAuth2 bool
	for _, r := range doer.Recorded() {
		if strings.Contains(r.URL, "qpEbW") && r.Query.Get("authuser") == "2" {
			sawRPCAuth2 = true
		}
		if strings.Contains(r.URL, "GEMINI_WEB_GRAPHQL") && r.Query.Get("authuser") == "2" {
			sawGQLAuth2 = true
		}
	}
	if !sawRPCAuth2 || !sawGQLAuth2 {
		t.Fatalf("expected authuser=2 on both qpEbW (%v) and GraphQL (%v)", sawRPCAuth2, sawGQLAuth2)
	}
}
