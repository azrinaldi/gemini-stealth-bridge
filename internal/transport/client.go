package transport

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// RequestKind selects the exact Chrome header set and wire ordering for a request.
type RequestKind int

const (
	ReqKindCORS RequestKind = iota
	ReqKindNavigate
	ReqKindIframe
)

// ErrStopStreaming may be returned by RequestOptions.OnChunk to stop reading
// the upstream response body immediately once the logical stream is complete.
var ErrStopStreaming = errors.New("stop streaming")

// DefaultMaxBodyBytes bounds buffered RPC responses. Media downloads set a
// separate, larger limit.
const DefaultMaxBodyBytes int64 = 32 << 20

// UpstreamError is a typed error carrying HTTP status, OpenAI/bridge error code,
// and retry-related upstream response headers.
type UpstreamError struct {
	Message    string
	StatusCode int
	Code       string
	Headers    map[string]string
	Cause      error
}

func (e *UpstreamError) Error() string {
	return e.Message
}

func (e *UpstreamError) Unwrap() error {
	return e.Cause
}

// NewUpstreamError constructs a typed UpstreamError.
func NewUpstreamError(msg string, status int, code string, headers map[string]string, cause error) *UpstreamError {
	if status == 0 {
		status = 502
	}
	if code == "" {
		code = "upstream_transport_error"
	}
	return &UpstreamError{
		Message:    msg,
		StatusCode: status,
		Code:       code,
		Headers:    headers,
		Cause:      cause,
	}
}

// ResolveProfile maps a profile name to a bogdanfinn/tls-client ClientProfile.
// Defaults to Chrome_150_PSK (BoringSSL + Post-Quantum X25519MLKEM768 + ECH GREASE + ALPS + Brotli + PSK).
func ResolveProfile(name string) profiles.ClientProfile {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "chrome_152_psk", "chrome152_psk":
		return profiles.Chrome_152_PSK
	case "chrome_152", "chrome152":
		return profiles.Chrome_152
	case "chrome_150_psk", "chrome150_psk", "chrome_latest", "":
		return profiles.Chrome_150_PSK
	case "chrome_150", "chrome150":
		return profiles.Chrome_150
	case "chrome_146_psk", "chrome146_psk":
		return profiles.Chrome_146_PSK
	case "chrome_146", "chrome146":
		return profiles.Chrome_146
	case "chrome_144_psk", "chrome144_psk":
		return profiles.Chrome_144_PSK
	case "chrome_144", "chrome144":
		return profiles.Chrome_144
	case "chrome_133_psk", "chrome133_psk":
		return profiles.Chrome_133_PSK
	case "chrome_133", "chrome133":
		return profiles.Chrome_133
	case "chrome_131_psk", "chrome131_psk":
		return profiles.Chrome_131_PSK
	case "chrome_131", "chrome131":
		return profiles.Chrome_131
	case "brave_146_psk", "brave":
		return profiles.Brave_146_PSK
	case "firefox_148", "firefox":
		return profiles.Firefox_148
	case "firefox_147_psk":
		return profiles.Firefox_147_PSK
	case "safari_16_0", "safari":
		return profiles.Safari_16_0
	default:
		return profiles.Chrome_150_PSK
	}
}

// Doer abstracts the underlying HTTP execution for testing and live uTLS requests.
type Doer interface {
	Do(req *fhttp.Request) (*fhttp.Response, error)
	CloseIdleConnections()
}

// ClientConfig configures a per-account StealthClient.
type ClientConfig struct {
	ProfileName            string
	ProxyURL               string
	UserAgent              string
	SecCHUA                string
	SecCHUAPlatform        string
	SecCHUAFullVersionList string
	SecCHUAArch            string
	SecCHUABitness         string
	SecCHUAPlatformVersion string
	AcceptLanguage         string
	TLSExtOrder            string // "fixed" | "random"
	FollowRedirects        bool
	IPv6Mode               string // "auto" | "off" | "on"
	DumpRequestDir         string
	Timeout                time.Duration
	FirstByteTimeout       time.Duration
	StallTimeout           time.Duration
	CustomDoer             Doer
}

