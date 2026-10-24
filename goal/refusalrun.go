package goal

import (
	"context"

	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

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
// which settles the goal, and whether one is a run asking for authority nobody gave it,
// which pauses the goal (allowance.go). Otherwise it hands back.
//
// The verdict is asked first. Where both readings fire the verdict is the truer one:
// the ask would have the author widen the authority of a run that was looking for a way
// through, the opposite of what either shape needs.
//
// It runs beside the audit, before the stop evaluator, and not among the stall guards:
// those explain why a run that did not converge stopped, and a route-around is a verdict
// on a run that did. Both readings are functions of the whole record, so a poll tick,
// a resync or a wake sees what the last pass already ruled on and no already-seen state
// is carried; a count banked on the status could be lost by a status write.
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
	return g.stalled(ctx, r, status, specHash, "RefusedRouteAround", verdict.RefusalReason())
}
