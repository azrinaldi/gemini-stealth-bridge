package gemini

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

const maxMediaDownloadBytes = 64 << 20

// mediaURL accepts only upstream media origins. Validate each hop before
// constructing headers; redirects in either headers or bodies are untrusted.
func mediaURL(raw string, base *url.URL) (*url.URL, bool) {
	if strings.TrimSpace(raw) == "" || len(raw) > 8192 {
		return nil, false
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, false
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "https" || u.User != nil || u.Opaque != "" || (u.Port() != "" && u.Port() != "443") {
		return nil, false
	}
	host := strings.ToLower(u.Hostname())
	if host != "gemini.google.com" && host != "contribution.usercontent.google.com" &&
		host != "googleusercontent.com" && !strings.HasSuffix(host, ".googleusercontent.com") &&
		host != "googlevideo.com" && !strings.HasSuffix(host, ".googlevideo.com") {
		return nil, false
	}
	u.Fragment = ""
	return u, true
}

// DownloadMedia fetches generated media without forwarding account cookies
// outside their Google origin. A fallback URL follows the same rules.
func (ac *AccountClient) DownloadMedia(ctx context.Context, rawURL, fallbackURL string) ([]byte, string, error) {
	var lastErr error
	var sess *transport.Session
	for i, raw := range [...]string{rawURL, fallbackURL} {
		if raw == "" || (i == 1 && raw == rawURL) {
			continue
		}
		u, ok := mediaURL(raw, nil)
		if !ok {
			lastErr = fmt.Errorf("media URL is not an allowed HTTPS URL")
			continue
		}
		if sess == nil {
			var err error
			sess, err = ac.acquireSession(ctx)
			if err != nil {
				return nil, "", err
			}
		}
		for hop := range 10 {
			authenticated := strings.HasSuffix(strings.ToLower(u.Hostname()), ".google.com")
			var startGen uint64
			headers := map[string]string{"referer": Origin + "/"}
			if authenticated {
				ac.mu.RLock()
				headers["cookie"] = ac.cookieHeader
				startGen = ac.cookieGen.Load()
				ac.mu.RUnlock()
			}
			res, err := sess.Client.Do(ctx, transport.RequestOptions{
				Method: "GET", URL: u.String(), Kind: transport.ReqKindNavigate, Headers: headers,
				Timeout: 60 * time.Second, MaxBodyBytes: maxMediaDownloadBytes,
			})
			if err != nil {
				if ctx.Err() != nil {
					return nil, "", err
				}
				lastErr = err
				break
			}
			if authenticated {
				ac.absorbAndPersistCookiesWithGen(res.SetCookies, startGen, true)
			}
			var location string
			switch res.Status {
			case 301, 302, 303, 307, 308:
				location = strings.TrimSpace(res.Headers["location"])
			case 200:
				ct := res.Headers["content-type"]
				if strings.HasPrefix(strings.ToLower(ct), "text/plain") {
					location = strings.TrimSpace(res.Body)
				} else if res.Body != "" {
					if ct == "" {
						ct = "application/octet-stream"
					}
					return []byte(res.Body), ct, nil
				}
			}
			if location == "" {
				lastErr = fmt.Errorf("media download failed (HTTP %d)", res.Status)
				break
			}
			next, allowed := mediaURL(location, u)
			if !allowed {
				lastErr = fmt.Errorf("media redirect is not an allowed HTTPS URL")
				break
			}
			if next.String() == u.String() || hop == 9 {
				lastErr = fmt.Errorf("media redirect limit exceeded")
				break
			}
			u = next
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("media URL is required")
	}
	return nil, "", lastErr
}
