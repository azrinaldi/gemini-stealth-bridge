package transport

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// SessionStatus describes the live state of a per-account stealth transport session.
type SessionStatus struct {
	Key     string `json:"key"`
	Ready   bool   `json:"ready"`
	Busy    bool   `json:"busy"`
	Queued  int    `json:"queued"`
	Active  int    `json:"active"`
	Stale   bool   `json:"stale"`
	IdleSec int64  `json:"idleSec"`
}

// Session owns an isolated per-account StealthClient and serializes generations
// on that account with a bounded wait queue, mirroring BrowserSession without Chrome.
type Session struct {
	Name             string
	Client           *StealthClient
	Generation       uint64
	MaxQueueDepth    int
	WaitTimeout      time.Duration
	proxyURL         string
	mu               sync.Mutex
	ready            bool
	active           int
	reservedUntil    time.Time
	lastUsed         time.Time
	cookieHeader     string
	cookieGen        uint64
	pendingCookie    string
	pendingCookieGen uint64
	execSem          chan struct{}
}

// NewSession constructs a new isolated account session.
func NewSession(name string, client *StealthClient, gen uint64, cookieHeader string, maxQueue int, waitTimeout ...time.Duration) *Session {
	if maxQueue <= 0 {
		maxQueue = 4
	}
	var wt time.Duration
	if len(waitTimeout) > 0 && waitTimeout[0] > 0 {
		wt = waitTimeout[0]
	}
	return &Session{
		Name:          name,
		Client:        client,
		Generation:    gen,
		MaxQueueDepth: maxQueue,
		WaitTimeout:   wt,
		ready:         true,
		lastUsed:      time.Now(),
		cookieHeader:  cookieHeader,
		execSem:       make(chan struct{}, 1),
	}
}

// Busy reports whether the session is currently executing or queueing work.
func (s *Session) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active > 0
}

// Active returns the number of in-flight + queued requests on this session.
func (s *Session) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Queued returns the number of requests waiting behind the currently executing request.
func (s *Session) Queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active <= 1 {
		return 0
	}
	return s.active - 1
}

// QueueCookieUpdate schedules a fresh cookie header to be applied before the next request.
// Unversioned updates never overwrite or clear a non-empty pendingCookie.
func (s *Session) QueueCookieUpdate(cookie string) {
	if cookie == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingCookie != "" {
		return
	}
	if cookie != s.cookieHeader {
		s.pendingCookie = cookie
		s.pendingCookieGen = max(s.cookieGen, s.pendingCookieGen) + 1
	}
}

// QueueCookieUpdateWithGen schedules a versioned cookie header update, ignoring
// stale updates whose generation is not strictly newer than the session's
// current or pending generation.
func (s *Session) QueueCookieUpdateWithGen(cookie string, gen uint64) {
	if cookie == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	curMax := max(s.cookieGen, s.pendingCookieGen)
	if gen <= curMax {
		return
	}
	if cookie != s.cookieHeader {
		s.pendingCookie = cookie
		s.pendingCookieGen = gen
	} else {
		s.pendingCookie = ""
		s.pendingCookieGen = 0
		s.cookieGen = gen
	}
}

// CookieHeader returns the session's active cookie header.
func (s *Session) CookieHeader() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingCookie != "" {
		return s.pendingCookie
	}
	return s.cookieHeader
}

// ProxyURL returns the proxy this session routes through, or "" when it dials
// directly.
func (s *Session) ProxyURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proxyURL
}

// Lease runs fn exclusively under this account session's lease, enforcing
// MaxQueueDepth and context cancellation while queued.
func (s *Session) Lease(ctx context.Context, fn func(cookieHeader string) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return NewUpstreamError("request cancelled", 499, "client_cancelled", nil, err)
	}

	s.mu.Lock()
	if s.execSem == nil {
		s.execSem = make(chan struct{}, 1)
	}
	sem := s.execSem
	waiting := 0
	if s.active > 0 {
		waiting = s.active - 1
	}
	if waiting >= s.MaxQueueDepth {
		s.mu.Unlock()
		return NewUpstreamError(
			fmt.Sprintf("session %s is busy: %d request(s) already queued", s.Name, waiting),
			429,
			"session_busy",
			map[string]string{"retry-after": "2"},
			nil,
		)
	}
	s.active++
	s.reservedUntil = time.Time{}
	s.lastUsed = time.Now()
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.active--
		s.lastUsed = time.Now()
		s.mu.Unlock()
	}()

	// Acquire the 1-slot execution semaphore directly in select so timed-out or
	// cancelled waiters never spawn background goroutines.
	var waitCh <-chan time.Time
	var timer *time.Timer
	if s.WaitTimeout > 0 {
		timer = time.NewTimer(s.WaitTimeout)
		defer timer.Stop()
		waitCh = timer.C
	}

	select {
	case sem <- struct{}{}:
	case <-waitCh:
		return NewUpstreamError(
			fmt.Sprintf("session %s is busy: timed out after %s waiting for active generation", s.Name, s.WaitTimeout),
			429,
			"session_busy",
			map[string]string{"retry-after": "2"},
			nil,
		)
	case <-ctx.Done():
		return NewUpstreamError("request cancelled", 499, "client_cancelled", nil, ctx.Err())
	}

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			<-sem
		})
	}
	defer release()

	if err := ctx.Err(); err != nil {
		return NewUpstreamError("request cancelled", 499, "client_cancelled", nil, err)
	}

	s.mu.Lock()
	s.lastUsed = time.Now()
	if s.pendingCookie != "" {
		if s.pendingCookie != s.cookieHeader {
			s.cookieHeader = s.pendingCookie
		}
		if s.pendingCookieGen > s.cookieGen {
			s.cookieGen = s.pendingCookieGen
		}
		s.pendingCookie = ""
		s.pendingCookieGen = 0
	}
	curCookie := s.cookieHeader
	s.mu.Unlock()

	return fn(curCookie)
}

