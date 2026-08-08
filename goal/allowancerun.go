package goal

import (
	"context"

	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// The pause reads the same record the route-around verdict reads, on the same probe, in the
// same place in the pass: after a completed step is observed, before the goal parks, plans,
// fans out, settles its ledger or is asked whether it is done. It has to be above the stop
// evaluator for the reason the audit does. A run that was refused an irreversible action and
// then reported the objective achieved has done one of two things, and neither of them is
// what it says: it found another way, or it is calling the objective met without the part it
// was refused. Asking the evaluator first would take its word for which.
//
// It reuses the refusal probe rather than adding one, because the ask is already on the
// record: the waist writes the rule that refused alongside the action, so the refusal that
// says a run wanted authority it was not given is the same kind of entry as the refusal that
// says a run kept pushing. One read per pass answers both.

// AllowanceStallReason is the condition reason a paused goal carries, distinct from the
// other stall reasons because it is the one that is answerable: the goal is stopped waiting
// on a declaration, not stopped because something failed.
const AllowanceStallReason = "AwaitingAllowance"

// AllowanceResumeReason is the condition reason recorded when a paused goal is released by
// its author declaring the allowance. It is written as the stall condition going False, so
// the record shows the pause being answered rather than just ending.
const AllowanceResumeReason = "AllowanceDeclared"

// pauseForAllowance parks a goal whose record shows it was refused an irreversible action
// nobody declared, and releases one whose author has since declared it.
//
// The release half is not bookkeeping. A pause is meant to be answered, so the run that
// resumes has to stop reading as stopped: leaving the stall condition True would leave a
// working goal describing itself as blocked on a question that has been answered.
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
	msg := ask.AskReason()
	status.Phase = PhaseStalled
	status.Message = msg
	status.SetCondition(Condition{Type: CondStalled, Status: "True", Reason: AllowanceStallReason, Message: msg}, g.clk.Now())
	status.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: AllowanceStallReason}, g.clk.Now())
	res, err := g.terminal(ctx, r, *status, specHash)
	return res, true, err
}

// pausedForAllowance reports whether the status this pass decoded is one a previous pass
// parked on an undeclared action. It reads the condition rather than a field of its own,
// because the condition is what the pause wrote and a second copy of the same fact is a
// second thing that can be wrong.
func (s Status) pausedForAllowance() bool {
	for _, c := range s.Conditions {
		if c.Type == CondStalled {
			return c.Status == "True" && c.Reason == AllowanceStallReason
		}
	}
	return false
}