// StealthClient is an isolated, per-account HTTP/2 + uTLS client that reproduces
// a single Chrome BrowserContext's network stack without launching a browser.
type StealthClient struct {
	mu      sync.Mutex
	cfg     ClientConfig
	client  Doer
	dumpSeq atomic.Uint64
}

// NewStealthClient creates an isolated StealthClient for one Google account.
func NewStealthClient(cfg ClientConfig) (*StealthClient, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.FirstByteTimeout <= 0 {
		cfg.FirstByteTimeout = 25 * time.Second
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = 90 * time.Second
	}
	sc := &StealthClient{cfg: cfg}
	if cfg.CustomDoer != nil {
		sc.client = cfg.CustomDoer
		return sc, nil
	}
	c, err := buildTLSClient(cfg)
	if err != nil {
		return nil, err
	}
	sc.client = c
	return sc, nil
}

func buildTLSClient(cfg ClientConfig) (tls_client.HttpClient, error) {
	timeoutSec := int(cfg.Timeout.Seconds())
	if timeoutSec < 30 {
		timeoutSec = 30
	}
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(timeoutSec),
		tls_client.WithDialer(net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 15 * time.Second,
		}),
		tls_client.WithClientProfile(ResolveProfile(cfg.ProfileName)),
		tls_client.WithNotFollowRedirects(),
	}
	if strings.EqualFold(strings.TrimSpace(cfg.TLSExtOrder), "random") {
		opts = append(opts, tls_client.WithRandomTLSExtensionOrder())
	}
	ipv6Mode := strings.ToLower(strings.TrimSpace(cfg.IPv6Mode))
	if ipv6Mode == "off" || (ipv6Mode == "" && strings.TrimSpace(os.Getenv("BRIDGE_DISABLE_IPV6")) == "1") {
		opts = append(opts, tls_client.WithDisableIPV6())
	}
	if cfg.ProxyURL != "" {
		opts = append(opts, tls_client.WithProxyUrl(cfg.ProxyURL))
	}
	return tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
}

// ResetConnections closes idle HTTP/2 connections and rebuilds the uTLS client
// when a fresh socket is needed after a stall or timeout.
func (sc *StealthClient) ResetConnections() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.client != nil {
		sc.client.CloseIdleConnections()
	}
	if sc.cfg.CustomDoer == nil {
		if c, err := buildTLSClient(sc.cfg); err == nil {
			sc.client = c
		}
	}
}

func (sc *StealthClient) getDoer() Doer {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.client
}

// ProxyURL returns the proxy this client routes through, or "" when it dials
// directly. It exists so callers can verify which egress an account uses.
func (sc *StealthClient) ProxyURL() string {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.cfg.ProxyURL
}

// CloseIdleConnections releases the client's idle connections without
// rebuilding it, so an in-flight request is not disturbed.
func (sc *StealthClient) CloseIdleConnections() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.client != nil {
		sc.client.CloseIdleConnections()
	}
}

// RequestOptions specifies parameters for an outbound HTTP request.
type RequestOptions struct {
	Method           string
	URL              string
	Kind             RequestKind
	Headers          map[string]string
	Body             []byte
	Timeout          time.Duration
	FirstByteTimeout time.Duration
	StallTimeout     time.Duration
	FreshSocket      bool
	// MaxBodyBytes bounds a buffered (non-streaming) response body; zero uses
	// DefaultMaxBodyBytes. Exceeding it fails fast with upstream_response_too_large
	// instead of buffering an unbounded body.
	MaxBodyBytes int64
	// OnHeaders runs synchronously once response headers have been received and
	// before the body is read, for any status code (including non-200). It marks
	// the boundary where the first-byte deadline stops and the body phase (stall
	// and any caller-owned no-frame watchdog) begins. It is not called when no
	// response was received (transport error, first-byte timeout, cancellation).
	OnHeaders func(status int)
	OnChunk   func(chunk string) error
}

