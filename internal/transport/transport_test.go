package transport

import (
	"context"
	"encoding/json"
	"errors"
	fhttp "github.com/bogdanfinn/fhttp"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stubDoer struct {
	mu      sync.Mutex
	fn      func(req *fhttp.Request) (*fhttp.Response, error)
	closed  int
	lastReq *fhttp.Request
}

func (d *stubDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	d.mu.Lock()
	d.lastReq = req
	fn := d.fn
	d.mu.Unlock()
	return fn(req)
}

func (d *stubDoer) CloseIdleConnections() {
	d.mu.Lock()
	d.closed++
	d.mu.Unlock()
}

func TestStealthClientOrderedHeaders(t *testing.T) {
	var captured *fhttp.Request
	doer := &stubDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			captured = req
			return &fhttp.Response{
				StatusCode: 200,
				Header: fhttp.Header{
					"Content-Type": []string{"application/json"},
					"Set-Cookie":   []string{"__Secure-1PSIDTS=new_ts; Path=/; Secure; HttpOnly"},
				},
				Body: io.NopCloser(strings.NewReader("ok")),
			}, nil
		},
	}

	sc, err := NewStealthClient(ClientConfig{
		ProfileName: "chrome_150_psk",
		CustomDoer:  doer,
	})
	if err != nil {
		t.Fatalf("NewStealthClient: %v", err)
	}

	res, err := sc.Do(context.Background(), RequestOptions{
		Method: "POST",
		URL:    "https://gemini.google.com/_/BardChatUi/data/batchexecute",
		Kind:   ReqKindCORS,
		Headers: map[string]string{
			"cookie":                    "SAPISID=abc",
			"x-goog-ext-525001261-jspb": "[1]",
		},
		Body: []byte("f.req=%5B%5D"),
	})
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	if res.Status != 200 || res.Body != "ok" {
		t.Fatalf("unexpected response: %+v", res)
	}
	if len(res.SetCookies) != 1 {
		t.Fatalf("expected 1 Set-Cookie, got %v", res.SetCookies)
	}
	pOrder := captured.Header[fhttp.PHeaderOrderKey]
	if len(pOrder) != 4 || pOrder[0] != ":method" || pOrder[1] != ":authority" {
		t.Fatalf("unexpected pseudo-header order: %v", pOrder)
	}
	hOrder := captured.Header[fhttp.HeaderOrderKey]
	if len(hOrder) == 0 || hOrder[0] != "sec-ch-ua" {
		t.Fatalf("expected sec-ch-ua first in header order, got %v", hOrder)
	}
}

func TestSessionLeaseQueueAndDeferredCookie(t *testing.T) {
	sc, _ := NewStealthClient(ClientConfig{
		CustomDoer: &stubDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader("ok")),
				}, nil
			},
		},
	})
	sess := NewSession("acct1", sc, 1, "SAPISID=initial", 2)

	var seenCookies []string
	var mu sync.Mutex
	releaseFirst := make(chan struct{})
	firstEntered := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := sess.Lease(context.Background(), func(cookieHeader string) error {
			mu.Lock()
			seenCookies = append(seenCookies, cookieHeader)
			mu.Unlock()
			close(firstEntered)
			<-releaseFirst
			return nil
		})
		if err != nil {
			t.Errorf("first lease error: %v", err)
		}
	}()

	<-firstEntered
	// Queue a rotated cookie while the first request is in flight.
	sess.QueueCookieUpdate("SAPISID=rotated")

	go func() {
		defer wg.Done()
		err := sess.Lease(context.Background(), func(cookieHeader string) error {
			mu.Lock()
			seenCookies = append(seenCookies, cookieHeader)
			mu.Unlock()
			return nil
		})
		if err != nil {
			t.Errorf("second lease error: %v", err)
		}
	}()

	// Wait briefly so second request is queued, then check queue depth enforcement.
	time.Sleep(30 * time.Millisecond)
	close(releaseFirst)
	wg.Wait()

	if len(seenCookies) != 2 || seenCookies[0] != "SAPISID=initial" || seenCookies[1] != "SAPISID=rotated" {
		t.Fatalf("expected [SAPISID=initial, SAPISID=rotated], got %v", seenCookies)
	}
}

