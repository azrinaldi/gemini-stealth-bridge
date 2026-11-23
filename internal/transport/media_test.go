package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

type mediaRoundTripper func(*http.Request) (*http.Response, error)

func (f mediaRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPlainMediaFetchDoesNotChangeGenerationTransport(t *testing.T) {
	var generationCalls, mediaCalls int
	sc, err := NewStealthClient(ClientConfig{MediaPlainFetch: true, UserAgent: "fixture-browser", CustomDoer: &stubDoer{fn: func(*fhttp.Request) (*fhttp.Response, error) {
		generationCalls++
		return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader("generation"))}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	sc.mediaClient = &http.Client{Transport: mediaRoundTripper(func(r *http.Request) (*http.Response, error) {
		mediaCalls++
		if r.Header.Get("User-Agent") != "fixture-browser" || r.Header.Get("Referer") != "https://gemini.google.com/" || r.Header.Get("Cookie") != "SID=fixture" {
			t.Fatal("media request lost account headers")
		}
		if r.Header.Get("Sec-Fetch-Mode") != "" || r.Header.Get("Sec-Fetch-Dest") != "" || strings.Contains(r.Header.Get("Accept"), "text/html") {
			t.Fatal("plain media request used document navigation headers")
		}
		if r.URL.String() != "https://lh3.googleusercontent.com/fixture=s512-rw?alr=yes" {
			t.Fatal("upstream URL was modified")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}}, Body: io.NopCloser(strings.NewReader("image"))}, nil
	})}
	res, err := sc.DoMedia(context.Background(), RequestOptions{Method: "GET", URL: "https://lh3.googleusercontent.com/fixture=s512-rw?alr=yes", Headers: map[string]string{"referer": "https://gemini.google.com/", "cookie": "SID=fixture"}})
	if err != nil || res.Body != "image" {
		t.Fatalf("plain media fetch failed: %v", err)
	}
	if _, err := sc.Do(context.Background(), RequestOptions{Method: "POST", URL: "https://gemini.google.com/generate"}); err != nil || generationCalls != 1 || mediaCalls != 1 {
		t.Fatalf("generation transport changed: generation=%d media=%d error=%v", generationCalls, mediaCalls, err)
	}
}

func TestPlainMediaFetchLimitsTypesAndRedactsURLErrors(t *testing.T) {
	for _, tc := range []struct {
		name, ct, body string
		err            error
	}{
		{name: "body cap", ct: "image/png", body: strings.Repeat("x", 17)},
		{name: "HTML rejection", ct: "text/html", body: "not an image"},
		{name: "redacted error", err: &url.Error{Op: "Get", URL: "https://lh3.googleusercontent.com/secret-path?token=secret-query", Err: context.DeadlineExceeded}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := &StealthClient{cfg: ClientConfig{MediaPlainFetch: true}, mediaClient: &http.Client{Transport: mediaRoundTripper(func(*http.Request) (*http.Response, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {tc.ct}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}}
			_, err := sc.DoMedia(context.Background(), RequestOptions{Method: "GET", URL: "https://lh3.googleusercontent.com/fixture", MaxBodyBytes: 16})
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("missing rejection or leaked signed URL: %v", err)
			}
			if tc.err != nil && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("request error cause was lost")
			}
		})
	}
}

func TestPlainMediaClientUsesEffectiveProxyAndIgnoresEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://ambient.invalid:9000")
	pool := NewPool(PoolOptions{ClientConfig: ClientConfig{MediaPlainFetch: true, ProxyURL: "socks5://127.0.0.1:1080", CustomDoer: &stubDoer{}}})
	for _, tc := range []struct{ id, override, want string }{
		{"inherited", "", "socks5://127.0.0.1:1080"},
		{"override", "http://127.0.0.1:9001", "http://127.0.0.1:9001"},
	} {
		session, err := pool.AcquireWithProxy(context.Background(), tc.id, "SID=fixture", tc.override)
		if err != nil {
			t.Fatal(err)
		}
		client, err := session.Client.plainMediaClient()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.CloseIdleConnections)
		req, _ := http.NewRequest("GET", "https://lh3.googleusercontent.com/fixture", nil)
		proxy, err := client.Transport.(*http.Transport).Proxy(req)
		if err != nil || proxy.String() != tc.want {
			t.Fatalf("wrong account egress: got=%v want=%s error=%v", proxy, tc.want, err)
		}
	}
	direct := &StealthClient{cfg: ClientConfig{MediaPlainFetch: true}}
	client, err := direct.plainMediaClient()
	if err != nil || client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("direct account inherited ambient proxy")
	}
	t.Cleanup(client.CloseIdleConnections)
	invalid := &StealthClient{cfg: ClientConfig{MediaPlainFetch: true, ProxyURL: "socks4://user:secret@127.0.0.1:1080"}}
	if _, err := invalid.plainMediaClient(); err == nil || strings.Contains(err.Error(), "secret") || invalid.mediaClient != nil {
		t.Fatal("unsupported proxy did not fail closed with redacted error")
	}
}

func TestPlainMediaFetchDoesNotFollowRedirectsOrFallbackToDirect(t *testing.T) {
	var originCalls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originCalls.Add(1)
		w.Header().Set("Location", "https://untrusted.invalid/secret")
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()
	direct := &StealthClient{cfg: ClientConfig{MediaPlainFetch: true}}
	t.Cleanup(direct.CloseIdleConnections)
	res, err := direct.DoMedia(context.Background(), RequestOptions{Method: "GET", URL: origin.URL})
	if err != nil || res.Status != 302 || originCalls.Load() != 1 {
		t.Fatalf("redirect was not returned for per-hop validation: calls=%d error=%v", originCalls.Load(), err)
	}
	proxied := &StealthClient{cfg: ClientConfig{MediaPlainFetch: true, ProxyURL: "http://127.0.0.1:1"}}
	t.Cleanup(proxied.CloseIdleConnections)
	if _, err := proxied.DoMedia(context.Background(), RequestOptions{Method: "GET", URL: origin.URL, Timeout: time.Second}); err == nil || originCalls.Load() != 1 {
		t.Fatal("failed proxy must never retry through direct egress")
	}
}