// Response captures the result of an outbound HTTP request.
type Response struct {
	Status     int
	Headers    map[string]string
	SetCookies []string
	Body       string
	TTFBMs     int64
}

// Do executes a request with Chrome uTLS/H2 fingerprinting, ordered headers,
// first-byte timeout, and stall detection.
func (sc *StealthClient) Do(ctx context.Context, opts RequestOptions) (*Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, NewUpstreamError("request cancelled", 499, "client_cancelled", nil, err)
	}
	if opts.FreshSocket {
		sc.ResetConnections()
	}

	method := opts.Method
	if method == "" {
		method = fhttp.MethodGet
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = sc.cfg.Timeout
	}
	firstByteTimeout := opts.FirstByteTimeout
	if firstByteTimeout == 0 {
		firstByteTimeout = sc.cfg.FirstByteTimeout
	} else if firstByteTimeout < 0 {
		firstByteTimeout = 0
	}
	stallTimeout := opts.StallTimeout
	if stallTimeout <= 0 {
		stallTimeout = sc.cfg.StallTimeout
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		timedOutFirstByte atomic.Bool
		stalled           atomic.Bool
	)

	var bodyReader io.Reader
	if len(opts.Body) > 0 {
		bodyReader = bytes.NewReader(opts.Body)
	}
	req, err := fhttp.NewRequestWithContext(reqCtx, method, opts.URL, bodyReader)
	if err != nil {
		return nil, NewUpstreamError(err.Error(), 502, "upstream_transport_error", nil, err)
	}

	hdrMap := make(map[string]string, len(opts.Headers)+1)
	for k, v := range opts.Headers {
		hdrMap[k] = v
	}
	if len(opts.Body) > 0 {
		req.ContentLength = int64(len(opts.Body))
		if _, hasCL := hdrMap["content-length"]; !hasCL {
			if _, hasCLUpper := hdrMap["Content-Length"]; !hasCLUpper {
				hdrMap["content-length"] = strconv.Itoa(len(opts.Body))
			}
		}
	}

	sc.applyOrderedChromeHeaders(req, opts.Kind, hdrMap)
	sc.dumpRequestIfEnabled(req, opts.Kind, opts.Body)

	// First-byte timer. It fires only while waiting for HTTP response headers; a
	// response that arrives with headers (any status) wins the race
	// deterministically because the timer is disarmed under the same lock that
	// records the outcome. timedOutFirstByte is therefore only ever set when no
	// headers arrived.
	var (
		fbMu      sync.Mutex
		fbTimer   *time.Timer
		fbArmed   bool
		fbExpired bool
		fbStop    = func() {
			fbMu.Lock()
			defer fbMu.Unlock()
			fbArmed = false
			if fbTimer != nil {
				fbTimer.Stop()
			}
		}
		fbDisarm = func() bool {
			fbMu.Lock()
			defer fbMu.Unlock()
			fbArmed = false
			if fbTimer != nil {
				fbTimer.Stop()
			}
			return fbExpired
		}
	)
	if firstByteTimeout > 0 {
		fbMu.Lock()
		fbTimer = time.AfterFunc(firstByteTimeout, func() {
			fbMu.Lock()
			if !fbArmed {
				fbMu.Unlock()
				return
			}
			fbArmed = false
			fbExpired = true
			fbMu.Unlock()
			timedOutFirstByte.Store(true)
			cancel()
		})
		fbArmed = true
		fbMu.Unlock()
		defer fbStop()
	}

	sendAt := time.Now()
	type doResult struct {
		resp *fhttp.Response
		err  error
	}
	doneCh := make(chan doResult, 1)
	doer := sc.getDoer()
	go func() {
		r, e := doer.Do(req)
		doneCh <- doResult{r, e}
	}()
	var resp *fhttp.Response
	select {
	case res := <-doneCh:
		resp, err = res.resp, res.err
	case <-reqCtx.Done():
		expiredFirstByte := fbDisarm()
		received := false
		if !expiredFirstByte {
			// Our own deadlines did not expire: this is a caller cancellation or the
			// per-request timeout. If the response was already handed back, prefer it
			// so a race between headers and cancellation cannot discard a delivered
			// response.
			select {
			case res := <-doneCh:
				resp, err = res.resp, res.err
				received = true
			default:
			}
		}
		if resp == nil {
			// Always close the body of a response that lands after we walked away, or
			// the connection is never returned to the pool. The shared per-account
			// pool itself is left alone: a cancelled peer request must not tear down
			// connections another request on the same account is using. Recovery uses
			// the explicit FreshSocket flag instead.
			if !received {
				go func() {
					if res := <-doneCh; res.resp != nil && res.resp.Body != nil {
						_ = res.resp.Body.Close()
					}
				}()
			}
			if expiredFirstByte {
				timedOutFirstByte.Store(true)
			}
			if received && err != nil {
				return nil, sc.classifyErr(err, ctx, reqCtx, &timedOutFirstByte, &stalled, stallTimeout, nil)
			}
			return nil, sc.classifyErr(reqCtx.Err(), ctx, reqCtx, &timedOutFirstByte, &stalled, stallTimeout, nil)
		}
	}
	if err != nil {
		fbDisarm()
		return nil, sc.classifyErr(err, ctx, reqCtx, &timedOutFirstByte, &stalled, stallTimeout, nil)
	}
	if fbDisarm() {
		// The first-byte timer won the race: it fired before we observed these
		// headers, so the request context is already cancelled and no body read can
		// succeed. Report the deadline instead of letting a dead context surface as
		// a generic transport error, and release the body.
		timedOutFirstByte.Store(true)
		_ = resp.Body.Close()
		return nil, sc.classifyErr(reqCtx.Err(), ctx, reqCtx, &timedOutFirstByte, &stalled, stallTimeout, nil)
	}
	stopBodyCancel := context.AfterFunc(reqCtx, func() {
		_ = resp.Body.Close()
	})
	defer stopBodyCancel()
	defer resp.Body.Close()
	respHeaders := extractHeaders(resp.Header)
	setCookies := resp.Header.Values("Set-Cookie")
	if len(setCookies) == 0 {
		if scVal := resp.Header.Get("set-cookie"); scVal != "" {
			setCookies = []string{scVal}
		}
	}

	// Headers are in: the first-byte deadline is over for every status code, so a
	// slow body (model thinking, streamed media) is governed by the stall timer
	// and the caller's own body-phase watchdog instead.
	streaming := opts.OnChunk != nil && resp.StatusCode == 200
	if opts.OnHeaders != nil {
		opts.OnHeaders(resp.StatusCode)
	}
	var (
		stallMu      sync.Mutex
		stallTimer   *time.Timer
		stallEpoch   uint64
		stallStopped bool
		armStall     = func() {
			if !streaming || stallTimeout <= 0 {
				return
			}
			stallMu.Lock()
			defer stallMu.Unlock()
			if stallStopped {
				return
			}
			stallEpoch++
			epoch := stallEpoch
			if stallTimer != nil {
				stallTimer.Stop()
			}
			stallTimer = time.AfterFunc(stallTimeout, func() {
				stallMu.Lock()
				if stallStopped || epoch != stallEpoch {
					stallMu.Unlock()
					return
				}
				stallStopped = true
				stallMu.Unlock()
				stalled.Store(true)
				cancel()
			})
		}
		stopStall = func() {
			stallMu.Lock()
			stallStopped = true
			stallEpoch++
			t := stallTimer
			stallMu.Unlock()
			if t != nil {
				t.Stop()
			}
		}
	)
	if streaming && stallTimeout > 0 {
		armStall()
		defer stopStall()
	}

	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	if !streaming && resp.ContentLength > maxBody {
		return nil, NewUpstreamError(
			fmt.Sprintf("upstream response body too large: %d byte limit exceeded", maxBody),
			502,
			"upstream_response_too_large",
			respHeaders,
			nil,
		)
	}

	var (
		fullBuf bytes.Buffer
		ttfbMs  int64 = -1
		readBuf       = make([]byte, 32*1024)
	)

	for {
		n, rerr := resp.Body.Read(readBuf)
		if n > 0 {
			chunkStr := string(readBuf[:n])
			if streaming {
				if !isHeartbeatOrLengthChunk(chunkStr) {
					if ttfbMs < 0 {
						ttfbMs = time.Since(sendAt).Milliseconds()
					}
					armStall()
				}
				if cbErr := opts.OnChunk(chunkStr); cbErr != nil {
					if errors.Is(cbErr, ErrStopStreaming) {
						stopStall()
						cancel()
						break
					}
					var ue *UpstreamError
					if errors.As(cbErr, &ue) {
						if ue.Headers == nil {
							ue.Headers = respHeaders
						}
						return nil, ue
					}
					return nil, NewUpstreamError(cbErr.Error(), 502, "upstream_callback_error", respHeaders, cbErr)
				}
			} else {
				if ttfbMs < 0 {
					ttfbMs = time.Since(sendAt).Milliseconds()
				}
				if int64(fullBuf.Len())+int64(n) > maxBody {
					return nil, NewUpstreamError(
						fmt.Sprintf("upstream response body too large: %d byte limit exceeded", maxBody),
						502,
						"upstream_response_too_large",
						respHeaders,
						nil,
					)
				}
				fullBuf.Write(readBuf[:n])
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, sc.classifyErr(rerr, ctx, reqCtx, &timedOutFirstByte, &stalled, stallTimeout, respHeaders)
		}
	}

	if ttfbMs < 0 {
		ttfbMs = time.Since(sendAt).Milliseconds()
	}

	return &Response{
		Status:     resp.StatusCode,
		Headers:    respHeaders,
		SetCookies: setCookies,
		Body:       fullBuf.String(),
		TTFBMs:     ttfbMs,
	}, nil
}

