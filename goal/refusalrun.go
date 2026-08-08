package goal

import (
	"context"

	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// Where the refusal verdict sits in a reconcile is the same argument as the invariant
// audit's, and it lands beside it: after a completed step is observed, before the goal
// parks, plans, fans out, settles its ledger or is asked whether it is done.
//
// It has to be there rather than among the other stall guards at the bottom of the pass.
// Those run only once the stop evaluator has said the goal is not finished, which makes
// every one of them a reason a run that did not converge stopped. A route-around is a
// verdict on a run that did converge: the whole shape is a run that got where it was going
// by a route it was refused, and reporting that run as converged and separately noting the
// refusals would be reporting the workaround as a success. So the refusals are read first
// and settle the goal from there, and the stop evaluator is never asked.
//
// The verdict is derived from the whole record every pass rather than banked on the
// status. A count kept on the status would be a number a status write could lose and a
// resumed run would restart from zero; the record cannot be spent, so re-reading it is
// both simpler and the stronger guarantee. The cost is one probe read per pass that
// observed a step, which is the same pacing as the audit and for the same reason: only a
// completed step can have added a refusal.

// WithRefusalProbe turns on refused-gate detection: after each completed step the goal's
// recorded refusals are read through p, and a run that kept pushing on one gate stops
// naming what refused it.
//
// Unlike a goal's invariants, which stall a goal when nothing is wired to check them, a
// nil probe simply leaves this off. The difference is what a goal declared: terms on the
// spec are a requirement the author stated and a run that ignores them is not the run they
// asked for, whereas nothing on a spec asks for this. It is a property of the host's
// governance, so a host that has not wired a probe has not made a promise it is breaking.
func WithRefusalProbe(p RefusalProbe) Option { return func(g *Reconciler) { g.refusals = p } }

// checkRefusals reads the run's recorded refusals and reports whether it handled the
// reconcile. One read answers two questions: whether the refusals amount to a verdict,
// which settles the goal, and whether one of them is a run asking for authority nobody gave
// it, which pauses the goal (allowance.go). Otherwise it hands back and the goal carries on.
//
// The verdict is asked first. A run refused for want of an allowance and a run working its
// way around a gate can leave overlapping records, and where both readings fire the verdict
// is the truer account: the ask would have the author widen the authority of a run that was
// looking for a way through, which is the opposite of what either shape needs.
//
// observed paces the read the way it paces the audit: a poll tick, a resync and a wake see
// the record the last pass already ruled on. There is no already-seen state to carry,
// because both readings are functions of the record rather than of what previous passes
// made of it.
func (g *Reconciler) checkRefusals(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash string, observed bool) (reconcile.Result, bool, error) {
	// A paused goal reads too, on the one pass an edit to its spec brings it back: it has
	// no step in flight, so the record is the only place the answer to the ask can be, and
	// a goal that only read after a completed step could never see itself released.
	if g.refusals == nil || (!observed && !status.pausedForAllowance()) {
		return reconcile.Result{}, false, nil
	}
	refusals, err := g.refusals.Refusals(ctx, r)
	if err != nil {
		// Classified by the probe: a read that failed for a moment retries rather than
		// stalling a healthy goal, and anything else settles it. Neither is a pass. An
		// unreadable record says nothing about whether the run was refused, and a run
		// whose refusals nobody could read is not a run that had none.
		return reconcile.Result{}, true, err
	}
	verdict, stop := ReadRefusals(refusals)
	if !stop {
		return g.pauseForAllowance(ctx, r, spec, status, specHash, refusals)
	}
	msg := verdict.RefusalReason()
	status.Phase = PhaseStalled
	status.Message = msg
	status.SetCondition(Condition{Type: CondStalled, Status: "True", Reason: "RefusedRouteAround", Message: msg}, g.clk.Now())
	status.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "RefusedRouteAround"}, g.clk.Now())
	res, err := g.terminal(ctx, r, *status, specHash)
	return res, true, err
}
