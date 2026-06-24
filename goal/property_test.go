package goal

import (
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/jobs"
)

// TestGoalReconcileConvergesProperty is the control-loop's behavioural contract
// under chaos: for any stop target and step budget, and with spurious extra
// reconciles injected, the goal always reaches a terminal phase, never runs more
// than one step at a time, and never exceeds its budget. It converges when the
// target is within budget and stalls otherwise.
func TestGoalReconcileConvergesProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		target := rapid.IntRange(1, 6).Draw(rt, "target")
		budget := rapid.IntRange(1, 6).Draw(rt, "budget")

		h := newHarness(t, stopAfter{at: target}, WithStepMaxAttempts(1))
		ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", MaxSteps: budget})

		for i := 0; i < 200; i++ {
			if _, err := h.gr.Reconcile(h.ctx, ref); err != nil {
				rt.Fatalf("reconcile error: %v", err)
			}
			// Chaos: a spurious re-trigger must never launch duplicate work.
			if rapid.Bool().Draw(rt, "spurious") {
				if _, err := h.gr.Reconcile(h.ctx, ref); err != nil {
					rt.Fatalf("spurious reconcile error: %v", err)
				}
			}

			// Invariant: at most one step is ever in flight. Claiming doubles as the
			// fake worker; complete whatever is claimed.
			claimed, _ := h.jobs.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 5, LeaseFor: int64(time.Minute)})
			if len(claimed) > 1 {
				rt.Fatalf("more than one step in flight: %d", len(claimed))
			}
			for _, j := range claimed {
				_ = h.jobs.Complete(h.ctx, j.ID)
			}

			st := h.status(t, ref)
			if st.Steps > budget {
				rt.Fatalf("steps %d exceeded budget %d", st.Steps, budget)
			}
			if st.Phase == PhaseConverged || st.Phase == PhaseStalled {
				if target <= budget && st.Phase != PhaseConverged {
					rt.Fatalf("target %d <= budget %d but phase=%s", target, budget, st.Phase)
				}
				if target > budget && st.Phase != PhaseStalled {
					rt.Fatalf("target %d > budget %d but phase=%s", target, budget, st.Phase)
				}
				return // reached the expected terminal state
			}
		}
		rt.Fatal("goal did not reach a terminal phase")
	})
}
