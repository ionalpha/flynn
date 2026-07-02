package goal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
)

// TestRunWakesOnEnqueue is the dispatch-latency gate: an idle worker must be
// woken by the queue's ready signal, so dispatch-to-claim latency is bounded
// by signal delivery, not the poll interval. The poll here is an hour; if the
// wake path breaks, the step is not claimed until that poll and the test fails
// on its 5-second deadline instead of passing on a 50ms tick.
func TestRunWakesOnEnqueue(t *testing.T) {
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg)
	q := jobs.NewMemory() // system clock: enqueued steps are due immediately
	w := NewWorker(store, q, clock.System{}, &fakeExec{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx, time.Hour)

	// Let the worker drain its first empty claim and block on the select, so the
	// claim below can only happen via the wake signal (or, harmlessly, via a
	// first claim that raced the enqueue; never via the hour-long poll).
	time.Sleep(100 * time.Millisecond)

	// A step whose goal has vanished is claimed and completed without an
	// executor, which keeps this test to the two parts under test: the enqueue
	// signal and the worker's select.
	j, err := q.Enqueue(ctx, jobs.EnqueueParams{Queue: StepQueue, Kind: "goal.step", Payload: []byte("gone")})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for {
		got, err := q.Get(ctx, j.ID)
		if err != nil && !errors.Is(err, jobs.ErrNotFound) {
			t.Fatal(err)
		}
		if got.State == jobs.StateDone {
			return // claimed and completed long before any poll tick
		}
		select {
		case <-deadline:
			t.Fatal("enqueued step not claimed within 5s: the worker slept through the enqueue signal (poll interval is 1h)")
		case <-time.After(time.Millisecond):
		}
	}
}
