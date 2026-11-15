package goal

import (
	"context"

	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// WithInvariantAudit audits invariants once per completed step, before the stop condition.
// A breach settles the goal terminally. Without an auditor a goal with terms stalls,
// because an unaudited run would finish looking like one whose terms held. A goal with no
// terms is unaffected.
func WithInvariantAudit(a InvariantAuditor) Option {
	return func(g *Reconciler) { g.auditor = a }
}

// auditInvariants reports whether a breach settled the reconcile. It runs before the goal
// parks, plans, fans out, settles its ledger or claims done, so no claim outranks a broken
// term. Only an observed step is audited, because nothing else adds work to judge; a
// recorded breach settles regardless, so a poll tick cannot step over it.
func (g *Reconciler) auditInvariants(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash string, observed bool) (reconcile.Result, bool, error) {
	if len(spec.Invariants) == 0 && len(status.Invariants) == 0 {
		return reconcile.Result{}, false, nil
	}
	// A recorded breach settles without asking again, so a later spec edit or lost
	// auditor cannot reach the stop evaluator past it.
	if st, breached := status.BreachedInvariant(); breached {
		return g.breach(ctx, r, spec, status, specHash, st)
	}
	// Unchecked terms read as governed when they are not, so the goal stalls.
	if g.auditor == nil {
		return g.stalled(ctx, r, status, specHash, "InvariantAuditorMissing",
			"the goal states terms of the run but no auditor is wired to check them")
	}
	if !observed {
		return reconcile.Result{}, false, nil
	}
	// Every term every time, because one that held at step 3 says nothing about step 4.
	terms := spec.Invariants
	breaches, err := g.auditor.Audit(ctx, r, spec, *status, terms)
	if err != nil {
		// Classified by the auditor; an unclassified error is Terminal. Neither class is a
		// pass, because an auditor that could not answer says nothing about the terms.
		return reconcile.Result{}, true, err
	}
	if !status.RecordAudit(terms, breaches, g.clk.Now()) {
		return reconcile.Result{}, false, nil // audited, and the terms hold
	}
	st, _ := status.BreachedInvariant()
	return g.breach(ctx, r, spec, status, specHash, st)
}

// breach is a stall, because the goal stopped short of its stop condition. The reason
// names the term so the outcome is not mistaken for a spent budget.
func (g *Reconciler) breach(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash string, st InvariantState) (reconcile.Result, bool, error) {
	return g.stalled(ctx, r, status, specHash, "InvariantBreached", BreachReason(spec.Invariants, st))
}
