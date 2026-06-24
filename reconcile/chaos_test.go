package reconcile

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"pgregory.net/rapid"
)

// TestControllerChaosConverges is the resilience property: no matter how many
// times a reconcile fails transiently, the controller keeps backing off and
// retrying until it converges, and stops exactly once. It injects a random run of
// failures (chaos) and drives the deterministic clock through each backoff.
func TestControllerChaosConverges(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		failures := rapid.IntRange(0, 8).Draw(rt, "failures")

		m := clock.NewManual(epoch())
		q := NewQueue[string](m)
		var attempts int32
		attemptCh := make(chan bool, 64) // true once an attempt succeeds

		rec := ReconcilerFunc[string](func(context.Context, string) (Result, error) {
			n := int(atomic.AddInt32(&attempts, 1))
			succeeded := n > failures
			attemptCh <- succeeded
			if succeeded {
				return Result{}, nil
			}
			return Result{}, fault.New(fault.Transient, "chaos", "injected failure")
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c := NewController("chaos", q, rec)
		go c.Run(ctx)

		q.Add("a")
		deadline := time.After(3 * time.Second)
		for {
			select {
			case ok := <-attemptCh:
				if ok {
					goto converged
				}
				// A transient failure scheduled a backoff timer; fire it.
				waitPending(t, m, 1)
				m.Advance(defaultMaxDelay)
			case <-deadline:
				rt.Fatalf("did not converge after %d injected failures", failures)
			}
		}
	converged:
		// Exactly failures+1 attempts, and no spurious reconcile after success.
		if got := atomic.LoadInt32(&attempts); int(got) != failures+1 {
			rt.Fatalf("attempts = %d, want %d (failures+1)", got, failures+1)
		}
		select {
		case <-attemptCh:
			rt.Fatal("reconciled again after converging")
		case <-time.After(10 * time.Millisecond):
		}
	})
}