func TestSessionQueueOverflowAndPoolReset(t *testing.T) {
	pool := NewPool(PoolOptions{
		ClientConfig: ClientConfig{
			CustomDoer: &stubDoer{
				fn: func(req *fhttp.Request) (*fhttp.Response, error) {
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{},
						Body:       io.NopCloser(strings.NewReader("ok")),
					}, nil
				},
			},
		},
		MaxSessions: 2,
		MaxQueue:    1,
		WaitTimeout: 80 * time.Millisecond,
	})

	sess, err := pool.Acquire(context.Background(), "acct", "SAPISID=1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	hold := make(chan struct{})
	started := make(chan struct{})
	var doneWg sync.WaitGroup
	doneWg.Add(2)

	// Holder #1 (active)
	go func() {
		defer doneWg.Done()
		_ = sess.Lease(context.Background(), func(_ string) error {
			close(started)
			<-hold
			return nil
		})
	}()
	<-started

	// Holder #2 (fills the 1-slot wait queue)
	var queuedFlag atomic.Bool
	go func() {
		defer doneWg.Done()
		queuedFlag.Store(true)
		_ = sess.Lease(context.Background(), func(_ string) error {
			<-hold
			return nil
		})
	}()
	for !queuedFlag.Load() || sess.Queued() < 1 {
		time.Sleep(5 * time.Millisecond)
	}

	// Holder #3 should be rejected with 429 rate_limit_error because MaxQueue == 1
	err3 := sess.Lease(context.Background(), func(_ string) error { return nil })
	var ue *UpstreamError
	if !errors.As(err3, &ue) || ue.StatusCode != 429 {
		t.Fatalf("expected 429 queue overflow error, got %v", err3)
	}

	close(hold)
	doneWg.Wait()

	pool.Reset()
	if len(pool.Status()) != 0 {
		t.Fatalf("expected 0 sessions after Reset, got %v", pool.Status())
	}
}

func TestSessionLeaseWaitTimeoutReturns429SessionBusy(t *testing.T) {
	pool := NewPool(PoolOptions{
		ClientConfig: ClientConfig{CustomDoer: &stubDoer{}},
		MaxSessions:  4,
		MaxQueue:     4,
		WaitTimeout:  40 * time.Millisecond,
	})
	sess, err := pool.Acquire(context.Background(), "acct-wait", "SID=1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	hold := make(chan struct{})
	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = sess.Lease(context.Background(), func(_ string) error {
			close(started)
			<-hold
			return nil
		})
	}()
	<-started

	errWait := sess.Lease(context.Background(), func(_ string) error { return nil })
	var ue *UpstreamError
	if !errors.As(errWait, &ue) || ue.StatusCode != 429 || ue.Code != "session_busy" || ue.Headers["retry-after"] != "2" {
		t.Fatalf("expected 429 session_busy with retry-after=2 after WaitTimeout, got %+v (%v)", ue, errWait)
	}

	close(hold)
	wg.Wait()

	// Subsequent lease after holder releases must succeed cleanly
	if errAfter := sess.Lease(context.Background(), func(_ string) error { return nil }); errAfter != nil {
		t.Fatalf("expected lease after release to succeed, got %v", errAfter)
	}
}

func TestNavigateVsCORSHeaderOrdering(t *testing.T) {
	var captured []*fhttp.Request
	doer := &stubDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			captured = append(captured, req)
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{},
				Body:       io.NopCloser(strings.NewReader("ok")),
			}, nil
		},
	}
	sc, err := NewStealthClient(ClientConfig{CustomDoer: doer})
	if err != nil {
		t.Fatalf("NewStealthClient: %v", err)
	}
	_, _ = sc.Do(context.Background(), RequestOptions{
		Method:  "GET",
		URL:     "https://gemini.google.com/app",
		Kind:    ReqKindNavigate,
		Headers: map[string]string{"cookie": "SID=1"},
	})
	_, _ = sc.Do(context.Background(), RequestOptions{
		Method:  "POST",
		URL:     "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate",
		Kind:    ReqKindCORS,
		Headers: map[string]string{"content-type": "application/x-www-form-urlencoded;charset=utf-8", "cookie": "SID=1"},
	})
	if len(captured) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(captured))
	}
	navOrder := captured[0].Header[fhttp.HeaderOrderKey]
	idxUpgrade, idxUA := -1, -1
	for i, k := range navOrder {
		if k == "upgrade-insecure-requests" {
			idxUpgrade = i
		}
		if k == "user-agent" {
			idxUA = i
		}
	}
	if idxUpgrade < 0 || idxUA < 0 || idxUpgrade >= idxUA {
		t.Fatalf("expected upgrade-insecure-requests before user-agent in Navigate order: %v", navOrder)
	}
}

