package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// TestResumeDrivesAnUnenqueuedGoal proves the controlled-resume contract: under
// DriveSubmittedOnly a goal that exists in the store but was never submitted to the
// runtime is not driven on its own, and Resume is what drives it to convergence.
// Resuming an unknown id is ErrNotFound.
func TestResumeDrivesAnUnenqueuedGoal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := goal.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	rstore := st.Resources(reg)

	// A goal placed directly in the store (no dispatched step, so nothing in the job
	// queue can drive it): it exists but was never submitted to a runtime.
	spec, err := json.Marshal(goal.Spec{Objective: "o", StopCondition: "c", MaxSteps: 1})
	if err != nil {
		t.Fatal(err)
	}
	parked, err := rstore.Put(ctx, resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "parked", Spec: spec})
	if err != nil {
		t.Fatal(err)
	}

	rt, err := New(Config{
		Store: rstore, Jobs: st.Jobs(),
		Executor: noopExec{}, Stop: stopAfter{at: 1},
		PollInterval: 15 * time.Millisecond, WorkerPoll: 5 * time.Millisecond,
		DriveSubmittedOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = rt.Start(ctx) }()

	// Without an explicit resume the goal must not be driven.
	time.Sleep(80 * time.Millisecond)
	if s := goalStatus(t, rstore, parked.Key()); s.Phase == goal.PhaseConverged {
		t.Fatalf("a goal that was never submitted converged without resume: %+v", s)
	}

	// Resume drives it to convergence.
	if _, err := rt.Resume(ctx, "parked"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitFor(t, rstore, parked.Key(),
		func(s goal.Status) bool { return s.Phase == goal.PhaseConverged },
		3*time.Second, "converge after resume")

	// Resuming an unknown run is ErrNotFound.
	if _, err := rt.Resume(ctx, "ghost"); !errors.Is(err, resource.ErrNotFound) {
		t.Fatalf("Resume of a missing goal: got %v, want ErrNotFound", err)
	}
}

// TestDroppedKeyIsRecoveredByScopedResync is the liveness guarantee of a one-shot
// run. The control plane deliberately abandons a key it cannot make progress on
// (the controller does not retry a non-transient fault, and a reconcile that parks
// a goal arms nothing), because resync is what brings it back: the bus is
// at-most-once, so a lost wake is an expected event, not a fault. A run that drove
// only what it was handed used to switch that safety net off entirely, so a single
// dropped key parked the run forever with no step in flight, nothing queued, and a
// caller waiting on a phase that would never arrive.
//
// Scoping the sweep to the goals this process submitted keeps the no-adoption
// property (TestResumeDrivesAnUnenqueuedGoal) and gets the guarantee back: nothing
// enqueues this goal again, so only resync can converge it.
func TestDroppedKeyIsRecoveredByScopedResync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gate := &gatedStop{}
	rt, err := New(Config{
		Executor: noopExec{}, Stop: gate,
		PollInterval: 15 * time.Millisecond, WorkerPoll: 5 * time.Millisecond,
		Resync:             40 * time.Millisecond,
		DriveSubmittedOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = rt.Start(ctx) }()

	g, err := rt.SubmitGoal(ctx, "gated", goal.Spec{Objective: "o", StopCondition: "c", MaxSteps: 4})
	if err != nil {
		t.Fatal(err)
	}
	// The gate holds: every reconcile that reaches the stop evaluator fails with a
	// class the controller does not retry, so the key is dropped and the goal is left
	// non-terminal with nothing armed to revisit it.
	waitFor(t, rt.Store(), g.Key(), func(_ goal.Status) bool { return gate.calls.Load() >= 2 },
		3*time.Second, "the gate to be hit and the key dropped")
	if st := goalStatus(t, rt.Store(), g.Key()); st.Phase == goal.PhaseConverged {
		t.Fatalf("goal converged while the gate was closed: %+v", st)
	}

	// Open the gate. Nothing enqueues the goal: no step completes, no bus signal
	// fires, no caller calls Resume. Only the scoped resync can drive it now.
	gate.open.Store(true)
	waitFor(t, rt.Store(), g.Key(),
		func(st goal.Status) bool { return st.Phase == goal.PhaseConverged },
		3*time.Second, "converge from resync alone after a dropped key")
}

// gatedStop is a governance gate: while it is closed it fails the reconcile with a
// class the controller must not retry (a real one pauses for a human), so the key
// is dropped rather than requeued. Once open it converges the goal.
type gatedStop struct {
	open  atomic.Bool
	calls atomic.Int64
}

func (g *gatedStop) Met(_ context.Context, _ goal.Spec, _ goal.Status) (bool, string, error) {
	g.calls.Add(1)
	if !g.open.Load() {
		return false, "", fault.New(fault.NeedsApproval, "stop_gate", "waiting on approval")
	}
	return true, "gate opened", nil
}

// noopExec is a step that does no real work: enough to drive the loop while the
// model-backed executor is wired in elsewhere.
type noopExec struct{}

func (noopExec) Execute(context.Context, resource.Resource) (json.RawMessage, error) {
	return nil, nil
}

// gatedExec completes one real step, then blocks every later step until its
// context is cancelled, so a goal records progress but can never converge while
// the gate holds. The restart test uses it to hold a goal mid-flight
// deterministically (polling for "some but not all steps" is a race the loop
// wins once reads stop queueing behind writes); the blocked step dies with the
// run, exactly the crashed-mid-step shape resume exists for.
type gatedExec struct{ stepped atomic.Bool }

func (g *gatedExec) Execute(ctx context.Context, _ resource.Resource) (json.RawMessage, error) {
	if g.stepped.Swap(true) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, nil
}

// stopAfter converges once the goal has taken `at` steps, a deterministic stand-in
// for the model's semantic stop test.
type stopAfter struct{ at int }

func (s stopAfter) Met(_ context.Context, _ goal.Spec, st goal.Status) (bool, string, error) {
	if st.Steps >= s.at {
		return true, "reached step target", nil
	}
	return false, "", nil
}

func goalStatus(t *testing.T, store resource.Store, key resource.Key) goal.Status {
	t.Helper()
	r, err := store.Get(context.Background(), key.Kind, key.Scope, key.Name)
	if err != nil {
		t.Fatalf("get goal %s: %v", key.Name, err)
	}
	st, err := goal.DecodeStatus(r)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st
}

// waitFor polls the goal's status until ok returns true or the timeout elapses,
// then returns the last status. It uses time.After/ticker rather than time.Now so
// it does not trip the no-wall-clock lint and stays stable under scheduling.
func waitFor(t *testing.T, store resource.Store, key resource.Key, ok func(goal.Status) bool, timeout time.Duration, desc string) goal.Status {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			st := goalStatus(t, store, key)
			t.Fatalf("goal did not %s in time; phase=%s steps=%d", desc, st.Phase, st.Steps)
			return st
		case <-tick.C:
			if st := goalStatus(t, store, key); ok(st) {
				return st
			}
		}
	}
}

