package inbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/inbox"
)

const testBackoff = 2 * time.Second

// waitFor spins until cond holds, so a test can wait for asynchronously-scheduled
// work (a backoff timer registering) before advancing the clock. It polls with a
// brief sleep, mirroring the project's existing timer-wait helpers; the bounded loop
// is a test-failure guard, not part of the behaviour under test.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 2000 {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// TestReceiveLoopBacksOffOnClock proves the reconnect backoff is clock-driven: a
// failing attempt does not retry until the Manual clock advances past the backoff,
// so retry timing is deterministic under test rather than racing the wall clock.
func TestReceiveLoopBacksOffOnClock(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0))
	attempts := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		inbox.ReceiveLoop(ctx, testBackoff, clk, func(context.Context) error {
			attempts <- struct{}{}
			return errors.New("boom") // always fail so every retry waits the backoff
		})
	}()

	// The first attempt runs immediately, before any clock advance.
	<-attempts
	// The loop then arms a backoff timer and blocks on it; no second attempt yet.
	waitFor(t, func() bool { return clk.PendingTimers() == 1 })
	select {
	case <-attempts:
		t.Fatal("retried before the backoff elapsed on the clock")
	default:
	}
	// Advancing past the backoff releases exactly the next attempt.
	clk.Advance(testBackoff)
	<-attempts

	cancel()
	<-done
}

// TestReceiveLoopStopsOnCancelDuringBackoff proves a cancellation during the backoff
// wait exits promptly, without needing the clock to advance, so teardown never
// blocks on a pending retry timer.
func TestReceiveLoopStopsOnCancelDuringBackoff(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0))
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		inbox.ReceiveLoop(ctx, testBackoff, clk, func(context.Context) error {
			return errors.New("boom")
		})
	}()

	// Wait until the loop is parked on the backoff timer, then cancel without
	// advancing the clock.
	waitFor(t, func() bool { return clk.PendingTimers() == 1 })
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReceiveLoop did not exit on cancel during backoff")
	}
}

// TestReceiveLoopSuccessRetriesImmediately proves a clean attempt (nil error) runs
// the next attempt at once, with no backoff timer: the poll-style source keeps
// long-polling without a pause.
func TestReceiveLoopSuccessRetriesImmediately(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0))
	attempts := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		inbox.ReceiveLoop(ctx, testBackoff, clk, func(context.Context) error {
			attempts <- struct{}{}
			return nil // clean end: next attempt should run immediately
		})
	}()

	// Several attempts occur back-to-back with no clock advance and no armed timer.
	for range 3 {
		<-attempts
	}
	if got := clk.PendingTimers(); got != 0 {
		t.Fatalf("a successful attempt armed a backoff timer: pending %d", got)
	}

	cancel()
	<-done
}