func TestConcurrentPoolAcquireAndLeaseRaceFree(t *testing.T) {
	pool := NewPool(PoolOptions{
		MaxSessions: 4,
		MaxQueue:    8,
	})
	defer pool.Reset()
	ctx := context.Background()

	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range 50 {
				s, err := pool.Acquire(ctx, "shared-account", "SID=worker")
				if err != nil {
					t.Errorf("Acquire failed: %v", err)
					return
				}
				_ = s.Lease(ctx, func(_ string) error {
					if i%2 == 0 {
						s.QueueCookieUpdate("SID=updated")
					}
					return nil
				})
				_ = s.CookieHeader()
			}
		}(worker)
	}
	wg.Wait()
}

func TestRequestDumpRedactsCookies(t *testing.T) {
	tmpDir := t.TempDir()
	doer := &stubDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{},
				Body:       io.NopCloser(strings.NewReader("ok")),
			}, nil
		},
	}
	sc, err := NewStealthClient(ClientConfig{
		ProfileName:    "chrome_150_psk",
		DumpRequestDir: tmpDir,
		CustomDoer:     doer,
	})
	if err != nil {
		t.Fatalf("NewStealthClient: %v", err)
	}

	rawCookieSecret := "SUPER_SECRET_COOKIE_VALUE_12345"
	_, err = sc.Do(context.Background(), RequestOptions{
		Method: "POST",
		URL:    "https://gemini.google.com/_/BardChatUi/data/batchexecute",
		Kind:   ReqKindCORS,
		Headers: map[string]string{
			"content-type":  "application/x-www-form-urlencoded;charset=utf-8",
			"cookie":        "SID=" + rawCookieSecret + "; __Secure-1PSIDTS=ts_secret_val",
			"authorization": "SAPISIDHASH 1700000000_deadbeef",
		},
		Body: []byte("f.req=%5B%5D&at=xsrf"),
	})
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 dump file in %s, got %d (err=%v)", tmpDir, len(entries), err)
	}
	rawBytes, err := os.ReadFile(filepath.Join(tmpDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	rawStr := string(rawBytes)
	if strings.Contains(rawStr, rawCookieSecret) || strings.Contains(rawStr, "ts_secret_val") || strings.Contains(rawStr, "deadbeef") {
		t.Fatalf("dump leaked raw cookie or auth token: %s", rawStr)
	}

	var dump RequestDump
	if err := json.Unmarshal(rawBytes, &dump); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	idxCT, idxCL, idxAE, idxAL := -1, -1, -1, -1
	for i, h := range dump.HeaderOrder {
		switch h {
		case "content-type":
			idxCT = i
		case "content-length":
			idxCL = i
		case "accept-encoding":
			idxAE = i
		case "accept-language":
			idxAL = i
		}
	}
	if idxCT < 0 || idxCL != idxCT+1 {
		t.Fatalf("expected content-length immediately after content-type in HeaderOrder, got %v", dump.HeaderOrder)
	}
	if idxAE < 0 || idxAL < 0 || idxAE >= idxAL {
		t.Fatalf("expected accept-encoding before accept-language in HeaderOrder, got %v", dump.HeaderOrder)
	}
	if dump.Headers["authorization"][0] != "SAPISIDHASH <redacted>" {
		t.Fatalf("unexpected authorization header in dump: %v", dump.Headers["authorization"])
	}
}

func TestSessionLeaseCancellationAndTimeoutNoGoroutineLeak(t *testing.T) {
	sessTimeout := NewSession("leak-timeout", nil, 1, "SID=1", 200, 5*time.Millisecond)
	sessCancel := NewSession("leak-cancel", nil, 1, "SID=1", 200, 2*time.Second)

	hold := make(chan struct{})
	startHolder := func(s *Session) chan error {
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- s.Lease(context.Background(), func(_ string) error {
				close(started)
				<-hold
				return nil
			})
		}()
		<-started
		return done
	}
	holderTimeoutDone := startHolder(sessTimeout)
	holderCancelDone := startHolder(sessCancel)

	// Warm up one cancelled call so runtime stabilizes before sampling baseline.
	warmCtx, warmCancel := context.WithCancel(context.Background())
	warmCancel()
	_ = sessCancel.Lease(warmCtx, func(_ string) error { return nil })
	runtime.Gosched()
	baseline := runtime.NumGoroutine()

	// While slot 1 is held open on each session, fire 50 timeouts and 50 context cancellations.
	for i := range 50 {
		errWait := sessTimeout.Lease(context.Background(), func(_ string) error { return nil })
		var ue *UpstreamError
		if !errors.As(errWait, &ue) || ue.StatusCode != 429 || ue.Code != "session_busy" {
			t.Fatalf("iter %d: expected 429 session_busy, got %v", i, errWait)
		}

		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		errCancel := sessCancel.Lease(cctx, func(_ string) error { return nil })
		cancel()
		if !errors.As(errCancel, &ue) || ue.StatusCode != 499 || ue.Code != "client_cancelled" {
			t.Fatalf("iter %d: expected 499 client_cancelled, got %v", i, errCancel)
		}
	}

	runtime.Gosched()
	time.Sleep(10 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > baseline+5 {
		t.Fatalf("goroutine leak detected during Session.Lease timeouts/cancellations: baseline=%d after=%d", baseline, after)
	}
	for _, s := range []*Session{sessTimeout, sessCancel} {
		if s.Active() != 1 || s.Queued() != 0 {
			t.Fatalf("unexpected session %s counts while holder active: active=%d queued=%d", s.Name, s.Active(), s.Queued())
		}
	}

	close(hold)
	for _, ch := range []chan error{holderTimeoutDone, holderCancelDone} {
		if err := <-ch; err != nil {
			t.Fatalf("holder returned error: %v", err)
		}
	}
	for _, s := range []*Session{sessTimeout, sessCancel} {
		if s.Active() != 0 {
			t.Fatalf("expected active=0 on %s after holder release, got %d", s.Name, s.Active())
		}
		if err := s.Lease(context.Background(), func(_ string) error { return nil }); err != nil {
			t.Fatalf("fresh lease on %s after holder release failed: %v", s.Name, err)
		}
	}
}

func TestSessionLeaseZeroValueStructInitializesSemSafely(t *testing.T) {
	s := &Session{
		Name:          "zero-val",
		MaxQueueDepth: 2,
		WaitTimeout:   100 * time.Millisecond,
	}
	var inside atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_ = s.Lease(context.Background(), func(_ string) error {
					if cur := inside.Add(1); cur > 1 {
						t.Errorf("mutual exclusion violated on zero-value Session: inside=%d", cur)
					}
					time.Sleep(time.Millisecond)
					inside.Add(-1)
					return nil
				})
			}
		}()
	}
	wg.Wait()
}

