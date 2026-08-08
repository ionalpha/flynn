package main

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/orchestration"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
)

// alwaysDone is a model whose every turn is a final one, with no fixed script to run out
// of: plan-driven fan-out puts the decomposition on the spec rather than in the
// conversation, so one child per unit plus the parent is not a turn count a test should
// have to predict. What is under test is whether the graph runs.
type alwaysDone struct{}

func (alwaysDone) Generate(context.Context, llm.Request) (llm.Response, error) {
	return llmtest.SayText("the unit's work is done"), nil
}

// TestFanoutAssemblyRunsAUnitGraph is the wiring proof for plan-driven fan-out: a goal
// submitted to the shipped fan-out assembly with a unit graph on its spec dispatches its
// ready unit as a governed child and runs the graph through to proof, rather than stalling
// with UnitSpawnerMissing the way it did while runtime.Config.Units was left unset.
//
// Nothing below the model is a fake: the spawner, the dispatch waist, the sandbox the
// verify clause runs in, the evidence gate and the settlement are the ones the binary
// assembles.
func TestFanoutAssemblyRunsAUnitGraph(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := memStore(t)
	reg := mustRegistry(t)
	rstore := store.Resources(reg)

	run, err := assembleFanoutMission(alwaysDone{}, harness.Plan{},
		t.TempDir(), defaultSystemPrompt, rstore, store.Jobs(), store.Log(), nil, "",
		nil, sandbox.ResourceLimits{})
	if err != nil {
		t.Fatalf("assemble fan-out: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })

	done := make(chan struct{})
	go func() { _ = run.rt.Start(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	units := []goal.Unit{
		{
			ID: "parser", Objective: "build the parser", Verify: "exit 0",
			Actions: []string{mission.ActionModelGenerate},
		},
		{
			ID: "api", Objective: "build the api on the parser", Verify: "exit 0",
			DependsOn: []string{"parser"}, Actions: []string{mission.ActionModelGenerate},
		},
	}
	if _, err := run.rt.SubmitGoal(ctx, "root", goal.Spec{
		Objective:     "deliver the parser and the api",
		StopCondition: "both units are proven",
		Grant:         []string{mission.ActionSpawn, mission.ActionModelGenerate},
		Units:         units,
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	status := waitForUnitDispatch(ctx, t, rstore, "root")

	byID := map[string]goal.UnitState{}
	for _, st := range status.Units {
		byID[st.ID] = st
	}
	// A graph that settles without ever creating a child is the exact regression: it is
	// what a goal carrying a unit graph did in the released binary, where the reason given
	// was that nothing was wired to run it. A unit that ran and then failed is a different
	// outcome and the assertions below say which.
	parser := byID["parser"]
	if parser.ChildID == "" {
		t.Fatalf("the ready unit was never dispatched (phase %s: %s): %+v",
			status.Phase, status.Message, status.Units)
	}
	if want := orchestration.UnitChildName("root", "parser"); parser.ChildID != want {
		t.Fatalf("unit parser ran as %q, want the derived %q", parser.ChildID, want)
	}
	// The dependent unit waits on proof, not on its dependency merely having started, so
	// nothing downstream of an unproven unit is in flight yet.
	if api := byID["api"]; api.ChildID != "" && !parser.Proven {
		t.Fatalf("unit api was dispatched while parser was unproven: %+v", api)
	}

	// The child is a real goal resource owned by the parent, which is what makes it
	// governed rather than a note on the parent's status.
	child, err := rstore.Get(ctx, goal.Kind, resource.Scope{}, parser.ChildID)
	if err != nil {
		t.Fatalf("the dispatched unit has no child resource: %v", err)
	}
	cs, err := goal.DecodeSpec(child)
	if err != nil {
		t.Fatal(err)
	}
	// The right to fan out again did not flow down the plan any more than it flows down a
	// tool call: the child was narrowed to what its unit asked for.
	if len(cs.Grant) != 1 || cs.Grant[0] != mission.ActionModelGenerate {
		t.Fatalf("child grant = %v, want [%s]", cs.Grant, mission.ActionModelGenerate)
	}
	if len(cs.Ledger) != 1 || cs.Ledger[0].Verify != "exit 0" {
		t.Fatalf("the child does not carry its unit's verify clause as a ledger: %+v", cs.Ledger)
	}

	// The whole graph then runs to proof: each child's declared check is executed in the
	// run's own sandbox, its verdict lands on the record, the unit settles from that, and
	// the parent converges only once every unit has.
	//
	// A verify clause is a semi-trusted command, so whether it runs at all is the host's
	// answer, not this change's: a runner that cannot contain semi-trusted work refuses
	// every check and every unit settles unproven, for a reason that has nothing to do
	// with the wiring under test. Everything above holds on every host; this half is
	// asserted where the host can actually execute the check.
	if !hostContainsSemiTrusted(t) {
		t.Log("host cannot contain semi-trusted work: skipping the proof half, dispatch is asserted above")
		return
	}
	final := waitForGoalPhase(ctx, t, rstore, "root", goal.PhaseConverged)
	if !final.UnitsSettled(units) {
		t.Fatalf("the parent converged over an unsettled graph: %+v", final.Units)
	}
	for _, st := range final.Units {
		if !st.Proven || st.Evidence == "" {
			t.Fatalf("unit %s converged without proof: %+v", st.ID, st)
		}
	}
}

// hostContainsSemiTrusted reports whether this machine's default confinement is strong
// enough to run a model-authored command, which is the tier a ledger item's verify clause
// is admitted at. It asks the same sandbox the assembly builds, so the answer is the one
// the run would get rather than a guess from the OS name.
func hostContainsSemiTrusted(t *testing.T) bool {
	t.Helper()
	sb, err := sandbox.NewLocal(t.TempDir(), sandbox.WithDefaultConfinement())
	if err != nil {
		return false
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sandbox.Admit(sb, sandbox.TrustSemi) == nil
}

// waitForGoalPhase polls the goal until it reaches want, failing with whatever it last
// saw (and the stall message, which is where a graph explains itself) on the deadline.
func waitForGoalPhase(ctx context.Context, t *testing.T, s resource.Store, name string, want goal.Phase) goal.Status {
	t.Helper()
	var last goal.Status
	for {
		r, err := s.Get(ctx, goal.Kind, resource.Scope{}, name)
		if err == nil {
			st, derr := goal.DecodeStatus(r)
			if derr != nil {
				t.Fatalf("decode status: %v", derr)
			}
			last = st
			if st.Phase == want {
				return st
			}
			if st.Phase == goal.PhaseStalled {
				t.Fatalf("the goal stalled instead of reaching %s: %s (%+v)", want, st.Message, st.Units)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the goal never reached %s: %+v", want, last)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// waitForUnitDispatch polls the parent until its graph records a dispatched unit or the
// goal stalls, and returns the status either way so the caller can report which happened.
func waitForUnitDispatch(ctx context.Context, t *testing.T, s resource.Store, name string) goal.Status {
	t.Helper()
	var last goal.Status
	for {
		r, err := s.Get(ctx, goal.Kind, resource.Scope{}, name)
		if err == nil {
			st, derr := goal.DecodeStatus(r)
			if derr != nil {
				t.Fatalf("decode status: %v", derr)
			}
			last = st
			if st.Phase == goal.PhaseStalled {
				return st
			}
			for _, u := range st.Units {
				if u.ChildID != "" {
					return st
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no unit was dispatched before the deadline: %+v", last)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