// RunStream executes an HTTP/2 multiplexed stream on this session without holding
// the single-tab execSem semaphore, allowing up to MaxQueueDepth concurrent streams per account.
func (s *Session) RunStream(ctx context.Context, fn func(cookieHeader string) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return NewUpstreamError("request cancelled", 499, "client_cancelled", nil, err)
	}

	s.mu.Lock()
	limit := s.MaxQueueDepth
	if limit <= 0 {
		limit = 4
	}
	if s.active >= limit {
		s.mu.Unlock()
		return NewUpstreamError(
			fmt.Sprintf("session %s is busy: %d concurrent stream(s) already active", s.Name, s.active),
			429,
			"session_busy",
			map[string]string{"retry-after": "2"},
			nil,
		)
	}
	s.active++
	s.reservedUntil = time.Time{}
	s.lastUsed = time.Now()
	if s.pendingCookie != "" {
		if s.pendingCookie != s.cookieHeader {
			s.cookieHeader = s.pendingCookie
		}
		if s.pendingCookieGen > s.cookieGen {
			s.cookieGen = s.pendingCookieGen
		}
		s.pendingCookie = ""
		s.pendingCookieGen = 0
	}
	curCookie := s.cookieHeader
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.active--
		s.lastUsed = time.Now()
		s.mu.Unlock()
	}()

	return fn(curCookie)
}

// Close releases idle connections held by the session.
func (s *Session) Close() {
	s.Retire()
}

// Retire takes a session out of service without rebuilding its client. An
// in-flight request keeps its connection until it finishes; only the idle
// connections of the retired session are released.
func (s *Session) Retire() {
	s.mu.Lock()
	s.ready = false
	s.pendingCookie = ""
	s.pendingCookieGen = 0
	s.mu.Unlock()
	if s.Client != nil {
		s.Client.CloseIdleConnections()
	}
}

// Pool manages isolated per-account stealth transport sessions with bounded capacity,
// reservation grace windows, and idle reaping.
type Pool struct {
	mu          sync.Mutex
	clientCfg   ClientConfig
	maxSessions int
	maxQueue    int
	waitTimeout time.Duration
	reserveFor  time.Duration
	generation  uint64
	sessions    map[string]*Session
}

// PoolOptions configures a Session Pool.
type PoolOptions struct {
	ClientConfig ClientConfig
	MaxSessions  int
	MaxQueue     int
	WaitTimeout  time.Duration
	ReserveFor   time.Duration
}

// NewPool creates a new per-account stealth transport pool.
func NewPool(opts PoolOptions) *Pool {
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = 16
	}
	if opts.MaxQueue <= 0 {
		opts.MaxQueue = 4
	}
	if opts.WaitTimeout <= 0 {
		opts.WaitTimeout = 5 * time.Second
	}
	if opts.ReserveFor <= 0 {
		opts.ReserveFor = 5 * time.Second
	}
	return &Pool{
		clientCfg:   opts.ClientConfig,
		maxSessions: opts.MaxSessions,
		maxQueue:    opts.MaxQueue,
		waitTimeout: opts.WaitTimeout,
		reserveFor:  opts.ReserveFor,
		sessions:    make(map[string]*Session),
	}
}

// Existing returns a ready session for key if one is currently open.
func (p *Pool) Existing(key string) *Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[key]
	if !ok || s.Generation != p.generation {
		return nil
	}
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if ready {
		return s
	}
	return nil
}

// Acquire returns or creates the isolated Session bound to key, using the
// pool-wide egress configuration.
func (p *Pool) Acquire(ctx context.Context, key, cookieHeader string) (*Session, error) {
	return p.AcquireWithProxy(ctx, key, cookieHeader, "")
}

// AcquireWithProxy returns or creates the isolated Session bound to key,
// routing that session's traffic through proxyURL instead of the pool-wide
// default. Each Google account may sit behind its own proxy, so the proxy is
// a property of the session rather than of the pool.
func (p *Pool) AcquireWithProxy(ctx context.Context, key, cookieHeader, proxyURL string) (*Session, error) {
	return p.acquireWithProxy(ctx, key, cookieHeader, proxyURL, 0, false)
}

