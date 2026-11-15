package goal

import (
	"context"

	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// WithRefusalProbe stops a run that kept pushing on one gate. A nil probe leaves this off
// instead of stalling, because no spec asks for it: it is host governance.
func WithRefusalProbe(p RefusalProbe) Option { return func(g *Reconciler) { g.refusals = p } }

// checkRefusals reads the refusals once for the route-around verdict and the allowance
// pause. The verdict goes first, because pausing for an allowance would widen authority for
// a run that was looking for a way through. Both readings use the whole record, so no
// count is banked on the status where a write could lose it.
func (g *Reconciler) checkRefusals(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash string, observed bool) (reconcile.Result, bool, error) {
	// A paused goal has no step in flight, so it must read here to see itself released.
	if g.refusals == nil || (!observed && !status.pausedForAllowance()) {
		return reconcile.Result{}, false, nil
	}
	refusals, err := g.refusals.Refusals(ctx, r)
	if err != nil {
		// Classified by the probe. Neither class is a pass, because an unreadable record says
		// nothing.
		return reconcile.Result{}, true, err
	}
	verdict, stop := ReadRefusals(refusals)
	if !stop {
		return g.pauseForAllowance(ctx, r, spec, status, specHash, refusals)
	}
	return g.stalled(ctx, r, status, specHash, "RefusedRouteAround", verdict.RefusalReason())
}