func TestSessionPendingCookieVersionRejectsStaleQueueAndAcquire(t *testing.T) {
	pool := NewPool(PoolOptions{
		ClientConfig: ClientConfig{CustomDoer: &stubDoer{}},
		MaxSessions:  4,
		MaxQueue:     4,
	})
	defer pool.Reset()

	ctx := context.Background()
	sess, err := pool.AcquireWithProxyWithGen(ctx, "acct-ver", "SID=v1", "", 1)
	if err != nil {
		t.Fatalf("AcquireWithProxyWithGen: %v", err)
	}

	// Queue newer generation 3.
	sess.QueueCookieUpdateWithGen("SID=v3", 3)
	if got := sess.CookieHeader(); got != "SID=v3" {
		t.Fatalf("expected SID=v3 pending, got %q", got)
	}

	// Stale generation 2 must be ignored.
	sess.QueueCookieUpdateWithGen("SID=v2_stale", 2)
	if got := sess.CookieHeader(); got != "SID=v3" {
		t.Fatalf("stale QueueCookieUpdateWithGen clobbered pending cookie: got %q", got)
	}

	// Stale AcquireWithProxyWithGen (gen=2) must NOT clobber gen=3.
	sess2, err := pool.AcquireWithProxyWithGen(ctx, "acct-ver", "SID=v2_stale", "", 2)
	if err != nil || sess2 != sess {
		t.Fatalf("expected same session, got err=%v", err)
	}
	if got := sess.CookieHeader(); got != "SID=v3" {
		t.Fatalf("stale AcquireWithProxyWithGen clobbered pending cookie: got %q", got)
	}

	// Legacy unversioned AcquireWithProxy must NOT clobber non-empty pendingCookie.
	if _, err := pool.AcquireWithProxy(ctx, "acct-ver", "SID=legacy_stale", ""); err != nil {
		t.Fatalf("AcquireWithProxy: %v", err)
	}
	if got := sess.CookieHeader(); got != "SID=v3" {
		t.Fatalf("unversioned AcquireWithProxy clobbered pending cookie: got %q", got)
	}

	// Lease promotes pendingCookie and preserves generation 3; stale gen=2 after Lease is still rejected.
	var leased string
	if err := sess.Lease(ctx, func(c string) error {
		leased = c
		return nil
	}); err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if leased != "SID=v3" {
		t.Fatalf("expected leased SID=v3, got %q", leased)
	}
	sess.QueueCookieUpdateWithGen("SID=v2_after_lease", 2)
	if got := sess.CookieHeader(); got != "SID=v3" {
		t.Fatalf("stale QueueCookieUpdateWithGen after Lease clobbered cookie: got %q", got)
	}

	// Newer gen=4 succeeds via AcquireWithProxyWithGen and promotes on RunStream.
	if _, err := pool.AcquireWithProxyWithGen(ctx, "acct-ver", "SID=v4", "", 4); err != nil {
		t.Fatalf("AcquireWithProxyWithGen gen 4: %v", err)
	}
	var streamed string
	if err := sess.RunStream(ctx, func(c string) error {
		streamed = c
		return nil
	}); err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if streamed != "SID=v4" {
		t.Fatalf("expected streamed SID=v4, got %q", streamed)
	}
}

