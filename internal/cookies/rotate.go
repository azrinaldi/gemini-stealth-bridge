package cookies

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

const (
	rotatePageURL = "https://accounts.google.com/RotateCookiesPage?og_pid=658&rot=3&origin=https%3A%2F%2Fgemini.google.com&exp_id=0"
	rotatePostURL = "https://accounts.google.com/RotateCookies"
	// Sentinel payload for renewing __Secure-1PSIDTS / __Secure-3PSIDTS.
	// Must be sent verbatim with leading zeros ([000,...]), never json.Marshal'd.
	rotate1PSIDTSSentinel = `[000,"-0000000000000000000"]`
	min1PSIDTSInterval    = 60 * time.Second
	maxRotateInterval     = 3600 * time.Second
	defaultRotateInterval = 9 * time.Minute
)

var (
	// rotationCookieSet sends the full canonical cookie jar so both SIDCC and
	// __Secure-1PSIDTS rotate in a single /RotateCookies call.
	rotationCookieSet = EssentialNames
	rotateInitRe      = regexp.MustCompile(`init\('([^']{4,64})'\s*,\s*([0-9.]+)\s*,[^)]*?([0-9.]+)\s*\)`)
)

// ClampRotateInterval clamps a rotation interval into [60s, 3600s].
// If d <= 0, defaultDur (or 9m if defaultDur <= 0) is used and clamped.
func ClampRotateInterval(d, defaultDur time.Duration) time.Duration {
	if d <= 0 {
		d = defaultDur
	}
	if d <= 0 {
		d = defaultRotateInterval
	}
	if d < 60*time.Second {
		return 60 * time.Second
	}
	if d > 3600*time.Second {
		return 3600 * time.Second
	}
	return d
}

// Rotator performs dual-path Google session keepalive over the account's own
// isolated StealthClient:
//  1. Sentinel POST /RotateCookies ([000,"-0000000000000000000"]) to renew
//     short-lived __Secure-1PSIDTS / __Secure-3PSIDTS (~20-30 min TTL).
//  2. Iframe GET /RotateCookiesPage?og_pid=658 -> POST /RotateCookies ([658,"<id>"])
//     to renew SIDCC / __Secure-1PSIDCC / __Secure-3PSIDCC.
type Rotator struct {
	mu           sync.Mutex
	lastPSIDTSAt map[string]time.Time
}

// NewRotator creates a new cookie Rotator with per-account rate-limit guards.
func NewRotator() *Rotator {
	return &Rotator{
		lastPSIDTSAt: make(map[string]time.Time),
	}
}

// RotateResult reports the outcome of a dual-path cookie rotation.
type RotateResult struct {
	Status       int
	Rotated      bool
	CookieHeader string
	SetCookies   []string
	NextInterval time.Duration
	RetryAfter   string // Retry-After from the response that set Status.
}

// Forget removes per-account rotation cooldown state for key.
func (r *Rotator) Forget(key string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.lastPSIDTSAt, key)
	r.mu.Unlock()
}

