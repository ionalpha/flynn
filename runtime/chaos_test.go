package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/resource"
)

// fastConfig is the shared timing for the integration tests: tight polls so the
// real-time async loop converges in tens of milliseconds, leaving a wide margin
// under the multi-second deadlines even on a loaded CI runner.
func fastConfig(exec goal.StepExecutor, stop goal.StopEvaluator) Config {
	return Config{
		Executor:     exec,
		Stop:         stop,
		PollInterval: 15 * time.Millisecond,
		WorkerPoll:   5 * time.Millisecond,
	}
}

// runUntil assembles and starts a runtime, returning it with a stop func that
// cancels it and waits for its goroutines to drain (no leaks across cases).
func runUntil(t *testing.T, cfg Config) (*Runtime, func()) {
	t.Helper()
	rt, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = rt.Start(ctx); close(done) }()
	return rt, func() { cancel(); <-done }
}

func submit(t *testing.T, rt *Runtime, name string, spec goal.Spec) resource.Key {
	t.Helper()
	g, err := rt.SubmitGoal(context.Background(), name, spec)
	if err != nil {
		t.Fatalf("submit %s: %v", name, err)
	}
	return g.Key()
}

func terminal(s goal.Status) bool {
	return s.Phase == goal.PhaseConverged || s.Phase == goal.PhaseStalled
}

// TestRuntimeConvergenceMatrix exhaustively exercises the assembled loop across
// every (target, budget) pair in a small grid: a goal converges exactly when its
// stop target is within budget, stalls otherwise, never runs past its budget, and
// never leaves a step in flight once terminal.
func TestRuntimeConvergenceMatrix(t *testing.T) {
	for target := 1; target <= 4; target++ {
		for budget := 1; budget <= 4; budget++ {
			t.Run(fmt.Sprintf("target%d_budget%d", target, budget), func(t *testing.T) {
				t.Parallel()
				rt, stop := runUntil(t, fastConfig(noopExec{}, stopAfter{at: target}))
				defer stop()

				key := submit(t, rt, "g", goal.Spec{Objective: "o", StopCondition: "c", MaxSteps: budget})
				st := waitFor(t, rt.Store(), key, terminal, 5*time.Second, "reach a terminal phase")

				if st.Steps > budget {
					t.Fatalf("steps %d exceeded budget %d", st.Steps, budget)
				}
				if st.InFlight != nil {
					t.Fatalf("terminal goal still has a step in flight: %+v", st)
				}
				if target <= budget {
					if st.Phase != goal.PhaseConverged || st.Steps != target {
						t.Fatalf("target<=budget: got %s/%d, want Converged/%d", st.Phase, st.Steps, target)
					}
				} else {
					if st.Phase != goal.PhaseStalled || st.Steps != budget {
						t.Fatalf("target>budget: got %s/%d, want Stalled/%d", st.Phase, st.Steps, budget)
					}
				}
			})
		}
	}
}

// TestRuntimeRecoversFromFlakyExecutor injects a run of step failures (a flaky
// model or tool) with the deterministic testkit fault plan and shows the goal
// still converges to the right step count: failed attempts retry with backoff and
// make no progress, successful ones advance the goal.
func TestRuntimeRecoversFromFlakyExecutor(t *testing.T) {
	for _, failures := range []int{0, 1, 3, 7} {
		t.Run(fmt.Sprintf("failures_%d", failures), func(t *testing.T) {
			t.Parallel()
			const target = 3
			exec := testkit.FaultyExecutor(nil, testkit.FailFirst(failures, errors.New("flaky step")))

			cfg := fastConfig(exec, stopAfter{at: target})
			cfg.WorkerRetryBase = 2 * time.Millisecond
			cfg.WorkerRetryCeiling = 10 * time.Millisecond
			cfg.StepMaxAttempts = 50 // comfortably above the injected failure run
			rt, stop := runUntil(t, cfg)
			defer stop()

			key := submit(t, rt, "g", goal.Spec{Objective: "o", StopCondition: "c"})
			st := waitFor(t, rt.Store(), key,
				func(s goal.Status) bool { return s.Phase == goal.PhaseConverged },
				5*time.Second, "converge despite flaky steps")
			if st.Steps != target {
				t.Fatalf("converged at %d steps, want %d", st.Steps, target)
			}
		})
	}
}