func TestSessionUnversionedQueueCookieUpdatePreservesPendingCookie(t *testing.T) {
	pool := NewPool(PoolOptions{
		ClientConfig: ClientConfig{CustomDoer: &stubDoer{}},
		MaxSessions:  4,
		MaxQueue:     4,
	})
	defer pool.Reset()

	ctx := context.Background()

	// Case A: unversioned QueueCookieUpdate("SID=stale") must not overwrite newer pendingCookie.
	sessA, err := pool.AcquireWithProxyWithGen(ctx, "acct-unver-a", "SID=v1", "", 1)
	if err != nil {
		t.Fatalf("AcquireWithProxyWithGen Case A: %v", err)
	}
	sessA.QueueCookieUpdateWithGen("SID=v10", 10)
	sessA.QueueCookieUpdate("SID=stale")
	if got := sessA.CookieHeader(); got != "SID=v10" {
		t.Fatalf("Case A: unversioned QueueCookieUpdate overwrote pending cookie: got %q, want %q", got, "SID=v10")
	}
	sessA.mu.Lock()
	pendingA, pendingGenA, cookieGenA := sessA.pendingCookie, sessA.pendingCookieGen, sessA.cookieGen
	sessA.mu.Unlock()
	if pendingA != "SID=v10" || pendingGenA != 10 || cookieGenA != 1 {
		t.Fatalf("Case A: unexpected session state pending=%q pendingGen=%d cookieGen=%d", pendingA, pendingGenA, cookieGenA)
	}
	var leasedA string
	if err := sessA.Lease(ctx, func(c string) error {
		leasedA = c
		return nil
	}); err != nil {
		t.Fatalf("Case A Lease: %v", err)
	}
	if leasedA != "SID=v10" {
		t.Fatalf("Case A: expected leased SID=v10, got %q", leasedA)
	}

	// Case B: unversioned QueueCookieUpdate matching active cookieHeader ("SID=v1") must not clear newer pendingCookie.
	sessB, err := pool.AcquireWithProxyWithGen(ctx, "acct-unver-b", "SID=v1", "", 1)
	if err != nil {
		t.Fatalf("AcquireWithProxyWithGen Case B: %v", err)
	}
	sessB.QueueCookieUpdateWithGen("SID=v20", 20)
	sessB.QueueCookieUpdate("SID=v1")
	if got := sessB.CookieHeader(); got != "SID=v20" {
		t.Fatalf("Case B: unversioned QueueCookieUpdate cleared pending cookie: got %q, want %q", got, "SID=v20")
	}
	sessB.mu.Lock()
	pendingB, pendingGenB, cookieGenB := sessB.pendingCookie, sessB.pendingCookieGen, sessB.cookieGen
	sessB.mu.Unlock()
	if pendingB != "SID=v20" || pendingGenB != 20 || cookieGenB != 1 {
		t.Fatalf("Case B: unexpected session state pending=%q pendingGen=%d cookieGen=%d", pendingB, pendingGenB, cookieGenB)
	}
	var leasedB string
	if err := sessB.Lease(ctx, func(c string) error {
		leasedB = c
		return nil
	}); err != nil {
		t.Fatalf("Case B Lease: %v", err)
	}
	if leasedB != "SID=v20" {
		t.Fatalf("Case B: expected leased SID=v20, got %q", leasedB)
	}
}

