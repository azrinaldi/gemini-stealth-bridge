package gemini

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
	fhttp "github.com/bogdanfinn/fhttp"
)

func TestGeneratedImagePreservesVerbatimUpstreamURL(t *testing.T) {
	for _, raw := range []string{
		"https://lh3.googleusercontent.com/rd-gg-dl/fixture=s512-rw?alr=yes&token=fixture",
		"https://lh3.googleusercontent.com/gg-dl/fixture=s2048",
		"https://lh3.googleusercontent.com/gg-dl/fixture",
	} {
		item := []any{[]any{nil, nil, nil, []any{nil, float64(1024), float64(768), raw}}}
		media := make([]any, 8)
		media[7] = []any{[]any{item}}
		candidate := make([]any, 13)
		candidate[12] = media
		images := extractGeneratedImages(candidate)
		if len(images) != 1 || images[0].RawURL != raw || images[0].Width != 1024 || images[0].Height != 768 {
			t.Fatalf("original image URL or dimensions lost: %+v", images)
		}
	}
}

func TestMediaURLAllowsFIFERedirectButRejectsUntrustedTargets(t *testing.T) {
	for _, raw := range []string{
		"https://work.fife.usercontent.google.com/rd-gg-dl/fixture",
		"https://usercontent.google.com/fixture",
		"https://lh3.googleusercontent.com/fixture",
		"https://rr1.googlevideo.com/fixture",
	} {
		if _, ok := mediaURL(raw, nil); !ok {
			t.Fatalf("trusted media host rejected: %s", raw)
		}
	}
	for _, raw := range []string{
		"https://work.fife.usercontent.google.com.evil.test/fixture",
		"https://evilusercontent.google.com/fixture",
		"https://user:password@work.fife.usercontent.google.com/fixture",
		"http://work.fife.usercontent.google.com/fixture",
		"https://work.fife.usercontent.google.com:8443/fixture",
		"https://127.0.0.1/fixture",
	} {
		if _, ok := mediaURL(raw, nil); ok {
			t.Fatalf("untrusted media URL accepted: %s", raw)
		}
	}
}

func TestDownloadMediaFIFERedirectAndTextURLChain(t *testing.T) {
	var hosts []string
	doer := &stubGeminiDoer{fn: func(req *fhttp.Request) (*fhttp.Response, error) {
		hosts = append(hosts, req.URL.Hostname())
		if req.URL.Path == "/start" {
			if got := strings.Join(req.Header["sec-fetch-mode"], ""); got != "cors" {
				t.Errorf("media fetch mode=%q, want cors", got)
			}
			if got := strings.Join(req.Header["sec-fetch-dest"], ""); got != "empty" {
				t.Errorf("media fetch destination=%q, want empty", got)
			}
			if got := strings.Join(req.Header["sec-fetch-site"], ""); got != "cross-site" {
				t.Errorf("usercontent fetch site=%q, want cross-site", got)
			}
			if strings.Join(req.Header["cookie"], "; ") != "" {
				t.Error("Google account cookies leaked to googleusercontent")
			}
			return &fhttp.Response{StatusCode: 302, Header: fhttp.Header{"Location": {"https://work.fife.usercontent.google.com/resolve"}, "Set-Cookie": {"MEDIA_GRANT=fixture; Path=/; Secure", "SID=media; Path=/; Secure"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		if req.URL.Path == "/resolve" {
			if got := strings.Join(req.Header["cookie"], "; "); strings.Contains(got, "MEDIA_GRANT") || strings.Contains(got, "SID=media") {
				t.Errorf("FIFE received another domain's media cookies: %q", got)
			}
			if got := strings.Join(req.Header["cookie"], "; "); strings.Contains(got, "OSID=") || strings.Contains(got, "COMPASS=gemini") || !strings.Contains(got, "__Secure-1PSIDRTS=fixture") {
				t.Errorf("FIFE cookie scopes or modern-session cookies incorrect: %q", got)
			}
			if got := strings.Join(req.Header["sec-fetch-site"], ""); got != "same-site" {
				t.Errorf("FIFE fetch site=%q, want same-site", got)
			}
			return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{"Content-Type": {"text/plain"}, "Set-Cookie": {"COMPASS=fife; Path=/; Secure"}}, Body: io.NopCloser(strings.NewReader("https://lh3.googleusercontent.com/final=s512-rw?alr=yes"))}, nil
		}
		if req.URL.RawQuery != "alr=yes" {
			t.Error("redirect query was modified")
		}
		if got := strings.Join(req.Header["cookie"], "; "); got != "MEDIA_GRANT=fixture; SID=media" {
			t.Errorf("media grants were dropped or assigned to the wrong domain: %q", got)
		}
		return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{"Content-Type": {"image/png"}}, Body: io.NopCloser(strings.NewReader("image fixture"))}, nil
	}}
	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}})
	const accountHeader = "SID=account; SAPISID=fixture; OSID=gemini; __Secure-OSID=gemini; COMPASS=gemini; __Secure-1PSIDRTS=fixture"
	ac := NewAccountClient("fixture", accountHeader, "", pool, nil)
	data, ct, err := ac.DownloadMedia(context.Background(), "https://lh3.googleusercontent.com/start", "")
	if err != nil || string(data) != "image fixture" || ct != "image/png" || strings.Join(hosts, ",") != "lh3.googleusercontent.com,work.fife.usercontent.google.com,lh3.googleusercontent.com" {
		t.Fatalf("media redirect chain failed: hosts=%v type=%s error=%v", hosts, ct, err)
	}
	if ac.CookieHeader() != accountHeader {
		t.Fatal("media-only cookies contaminated saved account credentials")
	}
}

