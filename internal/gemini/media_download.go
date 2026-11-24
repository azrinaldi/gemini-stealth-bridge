package gemini

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
	"golang.org/x/net/publicsuffix"
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
	if host != "gemini.google.com" &&
		host != "usercontent.google.com" && !strings.HasSuffix(host, ".usercontent.google.com") &&
		host != "googleusercontent.com" && !strings.HasSuffix(host, ".googleusercontent.com") &&
		host != "googlevideo.com" && !strings.HasSuffix(host, ".googlevideo.com") {
		return nil, false
	}
	u.Fragment = ""
	return u, true
}

// rejectedHost extracts only the hostname of a rejected media URL or
// redirect target, for diagnostics. The full URL is never logged: media
// URLs are signed and act as bearer credentials for the file.
func rejectedHost(raw string) string {
	p, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || p.Hostname() == "" {
		return "(unparseable)"
	}
	return p.Hostname()
}

// DownloadMedia fetches generated media without forwarding account cookies
// outside their Google origin. A fallback URL follows the same rules.
func (ac *AccountClient) DownloadMedia(ctx context.Context, rawURL, fallbackURL string) ([]byte, string, error) {
	var lastErr error
	var sess *transport.Session
	var trace []string
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	cookieOrigin, _ := url.Parse(Origin)
	for i, raw := range [...]string{rawURL, fallbackURL} {
		if raw == "" || (i == 1 && raw == rawURL) {
			continue
		}
		u, ok := mediaURL(raw, nil)
		if !ok {
			lastErr = fmt.Errorf("media URL host %q is not an allowed HTTPS URL", rejectedHost(raw))
			continue
		}
		if strings.HasSuffix(strings.ToLower(u.Hostname()), ".googleusercontent.com") &&
			(strings.HasPrefix(u.Path, "/gg-dl/") || strings.HasPrefix(u.Path, "/rd-gg-dl/")) {
			if !imageSizeSuffixRe.MatchString(u.Path) && !strings.HasSuffix(u.Path, "=d-I") {
				u.Path += "=s0"
				if u.RawPath != "" {
					u.RawPath += "=s0"
				}
			}
			if !u.Query().Has("alr") {
				if u.RawQuery != "" {
					u.RawQuery += "&"
				}
				u.RawQuery += "alr=yes"
			}
		}
		if sess == nil {
			var err error
			sess, err = ac.acquireSession(ctx)
			if err != nil {
				return nil, "", err
			}
		}
		trace = append(trace, fmt.Sprintf("GET %s shape=%s", u.Hostname(), urlShape(u.String())))
		for hop := range 10 {
			host := strings.ToLower(u.Hostname())
			// Scope account cookies to the approved Google/usercontent hosts,
			// not the separate googlevideo CDN.
			authenticated := strings.HasSuffix(host, ".google.com") || host == "googleusercontent.com" || strings.HasSuffix(host, ".googleusercontent.com")
			var startGen uint64
			headers := map[string]string{"referer": Origin + "/", "origin": Origin}
			headers["sec-fetch-site"] = "same-site"
			if host == "googleusercontent.com" || strings.HasSuffix(host, ".googleusercontent.com") || host == "googlevideo.com" || strings.HasSuffix(host, ".googlevideo.com") {
				headers["sec-fetch-site"] = "cross-site"
			}
			headers["x-same-domain"] = ""
			if authenticated {
				ac.mu.RLock()
				cookieHeader := ac.cookieHeader
				startGen = ac.cookieGen.Load()
				ac.mu.RUnlock()
				seed := (&http.Request{Header: http.Header{"Cookie": {cookieHeader}}}).Cookies()
				for _, cookie := range seed {
					cookie.Domain, cookie.Path, cookie.Secure = ".google.com", "/", true
					if strings.HasPrefix(cookie.Name, "__Host-") {
						cookie.Domain = ""
					}
					if cookie.Name == "OSID" || cookie.Name == "__Secure-OSID" || cookie.Name == "COMPASS" {
						cookie.Domain = ""
					}
				}
				jar.SetCookies(cookieOrigin, seed)
			}
			cookieRequest := &http.Request{Header: make(http.Header)}
			hopCookies := jar.Cookies(u)
			for _, cookie := range hopCookies {
				cookieRequest.AddCookie(cookie)
			}
			if cookieHeader := cookieRequest.Header.Get("Cookie"); cookieHeader != "" {
				headers["cookie"] = cookieHeader
			}
			res, err := sess.Client.DoMedia(ctx, transport.RequestOptions{
				Method: "GET", URL: u.String(), Kind: transport.ReqKindCORS, Headers: headers,
				Timeout: 60 * time.Second, MaxBodyBytes: maxMediaDownloadBytes,
			})
			if err != nil {
				if ctx.Err() != nil {
					return nil, "", err
				}
				lastErr = err
				break
			}
			trace = append(trace, fmt.Sprintf("%s->%d(cookies=%d,set_cookies=%d)", u.Hostname(), res.Status, len(hopCookies), len(res.SetCookies)))
			// Preserve media grants and their domain/path/deletion attributes,
			// without adding them to the essential account-cookie store.
			jar.SetCookies(u, (&http.Response{Header: http.Header{"Set-Cookie": res.SetCookies}}).Cookies())
			if host == "gemini.google.com" {
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
					log.Printf("[media] account=%s %s", ac.ID, strings.Join(trace, " "))
					return []byte(res.Body), ct, nil
				}
			}
			if location == "" {
				lastErr = fmt.Errorf("media download failed (HTTP %d)", res.Status)
				break
			}
			next, allowed := mediaURL(location, u)
			if !allowed {
				lastErr = fmt.Errorf("media redirect to host %q is not an allowed HTTPS URL", rejectedHost(location))
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
	return nil, "", fmt.Errorf("%w [media trace: %s]", lastErr, strings.Join(trace, " "))
}

// urlShape describes a media URL's size suffix and query keys for
// diagnostics, without exposing the signed token itself.
func urlShape(raw string) string {
	p, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "(unparseable)"
	}
	suffix := ""
	if strings.HasSuffix(p.Path, "=d-I") {
		suffix = "=d-I"
	} else if i := strings.LastIndex(p.Path, "=s"); i > strings.LastIndex(p.Path, "/") {
		if candidate := p.Path[i:]; len(candidate) <= 64 && imageSizeSuffixRe.MatchString(candidate) {
			suffix = candidate
		} else {
			suffix = "(unrecognized)"
		}
	}
	keys := make([]string, 0, len(p.Query()))
	for k := range p.Query() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Sprintf("suffix=%q query=%v", suffix, keys)
}