func (sc *StealthClient) classifyErr(
	err error,
	callerCtx, reqCtx context.Context,
	timedOutFirstByte, stalled *atomic.Bool,
	stallTimeout time.Duration,
	headers map[string]string,
) error {
	if callerCtx != nil && callerCtx.Err() != nil {
		return NewUpstreamError("request cancelled", 499, "client_cancelled", headers, err)
	}
	// Deliberately no connection reset here. A cancellation, a per-request
	// timeout, or a body stall says nothing about the shared per-account pool:
	// peers may be streaming over it right now. Callers that want a clean socket
	// after a same-account retry ask for it explicitly via RequestOptions.FreshSocket.
	if stalled.Load() {
		secs := int(stallTimeout.Seconds())
		if secs <= 0 {
			secs = 90
		}
		return NewUpstreamError(
			fmt.Sprintf("upstream produced no data for %ds", secs),
			504,
			"upstream_stalled",
			headers,
			err,
		)
	}
	if timedOutFirstByte.Load() {
		return NewUpstreamError("upstream HTTPS first-byte timeout", 504, "upstream_connect_timeout", headers, err)
	}
	if reqCtx != nil && errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
		return NewUpstreamError("upstream HTTPS timeout", 504, "upstream_timeout", headers, err)
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		if ue.Headers == nil {
			ue.Headers = headers
		}
		return ue
	}
	return NewUpstreamError(err.Error(), 502, "upstream_transport_error", headers, err)
}

