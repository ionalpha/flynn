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
		w := testkit.FaultyWork(nil, tc.plan)
		for i, want := range tc.want {
			_, err := w(context.Background())
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
			dispatch.WithClock(clock.NewManual(time.Unix(0, 0))),
			dispatch.WithEventSink(sink),
		)
		work := testkit.FaultyWork(nil, testkit.FailEvery(failEvery, fault.New(fault.Transient, "chaos", "x")))

		n := rapid.IntRange(0, 12).Draw(rt, "n")
		for range n {
			_ = d.Govern(ctx, testkit.ActionGen().Draw(rt, "action"), work)
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
			dispatch.WithClock(clock.NewManual(time.Unix(7, 0))),
			dispatch.WithEventSink(sink),
		)
		work := func(context.Context) (dispatch.Metering, error) { return dispatch.Metering{}, nil }
		for _, name := range []string{"alpha", "beta"} {
			_ = d.Govern(context.Background(), dispatch.Action{Name: name}, work)
		}
		return sink.Events()
	}
	testkit.DiffEvents(t, run(), run())
}
