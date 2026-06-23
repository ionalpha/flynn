package testkit_test

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/testkit"
)

// TestFlakyHandlerDegradesAndRecovers drives a dispatcher with a handler that
// fails its first call (transient) then succeeds, and asserts the waist records
// both outcomes coherently — the kind of degrade-then-recover the agent must
// survive.
func TestFlakyHandlerDegradesAndRecovers(t *testing.T) {
	ctx := context.Background()
	sink := &dispatch.MemorySink{}
	plan := testkit.FailFirst(1, fault.New(fault.Transient, "flaky_dep", "try again"))

	d := dispatch.New(
		testkit.FaultyHandler(nil, plan),
		dispatch.WithClock(clock.NewManual(time.Unix(0, 0))),
		dispatch.WithEventSink(sink),
	)

	if _, err := d.Dispatch(ctx, dispatch.Action{Name: "fetch"}); fault.Classify(err) != fault.Transient {
		t.Fatalf("first call class = %v, want transient", fault.Classify(err))
	}
	if _, err := d.Dispatch(ctx, dispatch.Action{Name: "fetch"}); err != nil {
		t.Fatalf("second call should recover, got %v", err)
	}

	events := sink.Events()
	testkit.RequireLifecycle(t, events)
	counts := testkit.CountByType(events)
	if counts[dispatch.EventEnd] != 2 {
		t.Fatalf("want 2 end events (one failed, one ok), got %d", counts[dispatch.EventEnd])
	}
}

// TestFaultySinkDoesNotBreakDispatch proves an event-sink failure is swallowed:
// the action still runs and returns its result. Observability must never take
// down the work.
func TestFaultySinkDoesNotBreakDispatch(t *testing.T) {
	ctx := context.Background()
	sink := testkit.FaultySink(nil, testkit.Always(fault.New(fault.Terminal, "sink_down", "no")))

	d := dispatch.New(
		dispatch.HandlerFunc(func(context.Context, dispatch.Action) (dispatch.Result, error) {
			return dispatch.Result{Tokens: 1}, nil
		}),
		dispatch.WithEventSink(sink),
	)

	r, err := d.Dispatch(ctx, dispatch.Action{Name: "noop"})
	if err != nil {
		t.Fatalf("a failing event sink must not fail the dispatch, got %v", err)
	}
	if r.Tokens != 1 {
		t.Fatalf("result lost: Tokens = %d, want 1", r.Tokens)
	}
}

func TestFaultPlanSchedules(t *testing.T) {
	boom := fault.New(fault.Terminal, "x", "x")

	every2 := testkit.FailEvery(2, boom)
	h := testkit.FaultyHandler(nil, every2)
	var fails int
	for i := 0; i < 4; i++ {
		if _, err := h.Handle(context.Background(), dispatch.Action{}); err != nil {
			fails++
		}
	}
	if fails != 2 { // calls 2 and 4
		t.Fatalf("FailEvery(2) over 4 calls failed %d times, want 2", fails)
	}
}
