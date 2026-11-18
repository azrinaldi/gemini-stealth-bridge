package gemini

import (
	"context"
	"testing"
	"time"
)

// TestFrameWatchdogCancelsIdleStream pins the behaviour that fixes the carbon
// hang: a stream that never yields a frame must be abandoned, because the
// transport timers cannot see it (keepalive envelopes are real bytes).
func TestFrameWatchdogCancelsIdleStream(t *testing.T) {
	ctx, watch := newFrameWatchdog(context.Background(), 40*time.Millisecond)
	defer watch.Release()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("watchdog did not cancel the stream")
	}
	if !watch.Expired() {
		t.Fatalf("Expired() = false after the deadline fired")
	}
}

// TestFrameWatchdogDisarmKeepsStreamAlive is the regression that matters most:
// disarming the deadline must not cancel the request context. Releasing and
// disarming share a cancel func, and conflating them tore down the stream on its
// very first frame, which surfaced as a spurious client_cancelled error after
// the answer had already been delivered.
func TestFrameWatchdogDisarmKeepsStreamAlive(t *testing.T) {
	ctx, watch := newFrameWatchdog(context.Background(), 40*time.Millisecond)
	defer watch.Release()

	watch.Disarm()

	select {
	case <-ctx.Done():
		t.Fatalf("Disarm cancelled the request context")
	case <-time.After(200 * time.Millisecond):
	}
	if watch.Expired() {
		t.Fatalf("deadline fired after Disarm")
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("context ended after Disarm: %v", err)
	}
}

// TestFrameWatchdogReleaseEndsStream checks the other half: once the request is
// over, the derived context must not leak.
func TestFrameWatchdogReleaseEndsStream(t *testing.T) {
	ctx, watch := newFrameWatchdog(context.Background(), 5*time.Second)
	watch.Release()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatalf("Release did not end the derived context")
	}
	if watch.Expired() {
		t.Fatalf("Release was reported as an expiry")
	}
}

// TestFrameWatchdogIgnoresCallerContext isolates the two cancellations: the
// watchdog must not claim expiry when the caller went away for its own reasons,
// otherwise a client disconnect would be reported as an upstream stall and the
// account would take a failure it did not earn.
func TestFrameWatchdogIgnoresCallerContext(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	_, watch := newFrameWatchdog(parent, 5*time.Second)
	defer watch.Release()

	cancel()
	if watch.Expired() {
		t.Fatalf("caller cancellation was reported as a watchdog expiry")
	}
}