// isHeartbeatOrLengthChunk mirrors browser-transport.js's stall-timer check:
// length prefixes, )]}' prefixes, and ["wrb.fr",null,null,...] keepalive frames
// do not reset the stall timer.
func isHeartbeatOrLengthChunk(chunk string) bool {
	stripped := strings.TrimSpace(strings.ReplaceAll(chunk, ")]}'", ""))
	if stripped == "" {
		return true
	}
	if isDigitsOnly(stripped) {
		return true
	}
	if strings.Contains(chunk, `[["wrb.fr",null,null,`) && !strings.Contains(chunk, `[["wrb.fr",null,"[`) {
		return true
	}
	return false
}

func isDigitsOnly(s string) bool {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		for i := range len(line) {
			if line[i] < '0' || line[i] > '9' {
				return false
			}
		}
	}
	return true
}

func extractHeaders(h fhttp.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vals := range h {
		if len(vals) > 0 {
			out[strings.ToLower(k)] = vals[0]
		}
	}
	return out
}

func (sc *StealthClient) applyOrderedChromeHeaders(req *fhttp.Request, kind RequestKind, extra map[string]string) {
	ua := sc.cfg.UserAgent
	if ua == "" {
		ua = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"
	}
	chUA := sc.cfg.SecCHUA
	if chUA == "" {
		chUA = `"Google Chrome";v="150", "Chromium";v="150", "Not?A_Brand";v="24"`
	}
	platform := sc.cfg.SecCHUAPlatform
	if platform == "" {
		platform = `"Linux"`
	}
	acceptLang := sc.cfg.AcceptLanguage
	if acceptLang == "" {
		acceptLang = "en-US,en;q=0.9"
	}

	ver := extractChromeMajor(chUA, ua)
	fullVerList := sc.cfg.SecCHUAFullVersionList
	if fullVerList == "" {
		fullVerList = fmt.Sprintf(`"Google Chrome";v="%s.0.0.0", "Chromium";v="%s.0.0.0", "Not?A_Brand";v="24.0.0.0"`, ver, ver)
	}
	arch := sc.cfg.SecCHUAArch
	if arch == "" {
		arch = `"x86"`
	}
	bitness := sc.cfg.SecCHUABitness
	if bitness == "" {
		bitness = `"64"`
	}
	platVer := sc.cfg.SecCHUAPlatformVersion
	if platVer == "" {
		switch strings.ToLower(strings.Trim(platform, `"`)) {
		case "windows":
			platVer = `"15.0.0"`
		case "macos", "darwin":
			platVer = `"14.6.1"`
		default:
			platVer = `"6.12.0"`
		}
	}

	hdr := fhttp.Header{}
	set := func(k, v string) {
		if v != "" {
			hdr[k] = []string{v}
		}
	}

	switch kind {
	case ReqKindNavigate:
		set("sec-ch-ua", chUA)
		set("sec-ch-ua-mobile", "?0")
		set("sec-ch-ua-platform", platform)
		set("upgrade-insecure-requests", "1")
		set("user-agent", ua)
		set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
		set("sec-fetch-site", "none")
		set("sec-fetch-mode", "navigate")
		set("sec-fetch-user", "?1")
		set("sec-fetch-dest", "document")
		set("accept-encoding", "gzip, deflate, br, zstd")
		set("accept-language", acceptLang)
		set("priority", "u=0, i")
	case ReqKindIframe:
		set("sec-ch-ua", chUA)
		set("sec-ch-ua-mobile", "?0")
		set("sec-ch-ua-platform", platform)
		set("upgrade-insecure-requests", "1")
		set("user-agent", ua)
		set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
		set("sec-fetch-site", "same-site")
		set("sec-fetch-mode", "navigate")
		set("sec-fetch-user", "?1")
		set("sec-fetch-dest", "iframe")
		set("referer", "https://gemini.google.com/")
		set("accept-encoding", "gzip, deflate, br, zstd")
		set("accept-language", acceptLang)
		set("priority", "u=0, i")
	default: // ReqKindCORS
		set("sec-ch-ua", chUA)
		set("x-same-domain", "1")
		set("sec-ch-ua-mobile", "?0")
		set("sec-ch-ua-full-version-list", fullVerList)
		set("sec-ch-ua-arch", arch)
		set("sec-ch-ua-platform-version", platVer)
		set("sec-ch-ua-model", `""`)
		set("sec-ch-ua-bitness", bitness)
		set("sec-ch-ua-wow64", "?0")
		set("user-agent", ua)
		set("sec-ch-ua-platform", platform)
		set("accept", "*/*")
		set("origin", "https://gemini.google.com")
		set("sec-fetch-site", "same-origin")
		set("sec-fetch-mode", "cors")
		set("sec-fetch-dest", "empty")
		set("referer", "https://gemini.google.com/app")
		set("accept-encoding", "gzip, deflate, br, zstd")
		set("accept-language", acceptLang)
		set("priority", "u=1, i")
	}

	var customKeys []string
	for k, v := range extra {
		lk := strings.ToLower(k)
		if v == "" {
			delete(hdr, lk)
			continue
		}
		hdr[lk] = []string{v}
		customKeys = append(customKeys, lk)
	}

	var baseOrder []string
	switch kind {
	case ReqKindNavigate, ReqKindIframe:
		baseOrder = []string{
			"sec-ch-ua",
			"sec-ch-ua-mobile",
			"sec-ch-ua-platform",
			"upgrade-insecure-requests",
			"user-agent",
			"accept",
			"sec-fetch-site",
			"sec-fetch-mode",
			"sec-fetch-user",
			"sec-fetch-dest",
			"referer",
			"accept-encoding",
			"accept-language",
			"cookie",
			"priority",
		}
	default:
		baseOrder = []string{
			"sec-ch-ua",
			"x-same-domain",
			"sec-ch-ua-mobile",
			"sec-ch-ua-full-version-list",
			"sec-ch-ua-arch",
			"sec-ch-ua-platform-version",
			"sec-ch-ua-model",
			"sec-ch-ua-bitness",
			"sec-ch-ua-wow64",
			"user-agent",
			"content-type",
			"content-length",
			"x-goog-ext-525001261-jspb",
			"x-goog-ext-525005358-jspb",
			"x-goog-ext-73010989-jspb",
			"x-goog-ext-73010990-jspb",
			"x-tenant-id",
			"push-id",
			"x-client-pctx",
			"x-goog-upload-command",
			"x-goog-upload-protocol",
			"x-goog-upload-header-content-length",
			"x-goog-upload-offset",
			"authorization",
			"sec-ch-ua-platform",
			"accept",
			"origin",
			"sec-fetch-site",
			"sec-fetch-mode",
			"sec-fetch-dest",
			"referer",
			"accept-encoding",
			"accept-language",
			"cookie",
			"priority",
		}
	}
	seen := make(map[string]bool, len(baseOrder)+len(customKeys))
	var order []string
	for _, k := range baseOrder {
		if _, exists := hdr[k]; exists {
			order = append(order, k)
			seen[k] = true
		}
	}
	for _, k := range customKeys {
		if !seen[k] {
			if _, exists := hdr[k]; exists {
				order = append(order, k)
				seen[k] = true
			}
		}
	}
	hdr[fhttp.HeaderOrderKey] = order
	hdr[fhttp.PHeaderOrderKey] = []string{":method", ":authority", ":scheme", ":path"}
	req.Header = hdr
}

