package goal

import (
	"context"

	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// WithInvariantAudit checks a goal's invariants through a, once per completed step and
// before the stop condition is evaluated. A breach settles the goal terminally, naming
// the term and what the audit found.
//
// Without it a goal that states terms stalls rather than running unaudited. An auditor
// is a model-backed or command-backed judgement the host supplies, and neither of the
// alternatives to stalling is honest: a default auditor invented here would be a
// formality with the shape of a guard, and carrying on unaudited would let a run that
// was never checked finish looking exactly like a run whose terms held. A goal that
// states no terms is unaffected, so this is not a new requirement on any existing host.
func WithInvariantAudit(a InvariantAuditor) Option {
	return func(g *Reconciler) { g.auditor = a }
}

// auditInvariants runs one pass of the goal's terms and reports whether it handled the
// reconcile. It handles it when a term is broken, which settles the goal; otherwise it
// hands back and the goal carries on.
//
// It runs after a completed step is observed and before the goal parks, plans, fans
// out, settles its ledger or is asked whether it is done, so no path lets the run's
// account of finishing outrank a term it broke getting there.
//
// observed says whether this pass saw a step complete, and it is what paces the audit:
// only a completed step has added anything for an auditor to rule on. The recorded-breach
// check does not wait for it, because a breach found on an earlier pass is a fact about
// the run that a poll tick must not step over.
func (g *Reconciler) auditInvariants(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash string, observed bool) (reconcile.Result, bool, error) {
	if len(spec.Invariants) == 0 && len(status.Invariants) == 0 {
		return reconcile.Result{}, false, nil
	}
	// A breach already on the record settles the goal without asking anyone again. This
	// is what makes a breach unspendable across reconciles: a spec edited after the fact,
	// a lost auditor, a pass that observed no step, none of them reach the stop evaluator
	// over a term the record says was broken.
	if st, breached := status.BreachedInvariant(); breached {
		return g.breach(ctx, r, spec, status, specHash, st)
	}
	// Terms with nobody to check them are worse than no terms at all: the goal reads as
	// governed and is not, and unlike a missing spawner the gap shows up nowhere, since
	// an unaudited run looks exactly like a run whose terms held. So it stalls, the same
	// way a goal carrying a unit graph with no spawner does, and for the same reason.
	if g.auditor == nil {
		return g.stalled(ctx, r, status, specHash, "InvariantAuditorMissing",
			"the goal states terms of the run but no auditor is wired to check them")
	}
	if !observed {
		return reconcile.Result{}, false, nil
	}
	// Every term the goal carries is audited, every time. There is no "already checked"
	// set to skip, because a term is a standing obligation rather than a box to tick:
	// one that held at step 3 says nothing about step 4. Nor is there a breached set to
	// skip, because the check above means a run that reaches here has no broken term.
	terms := spec.Invariants
	breaches, err := g.auditor.Audit(ctx, r, spec, *status, terms)
	if err != nil {
		// Classified by the auditor: a transient failure retries, and everything else
		// (including an unclassified error, which classifies Terminal) settles the goal.
		// Either way the run does not continue, and neither way is a pass: an auditor
		// that could not answer says nothing about whether the terms hold, and a run
		// whose guard is down is not a run that cleared its guard.
		return reconcile.Result{}, true, err
	}
	if !status.RecordAudit(terms, breaches, g.clk.Now()) {
		return reconcile.Result{}, false, nil // audited, and the terms hold
	}
	st, _ := status.BreachedInvariant()
	return g.breach(ctx, r, spec, status, specHash, st)
}

// breach settles a goal whose terms were broken. It is a stall rather than a distinct
// phase: the goal stopped short of its stop condition, which is what Stalled means, and
// the reason names what actually happened so the outcome is not mistaken for a budget
// running out.
func (g *Reconciler) breach(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash string, st InvariantState) (reconcile.Result, bool, error) {
	return g.stalled(ctx, r, status, specHash, "InvariantBreached", BreachReason(spec.Invariants, st))
}
