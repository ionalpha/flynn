package goal

import (
	"context"

	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// Where the steer gate sits in a reconcile is the opposite of where the invariant audit
// sits, and the two placements come from the same argument. A term of the run is checked
// before the stop evaluator is asked anything, because a run that broke its terms getting
// the task done must not be able to trade the breach against having finished. A redirect
// is checked at the completion claim itself, because the claim is where the run states
// what it did, and that statement is the thing being judged. Asking earlier would be
// asking about work the run has not finished describing.
//
// So this runs after the stop evaluator has said yes and after the ledger has held the
// claim up against the record, and before the goal is written converged. Everything it can
// do from there is refuse: a discharged redirect changes nothing about the verdict, and an
// outstanding one settles the goal un-done with the redirect and the account quoted.
//
// A goal that reported completion and was steered afterwards is judged by this same rule,
// against the account it already gave. That is intended and it is not a way to make a
// finished run do more work: the account was written before the redirect existed, so it
// will not address it, and the goal settles saying so. Getting more work out of a settled
// run is a new turn on the conversation, which is a different operation with a different
// name.

// WithSteerJudge rules on a run's account of how it addressed the operator's redirects,
// through j. Without one, a goal that is steered stops rather than running under an
// obligation it has no way to discharge.
//
// That stall is the same choice invariants make about a missing auditor, for the same
// reason and with the same recovery. The alternatives are both dishonest: discharging a
// redirect on the run's own say-so is the prose completion the ledger exists to replace,
// and carrying the redirect forever would let a steered run burn its whole step budget
// before anyone found out it could never finish. The stall names the missing judge and is
// re-examined on the next reconcile, so wiring one releases the goal rather than requiring
// the run to be recreated. A goal nobody steers is unaffected.
func WithSteerJudge(j SteerJudge) Option { return func(g *Reconciler) { g.judge = j } }

// dischargeSteers judges the run's account of finishing against the redirects it is still
// under, and reports whether it handled the reconcile. It handles it when the goal must
// stop: with no judge wired, or with a redirect the account did not address. Otherwise it
// records the acknowledgements on the status and hands back, and the goal converges.
//
// account is what the stop evaluator gave as its reason, which is the run's own statement
// of what it did. It is judged rather than searched: a run that names the redirect and
// then describes doing the opposite has not addressed it, and no amount of string matching
// tells the two apart.
func (g *Reconciler) dischargeSteers(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash, account string) (reconcile.Result, bool, error) {
	outstanding := status.OutstandingSteers(spec.Steers)
	if len(outstanding) == 0 {
		return reconcile.Result{}, false, nil
	}
	if g.judge == nil {
		status.stall("SteerJudgeMissing", "the run was redirected but no judge is wired to rule on whether it addressed the redirect", g.clk.Now())
		res, err := g.terminal(ctx, r, *status, specHash)
		return res, true, err
	}
	acks, err := g.judge.Acknowledged(ctx, r, spec, *status, outstanding, account)
	if err != nil {
		// Classified by the judge: a call that failed for a moment retries rather than
		// stopping a run that may well have complied, and anything else settles the goal.
		// Neither is a discharge. A judge nobody could reach has said nothing about
		// whether the redirect was honored, and silence is not compliance.
		return reconcile.Result{}, true, err
	}
	open := status.RecordAcknowledgements(outstanding, acks, g.clk.Now())
	if len(open) == 0 {
		return reconcile.Result{}, false, nil
	}
	status.stall("SteerUnaddressed", UnacknowledgedReason(open, account), g.clk.Now())
	res, err := g.terminal(ctx, r, *status, specHash)
	return res, true, err
}