func TestDownloadMediaAddsNativeImageParametersWithoutChangingTokens(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://lh3.googleusercontent.com/gg-dl/fixture", "https://lh3.googleusercontent.com/gg-dl/fixture=s0?alr=yes"},
		{"https://lh3.googleusercontent.com/gg-dl/token%2Ffixture", "https://lh3.googleusercontent.com/gg-dl/token%2Ffixture=s0?alr=yes"},
		{"https://lh3.googleusercontent.com/gg-dl/fixture=s512-rw?token=fixture", "https://lh3.googleusercontent.com/gg-dl/fixture=s512-rw?token=fixture&alr=yes"},
		{"https://lh3.googleusercontent.com/gg-dl/fixture=s512-rw?alr=yes&token=fixture", "https://lh3.googleusercontent.com/gg-dl/fixture=s512-rw?alr=yes&token=fixture"},
		{"https://lh3.googleusercontent.com/fixture", "https://lh3.googleusercontent.com/fixture"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			doer := &stubGeminiDoer{fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				if req.URL.String() != tc.want {
					t.Errorf("media URL corrupted: got=%s want=%s", req.URL.String(), tc.want)
				}
				return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{"Content-Type": {"image/png"}}, Body: io.NopCloser(strings.NewReader("image fixture"))}, nil
			}}
			pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}})
			ac := NewAccountClient("fixture", "SAPISID=fixture", "", pool, nil)
			if _, _, err := ac.DownloadMedia(context.Background(), tc.raw, ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMediaURLShapeDoesNotExposeSignedValues(t *testing.T) {
	shape := urlShape("https://lh3.googleusercontent.com/rd-gg-dl/secret-path=s512-rw?alr=yes&token=secret-query")
	if shape != `suffix="=s512-rw" query=[alr token]` || strings.Contains(shape, "secret") {
		t.Fatalf("unsafe or incorrect diagnostic shape: %s", shape)
	}
	if shape := urlShape("https://lh3.googleusercontent.com/fixture=s-secret-path"); strings.Contains(shape, "secret") {
		t.Fatalf("unrecognized suffix leaked path: %s", shape)
	}
}
