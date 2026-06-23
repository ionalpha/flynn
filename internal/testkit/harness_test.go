package testkit_test

import (
	"context"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/testkit"
)

// TestGeneratorsProduceValidValues checks the generators themselves: a single
// property exercises many cases, and rapid shrinks any failure.
func TestGeneratorsProduceValidValues(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		if sk := testkit.SkillGen().Draw(rt, "skill"); sk.Slug == "" {
			rt.Fatalf("SkillGen produced an empty slug")
		}
		if a := testkit.ActionGen().Draw(rt, "action"); a.Name == "" {
			rt.Fatalf("ActionGen produced an empty name")
		}
		if m := testkit.MemoryItemGen().Draw(rt, "memory"); m.Kind == "" {
			rt.Fatalf("MemoryItemGen produced an empty kind")
		}
		if in := testkit.AppendInputGen("s").Draw(rt, "append"); in.Stream != "s" || in.Type == "" {
			rt.Fatalf("AppendInputGen produced invalid input: %+v", in)
		}
		_ = testkit.ScopeGen().Draw(rt, "scope") // any scope is valid; exercise the generator
	})
}

// TestFaultConstructorsSchedule pins the firing schedule of every FaultPlan
// constructor, so the chaos primitives behave exactly as documented.
func TestFaultConstructorsSchedule(t *testing.T) {
	boom := fault.New(fault.Terminal, "x", "x")
	cases := []struct {
		name string
		plan *testkit.FaultPlan
		want []bool // whether calls 1..4 fail
	}{
		{"FailOnCall(2)", testkit.FailOnCall(2, boom), []bool{false, true, false, false}},
		{"FailFirst(2)", testkit.FailFirst(2, boom), []bool{true, true, false, false}},
		{"FailEvery(2)", testkit.FailEvery(2, boom), []bool{false, true, false, true}},
		{"Always", testkit.Always(boom), []bool{true, true, true, true}},
	}
	for _, tc := range cases {
		h := testkit.FaultyHandler(nil, tc.plan)
		for i, want := range tc.want {
			_, err := h.Handle(context.Background(), dispatch.Action{})
			if (err != nil) != want {
				t.Fatalf("%s call %d: failed=%v, want %v", tc.name, i+1, err != nil, want)
			}
		}
	}
}

// TestLifecycleHoldsUnderChaos is the headline composition: for ANY action
// sequence and ANY failure schedule, the dispatch waist emits a coherent
// lifecycle. One property + generators + fault injection replaces dozens of
// hand-written cases — the "fewer lines because of the architecture" payoff.
func TestLifecycleHoldsUnderChaos(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		sink := &dispatch.MemorySink{}
		failEvery := rapid.IntRange(0, 3).Draw(rt, "failEvery")

		d := dispatch.New(
			testkit.FaultyHandler(nil, testkit.FailEvery(failEvery, fault.New(fault.Transient, "chaos", "x"))),
			dispatch.WithClock(clock.NewManual(time.Unix(0, 0))),
			dispatch.WithEventSink(sink),
		)

		n := rapid.IntRange(0, 12).Draw(rt, "n")
		for i := 0; i < n; i++ {
			_, _ = d.Dispatch(ctx, testkit.ActionGen().Draw(rt, "action"))
		}
		testkit.RequireLifecycle(rt, sink.Events())
	})
}

// TestDeterministicReplay proves the determinism the whole harness rests on:
// the same scenario under a Manual clock produces byte-identical event streams.
func TestDeterministicReplay(t *testing.T) {
	run := func() []dispatch.Event {
		sink := &dispatch.MemorySink{}
		d := dispatch.New(
			dispatch.HandlerFunc(func(context.Context, dispatch.Action) (dispatch.Result, error) {
				return dispatch.Result{}, nil
			}),
			dispatch.WithClock(clock.NewManual(time.Unix(7, 0))),
			dispatch.WithEventSink(sink),
		)
		for _, name := range []string{"alpha", "beta"} {
			_, _ = d.Dispatch(context.Background(), dispatch.Action{Name: name})
		}
		return sink.Events()
	}
	testkit.DiffEvents(t, run(), run())
}
