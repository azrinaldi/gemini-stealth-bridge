package gemini

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// frameWatchdog bounds a StreamGenerate that never yields a candidate frame.
//
// The transport-level stall timer cannot cover this case. Gemini answers a
// request that is going nowhere with length prefixes and empty `wrb.fr`
// envelopes: real bytes on the wire, so every transport timer is satisfied,
// while the caller sees nothing at all. A watchdog keyed on parsed frames is the
// only place that can tell "the model is thinking" from "the model will never
// answer".
//
// The deadline covers the body phase only. Response headers have their own
// transport deadline (FirstByteTimeout), and starting this timer before headers
// arrive would race it: a slow connect would be reported as a frame stall and
// the failover path would treat a healthy account as broken. Arm is therefore
// called from the transport's OnHeaders callback, once the body is about to be
// read.
//
// Disarming and releasing are deliberately separate operations. The timer fires
// by cancelling the request context, so it shares that context with the stream;
// stopping the timer must not cancel it, or the first delivered frame would tear
// down the very stream it just proved healthy.
const (
	frameWatchPending  int32 = 0
	frameWatchArmed    int32 = 1
	frameWatchDisarmed int32 = 2
	frameWatchFired    int32 = 3
)

type frameWatchdog struct {
	cancel context.CancelFunc

	mu    sync.Mutex
	timer *time.Timer
	epoch uint64
	state atomic.Int32

	disarm  sync.Once
	release sync.Once
}

// newFrameWatchdog arms a watchdog that cancels parent when deadline elapses
// before Disarm. It returns the derived context the request must use.
func newFrameWatchdog(parent context.Context, deadline time.Duration) (context.Context, *frameWatchdog) {
	ctx, w := newPendingFrameWatchdog(parent)
	w.Arm(deadline)
	return ctx, w
}

// newPendingFrameWatchdog derives a context without starting a deadline. The
// caller arms it (usually once response headers have arrived) or releases it.
func newPendingFrameWatchdog(parent context.Context) (context.Context, *frameWatchdog) {
	ctx, cancel := context.WithCancel(parent)
	return ctx, &frameWatchdog{cancel: cancel}
}

// Arm starts the deadline. It is a no-op once the watchdog was disarmed,
// released, or already fired, and it never extends an armed deadline.
func (w *frameWatchdog) Arm(deadline time.Duration) {
	if w == nil || deadline <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.state.CompareAndSwap(frameWatchPending, frameWatchArmed) {
		return
	}
	w.startLocked(deadline)
}

// Reset restarts an armed deadline. Upstream counts as progressing when it sends
// a candidate frame or its own activity text, so a stream that is answering but
// has not finished is not abandoned merely because one phase was slow.
func (w *frameWatchdog) Reset(deadline time.Duration) {
	if w == nil || deadline <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state.Load() != frameWatchArmed {
		return
	}
	// A callback from the previous deadline may already be running; the epoch
	// bump makes it a no-op instead of racing this reset.
	if w.timer != nil {
		w.timer.Stop()
	}
	w.startLocked(deadline)
}

// startLocked installs a fresh timer. Callers hold w.mu.
func (w *frameWatchdog) startLocked(deadline time.Duration) {
	w.epoch++
	epoch := w.epoch
	w.timer = time.AfterFunc(deadline, func() { w.expire(epoch) })
}

func (w *frameWatchdog) expire(epoch uint64) {
	w.mu.Lock()
	if epoch != w.epoch {
		w.mu.Unlock()
		return
	}
	fired := w.state.CompareAndSwap(frameWatchArmed, frameWatchFired)
	w.mu.Unlock()
	if !fired {
		return
	}
	// Only the derived context is cancelled: the caller's request may still
	// be retried on another account.
	w.cancel()
}

// Disarm stops the deadline without cancelling the request. Call it as soon as
// the stream has produced a candidate frame, or when the request finished on
// its own.
func (w *frameWatchdog) Disarm() {
	if w == nil {
		return
	}
	w.disarm.Do(func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if !w.state.CompareAndSwap(frameWatchArmed, frameWatchDisarmed) {
			w.state.CompareAndSwap(frameWatchPending, frameWatchDisarmed)
		}
		if w.timer != nil {
			w.timer.Stop()
		}
	})
}

// Release frees the derived context. It is safe to call more than once and from
// any goroutine.
func (w *frameWatchdog) Release() {
	if w == nil {
		return
	}
	w.Disarm()
	w.release.Do(w.cancel)
}

// Expired reports whether the deadline fired before Disarm.
func (w *frameWatchdog) Expired() bool {
	return w != nil && w.state.Load() == frameWatchFired
}
