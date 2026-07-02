package screen_test

import (
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/tui/screen"
)

const frameInterval = 10 * time.Millisecond

// paints collects paint invocations on a channel the test can await.
func paintInto(ch chan struct{}) func() {
	return func() { ch <- struct{}{} }
}

func awaitPaint(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no paint arrived")
	}
}

func expectNoPaint(t *testing.T, ch chan struct{}, within time.Duration) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("unexpected paint")
	case <-time.After(within):
	}
}

func TestSchedulerPaintsOnRequest(t *testing.T) {
	ch := make(chan struct{})
	s := screen.NewScheduler(clock.System{}, frameInterval, paintInto(ch))
	defer s.Stop()
	s.Request()
	awaitPaint(t, ch)
}

func TestSchedulerCoalescesABurstIntoOneTrailingPaint(t *testing.T) {
	ch := make(chan struct{})
	s := screen.NewScheduler(clock.System{}, frameInterval, paintInto(ch))
	defer s.Stop()
	s.Request()
	awaitPaint(t, ch)
	// A burst during the cooldown folds into exactly one more paint.
	for range 50 {
		s.Request()
	}
	awaitPaint(t, ch)
	expectNoPaint(t, ch, 5*frameInterval)
}

func TestSchedulerIdlesWithoutRequests(t *testing.T) {
	ch := make(chan struct{})
	s := screen.NewScheduler(clock.System{}, frameInterval, paintInto(ch))
	defer s.Stop()
	expectNoPaint(t, ch, 3*frameInterval)
}

func TestSchedulerStopWaitsForThePaintGoroutine(t *testing.T) {
	painted := make(chan struct{}, 8)
	s := screen.NewScheduler(clock.System{}, frameInterval, func() { painted <- struct{}{} })
	s.Request()
	select {
	case <-painted:
	case <-time.After(2 * time.Second):
		t.Fatal("no paint before stop")
	}
	s.Stop()
	// After Stop returns, no further paint can run.
	if n := len(painted); n != 0 {
		t.Fatalf("%d paints queued after Stop", n)
	}
}

func TestSchedulerRequestNeverBlocks(t *testing.T) {
	// A scheduler whose paint function is stuck must not make Request block:
	// producers (the streaming goroutine) can never be back-pressured by the
	// renderer.
	block := make(chan struct{})
	s := screen.NewScheduler(clock.System{}, frameInterval, func() { <-block })
	s.Request()
	done := make(chan struct{})
	go func() {
		for range 1000 {
			s.Request()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Request blocked behind a stuck paint")
	}
	close(block)
	s.Stop()
}