// TestNewInvariantsProperty pins New's assembly contract across the config space:
// Executor and Stop are mandatory, a caller-supplied Store requires a Jobs queue,
// and whenever New succeeds it yields a runnable runtime whose default registry
// already admits the Goal kind (so a submit is accepted). This is the invariant of
// the composition root; the convergence loop itself is covered by the e2e tests.
func TestNewInvariantsProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		hasExec := rapid.Bool().Draw(rt, "hasExec")
		hasStop := rapid.Bool().Draw(rt, "hasStop")
		provideStore := rapid.Bool().Draw(rt, "provideStore")
		provideJobs := rapid.Bool().Draw(rt, "provideJobs")

		var cfg Config
		if hasExec {
			cfg.Executor = noopExec{}
		}
		if hasStop {
			cfg.Stop = stopAfter{at: 1}
		}
		if provideStore {
			reg := resource.NewRegistry()
			if err := resource.RegisterCoreKinds(reg); err != nil {
				rt.Fatal(err)
			}
			if err := goal.RegisterKind(reg); err != nil {
				rt.Fatal(err)
			}
			cfg.Store = resource.NewMemory(reg)
			if provideJobs {
				cfg.Jobs = jobs.NewMemory()
			}
		}

		r, err := New(cfg)

		wantErr := !hasExec || !hasStop || (provideStore && !provideJobs)
		if wantErr {
			if err == nil {
				rt.Fatalf("expected error (hasExec=%v hasStop=%v store=%v jobs=%v)", hasExec, hasStop, provideStore, provideJobs)
			}
			return
		}
		if err != nil {
			rt.Fatalf("unexpected error: %v", err)
		}
		if r.Store() == nil {
			rt.Fatal("assembled runtime has a nil store")
		}
		if _, err := r.SubmitGoal(context.Background(), "g", goal.Spec{Objective: "o", StopCondition: "c"}); err != nil {
			rt.Fatalf("submit on assembled runtime: %v", err)
		}
	})
}

