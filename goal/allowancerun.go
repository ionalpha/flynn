package goal

import (
	"context"

	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// AllowanceStallReason marks the one stall an author can answer, by declaring the
// allowance.
const AllowanceStallReason = "AwaitingAllowance"

// AllowanceResumeReason is written with the stall condition going False, so the record
// shows the pause answered.
const AllowanceResumeReason = "AllowanceDeclared"

// pauseForAllowance runs before the stop evaluator, because a run refused an irreversible
// action that then claims success either routed around the refusal or skipped the part it
// was refused. On release it clears the stall condition so a working goal stops reading as
// blocked.
func (g *Reconciler) pauseForAllowance(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash string, refusals []Refusal) (reconcile.Result, bool, error) {
	ask, need := ReadAllowanceAsk(refusals, spec.Allowances)
	if !need {
		if status.pausedForAllowance() {
			status.SetCondition(Condition{
				Type: CondStalled, Status: "False", Reason: AllowanceResumeReason,
				Message: "the allowance the run was paused on has been declared",
			}, g.clk.Now())
		}
		return reconcile.Result{}, false, nil
	}
	return g.stalled(ctx, r, status, specHash, AllowanceStallReason, ask.AskReason())
}

// pausedForAllowance reads the condition the pause wrote, so no second copy of the fact can
// disagree with it.
func (s Status) pausedForAllowance() bool {
	for _, c := range s.Conditions {
		if c.Type == CondStalled {
			return c.Status == "True" && c.Reason == AllowanceStallReason
		}
	}
	return false
}