func extractChromeMajor(chUA, ua string) string {
	if idx := strings.Index(chUA, `;v="`); idx >= 0 {
		rest := chUA[idx+4:]
		if end := strings.IndexByte(rest, '"'); end > 0 {
			return rest[:end]
		}
	}
	if idx := strings.Index(ua, "Chrome/"); idx >= 0 {
		rest := ua[idx+7:]
		if dot := strings.IndexByte(rest, '.'); dot > 0 {
			return rest[:dot]
		}
	}
	return "150"
}

// RequestDump is the redacted JSON representation of an outbound HTTP request
// produced when BRIDGE_DUMP_REQUEST is set.
type RequestDump struct {
	Timestamp     string              `json:"timestamp"`
	Method        string              `json:"method"`
	URL           string              `json:"url"`
	Kind          string              `json:"kind"`
	HeaderOrder   []string            `json:"headerOrder"`
	PseudoOrder   []string            `json:"pseudoOrder"`
	Headers       map[string][]string `json:"headers"`
	CookieNames   []string            `json:"cookieNames"`
	CookieHashes  map[string]string   `json:"cookieHashes"`
	ContentLength int                 `json:"contentLength"`
	BodySHA256    string              `json:"bodySha256"`
	TLSExtOrder   string              `json:"tlsExtOrder"`
}