// TestRuntimeDrivesGoalToConvergence is the integration proof: the assembled
// runtime, handed only a stub executor and a converge-after-N stop test, takes a
// submitted goal all the way to Converged through the real dispatch-observe loop.
func TestRuntimeDrivesGoalToConvergence(t *testing.T) {
	rt, err := New(Config{
		Executor:     noopExec{},
		Stop:         stopAfter{at: 3},
		PollInterval: 20 * time.Millisecond,
		WorkerPoll:   10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = rt.Start(ctx); close(done) }()

	g, err := rt.SubmitGoal(ctx, "ship", goal.Spec{Objective: "ship it", StopCondition: "shipped"})
	if err != nil {
		t.Fatal(err)
	}

	st := waitFor(t, rt.Store(), g.Key(),
		func(s goal.Status) bool { return s.Phase == goal.PhaseConverged },
		3*time.Second, "converge")
	if st.Steps != 3 {
		t.Fatalf("converged after %d steps, want 3", st.Steps)
	}
	if st.InFlight != nil {
		t.Fatal("converged goal still has a step in flight")
	}

	cancel()
	<-done
}

// TestRuntimeResumesAcrossRestart drives a goal partway in one runtime over a
// durable SQLite file, tears that runtime down, then opens a fresh runtime over the
// same file and shows the goal resumes from its persisted progress and converges.
// Nothing carries over in memory: only the store and queue on disk.
func TestRuntimeResumesAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "agent.db")

	newReg := func() *resource.Registry {
		reg := resource.NewRegistry()
		if err := resource.RegisterCoreKinds(reg); err != nil {
			t.Fatal(err)
		}
		if err := goal.RegisterKind(reg); err != nil {
			t.Fatal(err)
		}
		return reg
	}

	const target = 4

	// Run #1: submit and make some progress, then shut down without converging.
	s1, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	reg1 := newReg()
	rt1, err := New(Config{
		Store: s1.Resources(reg1), Jobs: s1.Jobs(),
		Executor: &gatedExec{}, Stop: stopAfter{at: target},
		PollInterval: 20 * time.Millisecond, WorkerPoll: 10 * time.Millisecond, WorkerLease: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithCancel(ctx)
	done1 := make(chan struct{})
	go func() { _ = rt1.Start(ctx1); close(done1) }()

	g, err := rt1.SubmitGoal(ctx1, "long", goal.Spec{Objective: "o", StopCondition: "c", MaxSteps: target})
	if err != nil {
		t.Fatal(err)
	}
	key := g.Key()
	// Wait until at least one step has completed, so there is real progress to
	// resume from, but stop well before the target so the goal is unfinished.
	mid := waitFor(t, rt1.Store(), key,
		func(s goal.Status) bool { return s.Steps >= 1 },
		3*time.Second, "make initial progress")
	if mid.Phase == goal.PhaseConverged {
		t.Fatalf("goal converged before restart (target too small to test resume): %+v", mid)
	}
	cancel1()
	<-done1
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	stepsAtRestart := mid.Steps

	// Run #2: a brand-new runtime over the same file, no shared memory.
	s2, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	reg2 := newReg()
	rt2, err := New(Config{
		Store: s2.Resources(reg2), Jobs: s2.Jobs(),
		Executor: noopExec{}, Stop: stopAfter{at: target},
		PollInterval: 20 * time.Millisecond, WorkerPoll: 10 * time.Millisecond, WorkerLease: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The persisted progress is visible to the fresh runtime before it does anything.
	resumed := goalStatus(t, rt2.Store(), key)
	if resumed.Steps < stepsAtRestart {
		t.Fatalf("restarted runtime lost progress: steps %d < %d", resumed.Steps, stepsAtRestart)
	}

	ctx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	done2 := make(chan struct{})
	go func() { _ = rt2.Start(ctx2); close(done2) }()
	// Nudge the manager in case the only change hint predated this process.
	rt2.manager.Enqueue(key)

	final := waitFor(t, rt2.Store(), key,
		func(s goal.Status) bool { return s.Phase == goal.PhaseConverged },
		3*time.Second, "converge after restart")
	if final.Steps != target {
		t.Fatalf("resumed goal converged at %d steps, want %d", final.Steps, target)
	}

	cancel2()
	<-done2
}

// breachingAuditor reports a breach against every term it is handed, which is enough to
// show the wire is live: with Config.Auditor set the reconciler asks somebody, and with
// it unset a goal that states terms cannot run at all.
type breachingAuditor struct{}

func (breachingAuditor) Audit(_ context.Context, _ resource.Resource, _ goal.Spec, _ goal.Status, terms []goal.Invariant) ([]goal.Breach, error) {
	out := make([]goal.Breach, 0, len(terms))
	for _, t := range terms {
		out = append(out, goal.Breach{ID: t.ID, Detail: "the check reported it broken"})
	}
	return out, nil
}

// TestRuntimeAuditsTheTermsOfTheRun: Config.Auditor is the wire, and it reaches the
// reconciler's audit. The stop evaluator here would converge the goal at the first step,
// and it does not, because the terms are ruled on first.
func TestRuntimeAuditsTheTermsOfTheRun(t *testing.T) {
	rt, err := New(Config{
		Executor:     noopExec{},
		Stop:         stopAfter{at: 1},
		Auditor:      breachingAuditor{},
		PollInterval: 20 * time.Millisecond,
		WorkerPoll:   10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = rt.Start(ctx); close(done) }()

	g, err := rt.SubmitGoal(ctx, "ship", goal.Spec{
		Objective:     "ship it",
		StopCondition: "shipped",
		Invariants: []goal.Invariant{{
			ID:        "no-force-push",
			Statement: "never force-push a shared branch",
			Check:     "git reflog show origin/main | grep -q forced-update && exit 1 || exit 0",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	st := waitFor(t, rt.Store(), g.Key(),
		func(s goal.Status) bool { return s.Phase == goal.PhaseStalled },
		3*time.Second, "stop on the broken term")
	if !strings.Contains(st.Message, "never force-push a shared branch") {
		t.Fatalf("the goal did not stop on its term: %q", st.Message)
	}
	cancel()
	<-done
}

// TestRuntimeWithNoAuditorRefusesToRunATermedGoal: leaving Auditor nil is not a way to
// run a goal's terms unchecked. A goal that states no terms is unaffected, which the
// convergence test above already shows.
func TestRuntimeWithNoAuditorRefusesToRunATermedGoal(t *testing.T) {
	rt, err := New(Config{
		Executor:     noopExec{},
		Stop:         stopAfter{at: 1},
		PollInterval: 20 * time.Millisecond,
		WorkerPoll:   10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = rt.Start(ctx); close(done) }()

	g, err := rt.SubmitGoal(ctx, "ship", goal.Spec{
		Objective:     "ship it",
		StopCondition: "shipped",
		Invariants: []goal.Invariant{{
			ID:        "no-force-push",
			Statement: "never force-push a shared branch",
			Check:     "git reflog show origin/main | grep -q forced-update && exit 1 || exit 0",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	st := waitFor(t, rt.Store(), g.Key(),
		func(s goal.Status) bool { return s.Phase == goal.PhaseStalled },
		3*time.Second, "stall with no auditor")
	if !strings.Contains(st.Message, "no auditor") {
		t.Fatalf("the goal stalled for another reason: %q", st.Message)
	}
	if st.Steps != 0 {
		t.Fatalf("the goal spent %d steps before stalling, want none", st.Steps)
	}
	cancel()
	<-done
}
