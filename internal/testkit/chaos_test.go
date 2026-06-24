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
		dispatch.WithClock(clock.NewManual(time.Unix(0, 0))),
		dispatch.WithEventSink(sink),
	)
	work := testkit.FaultyWork(nil, plan)

	if err := d.Govern(ctx, dispatch.Action{Name: "fetch"}, work); fault.Classify(err) != fault.Transient {
		t.Fatalf("first call class = %v, want transient", fault.Classify(err))
	}
	if err := d.Govern(ctx, dispatch.Action{Name: "fetch"}, work); err != nil {
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

	d := dispatch.New(dispatch.WithEventSink(sink))

	ran := false
	work := func(context.Context) (dispatch.Metering, error) {
		ran = true
		return dispatch.Metering{Tokens: 1}, nil
	}
	if err := d.Govern(ctx, dispatch.Action{Name: "noop"}, work); err != nil {
		t.Fatalf("a failing event sink must not fail the dispatch, got %v", err)
	}
	if !ran {
		t.Fatal("work did not run despite a failing sink")
	}
}

func TestFaultPlanSchedules(t *testing.T) {
	boom := fault.New(fault.Terminal, "x", "x")

	every2 := testkit.FailEvery(2, boom)
	w := testkit.FaultyWork(nil, every2)
	var fails int
	for i := 0; i < 4; i++ {
		if _, err := w(context.Background()); err != nil {
			fails++
		}
	}
	if fails != 2 { // calls 2 and 4
		t.Fatalf("FailEvery(2) over 4 calls failed %d times, want 2", fails)
	}
}