func TestSessionQueueCookieUpdateWithGenRaceFree(t *testing.T) {
	pool := NewPool(PoolOptions{
		ClientConfig: ClientConfig{CustomDoer: &stubDoer{}},
		MaxSessions:  4,
		MaxQueue:     8,
	})
	defer pool.Reset()
	ctx := context.Background()

	var genSeq atomic.Uint64
	var wg sync.WaitGroup
	for w := range 6 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 40 {
				g := genSeq.Add(1)
				c := "SID=g" + string(rune('a'+(i%20)))
				s, err := pool.AcquireWithProxyWithGen(ctx, "race-acct", c, "", g)
				if err != nil {
					t.Errorf("worker %d acquire failed: %v", worker, err)
					return
				}
				if i%3 == 0 {
					s.QueueCookieUpdateWithGen("SID=rot"+string(rune('0'+(worker%10))), genSeq.Add(1))
				}
				_ = s.Lease(ctx, func(_ string) error { return nil })
				_ = s.RunStream(ctx, func(_ string) error { return nil })
				_ = s.CookieHeader()
			}
		}(w)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// First-byte deadline, OnHeaders boundary, cancellation isolation, body cap
// ---------------------------------------------------------------------------

type bodyFunc func(p []byte) (int, error)

func (f bodyFunc) Read(p []byte) (int, error) { return f(p) }
func (bodyFunc) Close() error                 { return nil }

