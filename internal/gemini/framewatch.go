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
// Disarming and releasing are deliberately separate operations. The timer fires
// by cancelling the request context, so it shares that context with the stream;
// stopping the timer must not cancel it, or the first delivered frame would tear
// down the very stream it just proved healthy.
type frameWatchdog struct {
	cancel  context.CancelFunc
	timer   *time.Timer
	fired   atomic.Bool
	disarm  sync.Once
	release sync.Once
}

// newFrameWatchdog arms a watchdog that cancels parent when deadline elapses
// before Disarm. It returns the derived context the request must use.
func newFrameWatchdog(parent context.Context, deadline time.Duration) (context.Context, *frameWatchdog) {
	ctx, cancel := context.WithCancel(parent)
	w := &frameWatchdog{cancel: cancel}
	if deadline <= 0 {
		return ctx, w
	}
	w.timer = time.AfterFunc(deadline, func() {
		w.fired.Store(true)
		// Only the derived context is cancelled: the caller's request may still
		// be retried on another account.
		cancel()
	})
	return ctx, w
}

// Disarm stops the deadline without cancelling the request. Call it as soon as
// the stream has produced a frame, or when the request finished on its own.
func (w *frameWatchdog) Disarm() {
	if w == nil {
		return
	}
	w.disarm.Do(func() {
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
	return w != nil && w.fired.Load()
}
