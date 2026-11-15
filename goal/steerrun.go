package goal

import (
	"context"

	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// WithSteerJudge sets the judge of whether a run addressed its redirects. Without one a
// steered goal stalls, because trusting the run's word or carrying the redirect forever
// are both dishonest. Wiring a judge releases it on the next reconcile.
func WithSteerJudge(j SteerJudge) Option { return func(g *Reconciler) { g.judge = j } }

// dischargeSteers runs at the completion claim, because the claim is where the run states
// what it did. account is judged by a model, because string matching cannot tell naming a
// redirect from honoring it. It reports whether it handled the reconcile.
func (g *Reconciler) dischargeSteers(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash, account string) (reconcile.Result, bool, error) {
	outstanding := status.OutstandingSteers(spec.Steers)
	if len(outstanding) == 0 {
		return reconcile.Result{}, false, nil
	}
	if g.judge == nil {
		return g.stalled(ctx, r, status, specHash, "SteerJudgeMissing", "the run was redirected but no judge is wired to rule on whether it addressed the redirect")
	}
	acks, err := g.judge.Acknowledged(ctx, r, spec, *status, outstanding, account)
	if err != nil {
		// Classified by the judge. Neither class is a discharge, because an unreachable
		// judge has said nothing.
		return reconcile.Result{}, true, err
	}
	open := status.RecordAcknowledgements(outstanding, acks, g.clk.Now())
	if len(open) == 0 {
		return reconcile.Result{}, false, nil
	}
	return g.stalled(ctx, r, status, specHash, "SteerUnaddressed", UnacknowledgedReason(open, account))
}