func newDelayedBody(data string, chunkDelay time.Duration, ctx context.Context) io.ReadCloser {
	sent := false
	return bodyFunc(func(p []byte) (int, error) {
		if sent {
			return 0, io.EOF
		}
		sent = true
		if chunkDelay > 0 {
			select {
			case <-time.After(chunkDelay):
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		return copy(p, data), nil
	})
}

// Headers arriving fast must end the first-byte deadline: a body that stays
// silent longer than FirstByteTimeout is a stall (or a slow model), never a
// header timeout. Non-200 responses must disarm it too.
func TestFirstByteTimeoutEndsAtHeadersForNon200(t *testing.T) {
	var seenStatus int
	doer := &stubDoer{fn: func(req *fhttp.Request) (*fhttp.Response, error) {
		return &fhttp.Response{
			StatusCode: 429,
			Header:     fhttp.Header{"Retry-After": []string{"1"}},
			Body:       newDelayedBody("slow body", 120*time.Millisecond, req.Context()),
		}, nil
	}}
	sc, err := NewStealthClient(ClientConfig{
		CustomDoer:       doer,
		Timeout:          5 * time.Second,
		FirstByteTimeout: 40 * time.Millisecond,
		StallTimeout:     2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewStealthClient: %v", err)
	}
	res, err := sc.Do(context.Background(), RequestOptions{
		Method:    "POST",
		URL:       "https://gemini.google.com/x",
		OnHeaders: func(status int) { seenStatus = status },
	})
	if err != nil {
		t.Fatalf("expected slow body after fast 429 headers to succeed, got %v", err)
	}
	if seenStatus != 429 {
		t.Fatalf("expected OnHeaders(429), got %d", seenStatus)
	}
	if res.Status != 429 || res.Body != "slow body" {
		t.Fatalf("unexpected response: %+v", res)
	}
	if doer.closed != 0 {
		t.Fatalf("successful request must not reset connections, got %d", doer.closed)
	}
}

func TestOnHeadersRunsBeforeBodyAndOnlyWhenHeadersArrive(t *testing.T) {
	order := make(chan string, 4)
	doer := &stubDoer{fn: func(req *fhttp.Request) (*fhttp.Response, error) {
		return &fhttp.Response{
			StatusCode: 200,
			Header:     fhttp.Header{},
			Body: bodyFunc(func(p []byte) (int, error) {
				order <- "body"
				return 0, io.EOF
			}),
		}, nil
	}}
	sc, _ := NewStealthClient(ClientConfig{CustomDoer: doer, FirstByteTimeout: time.Second})
	if _, err := sc.Do(context.Background(), RequestOptions{
		Method:    "GET",
		URL:       "https://gemini.google.com/x",
		OnHeaders: func(int) { order <- "headers" },
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	close(order)
	var got []string
	for s := range order {
		got = append(got, s)
	}
	if len(got) != 2 || got[0] != "headers" || got[1] != "body" {
		t.Fatalf("expected headers before body, got %v", got)
	}

	// No response at all: the first-byte deadline fires, OnHeaders must not run.
	hang := &stubDoer{fn: func(req *fhttp.Request) (*fhttp.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}}
	sc2, _ := NewStealthClient(ClientConfig{CustomDoer: hang, FirstByteTimeout: 30 * time.Millisecond, Timeout: 5 * time.Second})
	called := false
	_, err := sc2.Do(context.Background(), RequestOptions{
		Method:    "GET",
		URL:       "https://gemini.google.com/x",
		OnHeaders: func(int) { called = true },
	})
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 504 || ue.Code != "upstream_connect_timeout" {
		t.Fatalf("expected 504 upstream_connect_timeout, got %+v (%v)", ue, err)
	}
	if called {
		t.Fatal("OnHeaders must not run when no response headers arrived")
	}
}

// A caller cancellation is not evidence against the account's shared pool: it
// must not reset connections and must not leak the late response body.
func TestCancelDoesNotResetPoolAndClosesLateBody(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var bodyClosed atomic.Bool
	doer := &stubDoer{fn: func(req *fhttp.Request) (*fhttp.Response, error) {
		close(entered)
		<-release
		return &fhttp.Response{
			StatusCode: 200,
			Header:     fhttp.Header{},
			Body: &trackedCloser{
				closed: &bodyClosed,
				reader: bodyFunc(func([]byte) (int, error) { return 0, io.EOF }),
			},
		}, nil
	}}
	sc, _ := NewStealthClient(ClientConfig{CustomDoer: doer, FirstByteTimeout: 2 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-entered
		cancel()
	}()
	_, err := sc.Do(ctx, RequestOptions{Method: "GET", URL: "https://gemini.google.com/x"})
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 499 || ue.Code != "client_cancelled" {
		t.Fatalf("expected 499 client_cancelled, got %+v (%v)", ue, err)
	}
	doer.mu.Lock()
	closed := doer.closed
	doer.mu.Unlock()
	if closed != 0 {
		t.Fatalf("caller cancellation must not reset connections, got %d", closed)
	}
	close(release)
	waitFor(t, 2*time.Second, bodyClosed.Load)
	if !bodyClosed.Load() {
		t.Fatal("late response body must be closed, not leaked")
	}
}

// An explicit FreshSocket still resets the pool; that is the supported way for
// a same-account retry to get a clean socket.
func TestFreshSocketStillResetsConnections(t *testing.T) {
	doer := &stubDoer{fn: func(req *fhttp.Request) (*fhttp.Response, error) {
		return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}}
	sc, _ := NewStealthClient(ClientConfig{CustomDoer: doer, FirstByteTimeout: time.Second})
	if _, err := sc.Do(context.Background(), RequestOptions{Method: "GET", URL: "https://gemini.google.com/x", FreshSocket: true}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	doer.mu.Lock()
	closed := doer.closed
	doer.mu.Unlock()
	if closed == 0 {
		t.Fatal("FreshSocket must reset connections")
	}
}

func TestBufferedBodyCapFailsFast(t *testing.T) {
	const capBytes = 1024
	body := &e2eRepeatingReader{remaining: 8 << 20}
	doer := &stubDoer{fn: func(req *fhttp.Request) (*fhttp.Response, error) {
		return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: body}, nil
	}}
	sc, _ := NewStealthClient(ClientConfig{CustomDoer: doer, FirstByteTimeout: time.Second})
	_, err := sc.Do(context.Background(), RequestOptions{
		Method:       "GET",
		URL:          "https://lh3.googleusercontent.com/huge",
		MaxBodyBytes: capBytes,
	})
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Code != "upstream_response_too_large" {
		t.Fatalf("expected upstream_response_too_large, got %+v (%v)", ue, err)
	}
	if body.read.Load() > 32*1024 {
		t.Fatalf("expected abort within one read buffer, read %d bytes", body.read.Load())
	}
}

func TestDeclaredContentLengthOverCapFailsBeforeRead(t *testing.T) {
	var reads atomic.Int64
	doer := &stubDoer{fn: func(req *fhttp.Request) (*fhttp.Response, error) {
		return &fhttp.Response{
			StatusCode:    200,
			Header:        fhttp.Header{},
			ContentLength: 1 << 30,
			Body: bodyFunc(func([]byte) (int, error) {
				reads.Add(1)
				return 0, io.EOF
			}),
		}, nil
	}}
	sc, _ := NewStealthClient(ClientConfig{CustomDoer: doer, FirstByteTimeout: time.Second})
	_, err := sc.Do(context.Background(), RequestOptions{
		Method:       "GET",
		URL:          "https://lh3.googleusercontent.com/huge",
		MaxBodyBytes: 1 << 20,
	})
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Code != "upstream_response_too_large" {
		t.Fatalf("expected upstream_response_too_large, got %+v (%v)", ue, err)
	}
	if reads.Load() != 0 {
		t.Fatalf("declared oversize Content-Length must fail before any read, got %d reads", reads.Load())
	}
}

// Default cap exists so a plain Do cannot buffer unbounded bodies.
func TestDefaultBodyCapAppliesWithoutExplicitLimit(t *testing.T) {
	if DefaultMaxBodyBytes <= 0 || DefaultMaxBodyBytes >= 300<<20 {
		t.Fatalf("default cap must be positive and below 300 MiB, got %d", DefaultMaxBodyBytes)
	}
	var reads atomic.Int64
	doer := &stubDoer{fn: func(req *fhttp.Request) (*fhttp.Response, error) {
		return &fhttp.Response{
			StatusCode:    200,
			Header:        fhttp.Header{},
			ContentLength: DefaultMaxBodyBytes + 1,
			Body: bodyFunc(func([]byte) (int, error) {
				reads.Add(1)
				return 0, io.EOF
			}),
		}, nil
	}}
	sc, _ := NewStealthClient(ClientConfig{CustomDoer: doer, FirstByteTimeout: time.Second})
	_, err := sc.Do(context.Background(), RequestOptions{Method: "GET", URL: "https://lh3.googleusercontent.com/huge"})
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Code != "upstream_response_too_large" {
		t.Fatalf("expected default cap to reject oversized body, got %+v (%v)", ue, err)
	}
	if reads.Load() != 0 {
		t.Fatalf("default cap must reject before reading, got %d reads", reads.Load())
	}
}

// Body stall semantics are unchanged: heartbeats do not reset the stall timer,
// real data does, and a silent stream reports upstream_stalled without touching
// the shared pool.
func TestStallTimerStillAppliesToStreamingBody(t *testing.T) {
	doer := &stubDoer{fn: func(req *fhttp.Request) (*fhttp.Response, error) {
		sent := false
		return &fhttp.Response{
			StatusCode: 200,
			Header:     fhttp.Header{},
			Body: bodyFunc(func(p []byte) (int, error) {
				if !sent {
					sent = true
					return copy(p, ")]}'\n"), nil
				}
				<-req.Context().Done()
				return 0, req.Context().Err()
			}),
		}, nil
	}}
	sc, _ := NewStealthClient(ClientConfig{
		CustomDoer:       doer,
		Timeout:          5 * time.Second,
		FirstByteTimeout: 30 * time.Millisecond,
		StallTimeout:     60 * time.Millisecond,
	})
	_, err := sc.Do(context.Background(), RequestOptions{
		Method:  "POST",
		URL:     "https://gemini.google.com/StreamGenerate",
		OnChunk: func(string) error { return nil },
	})
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 504 || ue.Code != "upstream_stalled" {
		t.Fatalf("expected 504 upstream_stalled, got %+v (%v)", ue, err)
	}
	doer.mu.Lock()
	closed := doer.closed
	doer.mu.Unlock()
	if closed != 0 {
		t.Fatalf("stall must not reset the shared pool implicitly, got %d resets", closed)
	}
}

type trackedCloser struct {
	closed *atomic.Bool
	reader io.Reader
}

func (c *trackedCloser) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *trackedCloser) Close() error {
	c.closed.Store(true)
	return nil
}

type e2eRepeatingReader struct {
	remaining int64
	read      atomic.Int64
	buf       []byte
}

func (r *e2eRepeatingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if r.buf == nil {
		r.buf = bytesRepeat('A', 32*1024)
	}
	n := len(p)
	if n > len(r.buf) {
		n = len(r.buf)
	}
	if int64(n) > r.remaining {
		n = int(r.remaining)
	}
	copy(p[:n], r.buf[:n])
	r.remaining -= int64(n)
	r.read.Add(int64(n))
	return n, nil
}

func (r *e2eRepeatingReader) Close() error { return nil }

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