func (r RequestKind) String() string {
	switch r {
	case ReqKindNavigate:
		return "navigate"
	case ReqKindIframe:
		return "iframe"
	default:
		return "cors"
	}
}

func (sc *StealthClient) dumpRequestIfEnabled(req *fhttp.Request, kind RequestKind, body []byte) {
	dir := strings.TrimSpace(sc.cfg.DumpRequestDir)
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv("BRIDGE_DUMP_REQUEST"))
	}
	if dir == "" || req == nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}

	redactedHeaders := make(map[string][]string, len(req.Header))
	var cookieNames []string
	cookieHashes := make(map[string]string)

	for k, vals := range req.Header {
		if k == fhttp.HeaderOrderKey || k == fhttp.PHeaderOrderKey {
			continue
		}
		lk := strings.ToLower(k)
		switch lk {
		case "cookie":
			var redactedParts []string
			for _, rawCookie := range vals {
				for _, part := range strings.Split(rawCookie, ";") {
					part = strings.TrimSpace(part)
					if part == "" {
						continue
					}
					name, val, ok := strings.Cut(part, "=")
					name = strings.TrimSpace(name)
					if !ok || name == "" {
						continue
					}
					sum := sha256.Sum256([]byte(val))
					shortHash := "sha256:" + hex.EncodeToString(sum[:])[:8]
					cookieNames = append(cookieNames, name)
					cookieHashes[name] = shortHash
					redactedParts = append(redactedParts, name+"="+shortHash)
				}
			}
			redactedHeaders[lk] = []string{strings.Join(redactedParts, "; ")}
		case "authorization":
			redactedHeaders[lk] = []string{"SAPISIDHASH <redacted>"}
		default:
			cp := make([]string, len(vals))
			copy(cp, vals)
			redactedHeaders[lk] = cp
		}
	}

	var bodyHash string
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		bodyHash = hex.EncodeToString(sum[:])
	}

	tlsOrder := strings.ToLower(strings.TrimSpace(sc.cfg.TLSExtOrder))
	if tlsOrder == "" {
		tlsOrder = "fixed"
	}

	dump := RequestDump{
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
		Method:        req.Method,
		URL:           req.URL.String(),
		Kind:          kind.String(),
		HeaderOrder:   append([]string(nil), req.Header[fhttp.HeaderOrderKey]...),
		PseudoOrder:   append([]string(nil), req.Header[fhttp.PHeaderOrderKey]...),
		Headers:       redactedHeaders,
		CookieNames:   cookieNames,
		CookieHashes:  cookieHashes,
		ContentLength: len(body),
		BodySHA256:    bodyHash,
		TLSExtOrder:   tlsOrder,
	}

	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		return
	}
	seq := sc.dumpSeq.Add(1)
	fname := fmt.Sprintf("req-%d-%d.json", time.Now().UnixNano(), seq)
	_ = os.WriteFile(filepath.Join(dir, fname), data, 0o600)
}