// RotateAccount executes both the 1PSIDTS sentinel rotation and the 658 iframe
// SIDCC rotation using the account's dedicated StealthClient.
func (r *Rotator) RotateAccount(ctx context.Context, accountID string, currentCookie string, sc *transport.StealthClient) (*RotateResult, error) {
	cookie := currentCookie
	rotatedAny := false
	lastStatus := 0
	var retryAfter string
	var setCookies []string
	nextInterval := defaultRotateInterval

	// Primary path: POST /RotateCookies with [000,"-0000000000000000000"] and full cookie jar.
	// Renews both __Secure-1PSIDTS / __Secure-3PSIDTS and SIDCC / __Secure-*PSIDCC in a single call.
	// Calling /RotateCookies twice in the same minute triggers HTTP 429 on the second call,
	// so return early during the 60s cooldown instead of falling through to iframe rotation.
	if !r.allow1PSIDTS(accountID) {
		return &RotateResult{
			Status:       0,
			Rotated:      false,
			CookieHeader: cookie,
			NextInterval: ClampRotateInterval(nextInterval, defaultRotateInterval),
		}, nil
	}

	subset := Subset(cookie, rotationCookieSet)
	if subset != "" {
		res, err := sc.Do(ctx, transport.RequestOptions{
			Method: "POST",
			URL:    rotatePostURL,
			Kind:   transport.ReqKindCORS,
			Headers: map[string]string{
				"content-type":   "application/json",
				"origin":         "https://accounts.google.com",
				"referer":        "https://accounts.google.com/",
				"sec-fetch-site": "same-origin",
				"cookie":         subset,
			},
			Body:    []byte(rotate1PSIDTSSentinel),
			Timeout: 20 * time.Second,
		})
		if err == nil {
			lastStatus = res.Status
			retryAfter = res.Headers["retry-after"]
			r.note1PSIDTS(accountID, res.Status)
			if res.Status == 200 {
				var primarySetCookies []string
				if len(res.SetCookies) > 0 {
					primarySetCookies = append([]string(nil), res.SetCookies...)
				}
				if merged, changed := AbsorbSetCookie(cookie, res.SetCookies); changed {
					cookie = merged
					if hasEssentialCookieUpdate(res.SetCookies) {
						rotatedAny = true
					}
				}
				return &RotateResult{
					Status:       res.Status,
					Rotated:      rotatedAny,
					CookieHeader: cookie,
					SetCookies:   primarySetCookies,
					NextInterval: ClampRotateInterval(nextInterval, defaultRotateInterval),
					RetryAfter:   retryAfter,
				}, nil
			}
			if res.Status == 401 || res.Status == 403 || res.Status == 429 {
				return &RotateResult{
					Status:       res.Status,
					Rotated:      false,
					CookieHeader: cookie,
					NextInterval: ClampRotateInterval(nextInterval, defaultRotateInterval),
					RetryAfter:   retryAfter,
				}, nil
			}
		}
	}

	// Fallback path: Browser iframe RotateCookiesPage (og_pid=658) -> SIDCC / *PSIDCC rotation.
	var pageErr error
	pageRes, pageErr := sc.Do(ctx, transport.RequestOptions{
		Method: "GET",
		URL:    rotatePageURL,
		Kind:   transport.ReqKindIframe,
		Headers: map[string]string{
			"cookie": cookie,
		},
		Timeout: 20 * time.Second,
	})
	if pageErr == nil {
		if lastStatus == 0 || pageRes.Status == 401 || pageRes.Status == 403 || pageRes.Status == 429 {
			lastStatus = pageRes.Status
			retryAfter = pageRes.Headers["retry-after"]
		}
		if pageRes.Status == 200 {
			if len(pageRes.SetCookies) > 0 {
				setCookies = append(setCookies, pageRes.SetCookies...)
			}
			if merged, changed := AbsorbSetCookie(cookie, pageRes.SetCookies); changed {
				cookie = merged
				if hasEssentialCookieUpdate(pageRes.SetCookies) {
					rotatedAny = true
				}
			}
			if m := rotateInitRe.FindStringSubmatch(pageRes.Body); len(m) >= 4 {
				rotID := m[1]
				if sec, err := strconv.ParseFloat(m[3], 64); err == nil && sec > 0 {
					if sec >= float64(maxRotateInterval/time.Second) {
						nextInterval = maxRotateInterval
					} else {
						nextInterval = time.Duration(sec * float64(time.Second))
						if nextInterval <= 0 {
							nextInterval = time.Nanosecond
						}
					}
				}
				body := fmt.Sprintf(`[658,%q]`, rotID)
				postRes, postErr := sc.Do(ctx, transport.RequestOptions{
					Method: "POST",
					URL:    rotatePostURL,
					Kind:   transport.ReqKindCORS,
					Headers: map[string]string{
						"content-type":   "application/json",
						"origin":         "https://accounts.google.com",
						"referer":        rotatePageURL,
						"sec-fetch-site": "same-origin",
						"sec-fetch-mode": "same-origin",
						"cookie":         cookie,
					},
					Body:    []byte(body),
					Timeout: 20 * time.Second,
				})
				if postErr != nil {
					return nil, postErr
				}
				lastStatus = postRes.Status
				retryAfter = postRes.Headers["retry-after"]
				if postRes.Status == 200 {
					if len(postRes.SetCookies) > 0 {
						setCookies = append(setCookies, postRes.SetCookies...)
					}
					if merged, changed := AbsorbSetCookie(cookie, postRes.SetCookies); changed {
						cookie = merged
						if hasEssentialCookieUpdate(postRes.SetCookies) {
							rotatedAny = true
						}
					}
				} else {
					rotatedAny = false
					setCookies = nil
					cookie = currentCookie
				}
			}
		}
	}
	if lastStatus == 0 && pageErr != nil {
		return nil, pageErr
	}
	if lastStatus != 200 {
		rotatedAny = false
		setCookies = nil
	}
	return &RotateResult{
		Status:       lastStatus,
		Rotated:      rotatedAny,
		CookieHeader: cookie,
		SetCookies:   setCookies,
		NextInterval: ClampRotateInterval(nextInterval, defaultRotateInterval),
		RetryAfter:   retryAfter,
	}, nil
}

func (r *Rotator) allow1PSIDTS(accountID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.lastPSIDTSAt[accountID]
	if !ok {
		return true
	}
	return time.Since(last) >= min1PSIDTSInterval
}

func (r *Rotator) note1PSIDTS(accountID string, status int) {
	if status == 200 || status == 401 || status == 403 || status == 429 {
		r.mu.Lock()
		r.lastPSIDTSAt[accountID] = time.Now()
		r.mu.Unlock()
	}
}
