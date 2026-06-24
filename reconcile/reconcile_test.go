package reconcile

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
)

// reconcileOne is synchronous, so the retry policy can be asserted directly off
// the queue state with no goroutines or real time.
func TestComposeSuccessForgets(t *testing.T) {
	m := clock.NewManual(epoch())
	q := NewQueue[string](m)
	q.AddRateLimited("a") // give it a non-zero backoff to prove success clears it
	c := NewController("t", q, ok(Result{}))

	c.reconcileOne(context.Background(), "a")

	if n := q.NumRequeues("a"); n != 0 {
		t.Fatalf("success did not forget backoff: NumRequeues %d", n)
	}
}

func TestComposeRequeueAfter(t *testing.T) {
	m := clock.NewManual(epoch())
	q := NewQueue[string](m)
	c := NewController("t", q, ok(Result{RequeueAfter: 30 * time.Second}))

	c.reconcileOne(context.Background(), "a")

	if m.PendingTimers() != 1 {
		t.Fatalf("RequeueAfter did not schedule a timer: pending %d", m.PendingTimers())
	}
	if n := q.NumRequeues("a"); n != 0 {
		t.Fatalf("RequeueAfter should not count as a rate-limited requeue: %d", n)
	}
	if q.Len() != 0 {
		t.Fatal("item should not be ready before its RequeueAfter elapses")
	}
	m.Advance(30 * time.Second)
	if got, _ := q.Get(); got != "a" {
		t.Fatalf("item not requeued after RequeueAfter: %q", got)
	}
}

func TestComposeTransientBacksOff(t *testing.T) {
	m := clock.NewManual(epoch())
	q := NewQueue[string](m)
	c := NewController("t", q, fail(fault.New(fault.Transient, "blip", "try again")))

	c.reconcileOne(context.Background(), "a")

	if n := q.NumRequeues("a"); n != 1 {
		t.Fatalf("transient error did not back off: NumRequeues %d", n)
	}
	if m.PendingTimers() != 1 {
		t.Fatalf("transient error did not schedule a backoff timer: pending %d", m.PendingTimers())
	}
}

func TestComposeTerminalDoesNotRequeue(t *testing.T) {
	m := clock.NewManual(epoch())
	q := NewQueue[string](m)
	c := NewController("t", q, fail(fault.New(fault.Terminal, "bad", "give up")))

	c.reconcileOne(context.Background(), "a")

	if m.PendingTimers() != 0 {
		t.Fatalf("terminal error scheduled a retry: pending %d", m.PendingTimers())
	}
	if n := q.NumRequeues("a"); n != 0 {
		t.Fatalf("terminal error counted a requeue: %d", n)
	}
}

func TestComposePanicBecomesTransient(t *testing.T) {
	m := clock.NewManual(epoch())
	q := NewQueue[string](m)
	c := NewController("t", q, ReconcilerFunc[string](func(context.Context, string) (Result, error) {
		panic("boom")
	}))

	c.reconcileOne(context.Background(), "a") // must not propagate the panic

	if n := q.NumRequeues("a"); n != 1 {
		t.Fatalf("panic was not retried as transient: NumRequeues %d", n)
	}
}

// TestControllerRetriesEndToEnd runs the worker loop and proves a transient
// failure is actually re-reconciled after its backoff fires, then settles.
func TestControllerRetriesEndToEnd(t *testing.T) {
	m := clock.NewManual(epoch())
	q := NewQueue[string](m)
	var attempts int32
	calls := make(chan int32, 8)
	rec := ReconcilerFunc[string](func(_ context.Context, _ string) (Result, error) {
		n := atomic.AddInt32(&attempts, 1)
		calls <- n
		if n < 3 {
			return Result{}, fault.New(fault.Transient, "blip", "retry")
		}
		return Result{}, nil // succeed on the third attempt
	})
	c := NewController("t", q, rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	q.Add("a")
	if got := <-calls; got != 1 {
		t.Fatalf("first attempt = %d", got)
	}
	// Each retry waits for the backoff timer to register, then advances the clock.
	waitPending(t, m, 1)
	m.Advance(defaultBaseDelay)
	if got := <-calls; got != 2 {
		t.Fatalf("second attempt = %d", got)
	}
	waitPending(t, m, 1)
	m.Advance(2 * defaultBaseDelay)
	if got := <-calls; got != 3 {
		t.Fatalf("third attempt = %d", got)
	}
	// Third attempt succeeded: no further reconciles.
	select {
	case got := <-calls:
		t.Fatalf("reconciled again after success: attempt %d", got)
	case <-time.After(20 * time.Millisecond):
	}
}

// --- helpers ---------------------------------------------------------------

func ok(r Result) Reconciler[string] {
	return ReconcilerFunc[string](func(context.Context, string) (Result, error) { return r, nil })
}

func fail(err error) Reconciler[string] {
	return ReconcilerFunc[string](func(context.Context, string) (Result, error) { return Result{}, err })
}

func waitPending(t *testing.T, m *clock.Manual, n int) {
	t.Helper()
	for i := 0; i < 2000; i++ {
		if m.PendingTimers() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d pending timers (have %d)", n, m.PendingTimers())
}