// AcquireWithProxyWithGen behaves like AcquireWithProxy while enforcing a
// monotonic cookie generation counter so stale snapshots cannot clobber newer
// cookies queued or promoted on an existing session.
func (p *Pool) AcquireWithProxyWithGen(ctx context.Context, key, cookieHeader, proxyURL string, cookieGen uint64) (*Session, error) {
	return p.acquireWithProxy(ctx, key, cookieHeader, proxyURL, cookieGen, true)
}

func (p *Pool) acquireWithProxy(ctx context.Context, key, cookieHeader, proxyURL string, cookieGen uint64, hasGen bool) (*Session, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(p.waitTimeout)

	for {
		p.mu.Lock()
		if existing, ok := p.sessions[key]; ok {
			existing.mu.Lock()
			isReady := existing.Generation == p.generation && existing.ready
			proxyChanged := existing.proxyURL != proxyURL
			if isReady {
				if hasGen {
					curMax := max(existing.cookieGen, existing.pendingCookieGen)
					if cookieHeader != "" && cookieGen > curMax {
						if cookieHeader != existing.cookieHeader {
							existing.pendingCookie = cookieHeader
							existing.pendingCookieGen = cookieGen
						} else {
							existing.pendingCookie = ""
							existing.pendingCookieGen = 0
							existing.cookieGen = cookieGen
						}
					}
				} else if cookieHeader != "" && cookieHeader != existing.cookieHeader && existing.pendingCookie == "" {
					existing.pendingCookie = cookieHeader
					existing.pendingCookieGen = max(existing.cookieGen, existing.pendingCookieGen) + 1
				}
				existing.reservedUntil = time.Now().Add(p.reserveFor)
			}
			existing.mu.Unlock()
			if isReady && !proxyChanged {
				p.mu.Unlock()
				return existing, nil
			}
			// The account was re-pointed at a different proxy: the old client
			// still holds connections to the previous egress, so drop it.
			delete(p.sessions, key)
			go existing.Retire()
		}

		if len(p.sessions) < p.maxSessions || p.evictLRUIdleLocked() {
			cfg := p.clientCfg
			if proxyURL != "" {
				cfg.ProxyURL = proxyURL
			}
			sc, err := NewStealthClient(cfg)
			if err != nil {
				p.mu.Unlock()
				return nil, err
			}
			s := NewSession(key, sc, p.generation, cookieHeader, p.maxQueue, p.waitTimeout)
			if hasGen {
				s.cookieGen = cookieGen
			}
			s.proxyURL = proxyURL
			s.reservedUntil = time.Now().Add(p.reserveFor)
			p.sessions[key] = s
			p.mu.Unlock()
			return s, nil
		}
		p.mu.Unlock()

		if time.Now().After(deadline) {
			return nil, NewUpstreamError(
				fmt.Sprintf("all %d transport sessions are busy", p.maxSessions),
				429,
				"rate_limit_error",
				nil,
				nil,
			)
		}
		select {
		case <-ctx.Done():
			return nil, NewUpstreamError("request cancelled", 499, "client_cancelled", nil, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (p *Pool) evictLRUIdleLocked() bool {
	now := time.Now()
	var bestKey string
	var bestSession *Session
	var bestLastUsed time.Time
	for k, s := range p.sessions {
		s.mu.Lock()
		idle := s.active == 0 && s.ready && !s.reservedUntil.After(now)
		lu := s.lastUsed
		s.mu.Unlock()
		if !idle {
			continue
		}
		if bestSession == nil || lu.Before(bestLastUsed) {
			bestKey = k
			bestSession = s
			bestLastUsed = lu
		}
	}
	if bestSession == nil {
		return false
	}
	delete(p.sessions, bestKey)
	go bestSession.Close()
	return true
}

// ReapIdle closes sessions that have been idle longer than maxIdle.
func (p *Pool) ReapIdle(maxIdle time.Duration) int {
	now := time.Now()
	p.mu.Lock()
	var toClose []*Session
	for k, s := range p.sessions {
		s.mu.Lock()
		canReap := s.active == 0 && s.ready && !s.reservedUntil.After(now) && now.Sub(s.lastUsed) >= maxIdle
		s.mu.Unlock()
		if canReap {
			delete(p.sessions, k)
			toClose = append(toClose, s)
		}
	}
	p.mu.Unlock()
	for _, s := range toClose {
		s.Close()
	}
	return len(toClose)
}

// Status returns a diagnostic snapshot of all open sessions in the pool.
func (p *Pool) Status() []SessionStatus {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]SessionStatus, 0, len(p.sessions))
	for k, s := range p.sessions {
		s.mu.Lock()
		queued := 0
		if s.active > 1 {
			queued = s.active - 1
		}
		st := SessionStatus{
			Key:     k,
			Ready:   s.ready,
			Busy:    s.active > 0,
			Queued:  queued,
			Active:  s.active,
			Stale:   s.Generation != p.generation,
			IdleSec: int64(now.Sub(s.lastUsed).Seconds()),
		}
		s.mu.Unlock()
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Reset invalidates the current generation and closes all sessions.
func (p *Pool) Reset() {
	p.mu.Lock()
	p.generation++
	old := p.sessions
	p.sessions = make(map[string]*Session)
	p.mu.Unlock()
	for _, s := range old {
		s.Close()
	}
}