// TestRuntimeStallsWhenStepDiesPermanently is the other side of fault handling: a
// step that fails every attempt exhausts its retry budget, goes dead, and stalls
// the goal rather than spinning forever.
func TestRuntimeStallsWhenStepDiesPermanently(t *testing.T) {
	exec := testkit.FaultyExecutor(nil, testkit.Always(errors.New("always fails")))
	cfg := fastConfig(exec, stopAfter{at: 3})
	cfg.WorkerRetryBase = time.Millisecond
	cfg.WorkerRetryCeiling = 5 * time.Millisecond
	cfg.StepMaxAttempts = 3 // small, so the step goes dead quickly
	rt, stop := runUntil(t, cfg)
	defer stop()

	key := submit(t, rt, "g", goal.Spec{Objective: "o", StopCondition: "c"})
	st := waitFor(t, rt.Store(), key,
		func(s goal.Status) bool { return s.Phase == goal.PhaseStalled },
		5*time.Second, "stall on a permanently failing step")
	if !hasCondition(st, goal.CondStalled, "True") {
		t.Fatalf("stalled goal missing the Stalled condition: %+v", st.Conditions)
	}
}

// TestRuntimeManyConcurrentGoals stresses isolation: many goals submitted at once
// to one runtime each converge to the same step count, with none stealing another's
// steps or stalling. A miscount would mean cross-goal interference.
func TestRuntimeManyConcurrentGoals(t *testing.T) {
	const (
		goals  = 12
		target = 3
	)
	rt, stop := runUntil(t, fastConfig(noopExec{}, stopAfter{at: target}))
	defer stop()

	keys := make([]resource.Key, goals)
	for i := range keys {
		keys[i] = submit(t, rt, fmt.Sprintf("g-%d", i), goal.Spec{Objective: "o", StopCondition: "c"})
	}
	for i, key := range keys {
		st := waitFor(t, rt.Store(), key,
			func(s goal.Status) bool { return s.Phase == goal.PhaseConverged },
			15*time.Second, fmt.Sprintf("goal %d converge", i))
		if st.Steps != target {
			t.Fatalf("goal %d converged at %d steps, want %d (cross-goal interference?)", i, st.Steps, target)
		}
	}
}

// dropBus is a bus that accepts subscriptions but silently drops every published
// message: it models a lost-signal world (an at-most-once bus that delivers
// nothing) so a test can prove the loop converges without any completion hint.
type dropBus struct{}

func (dropBus) Publish(context.Context, bus.Message) error { return nil }

func (dropBus) Subscribe(_ context.Context, pattern string, _ bus.Handler) (bus.Subscription, error) {
	return droppedSub(pattern), nil
}

func (dropBus) Close() error { return nil }

type droppedSub string

func (s droppedSub) Subject() string  { return string(s) }
func (droppedSub) Unsubscribe() error { return nil }

// TestRuntimeConvergesWithoutBusSignals removes the prompt path entirely: with a
// bus that drops every step-completion signal, the goal still converges, driven
// purely by the reconciler's RequeueAfter and the manager's resync. This is the
// safety net that makes the system level-triggered rather than edge-triggered.
func TestRuntimeConvergesWithoutBusSignals(t *testing.T) {
	cfg := fastConfig(noopExec{}, stopAfter{at: 3})
	cfg.Bus = dropBus{}
	cfg.Resync = 50 * time.Millisecond
	rt, stop := runUntil(t, cfg)
	defer stop()

	key := submit(t, rt, "g", goal.Spec{Objective: "o", StopCondition: "c"})
	st := waitFor(t, rt.Store(), key,
		func(s goal.Status) bool { return s.Phase == goal.PhaseConverged },
		5*time.Second, "converge with no completion signals")
	if st.Steps != 3 {
		t.Fatalf("converged at %d steps, want 3", st.Steps)
	}
}

func hasCondition(st goal.Status, typ, status string) bool {
	for _, c := range st.Conditions {
		if c.Type == typ {
			return c.Status == status
		}
	}
	return false
}
