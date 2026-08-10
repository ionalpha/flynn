package goal

// The two checks the reconcile loop makes about the records themselves rather than
// about the work: whether the desired state it is being judged against is still the
// one it adopted, and whether a goal that has not converged must stop anyway.

import (
	"context"
	"errors"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// admit checks the desired-state records a reconcile reads (the ledger, the unit graph
// and the run's terms) and brings the status's observation of each into line with it.
// Everything it
// refuses is a terminal spec fault, because all of it means the same thing: the
// definition the run is being judged against changed underneath the run.
//
// A ledger that lost or rewrote an item is the definition of done being edited
// mid-run. A unit graph with a cycle, an edge to a unit that does not exist, or a unit
// with no way to prove it is a spec that could never run, and it is refused whole
// before anything is dispatched, because discovering it halfway through a fan-out
// means children are already running against it. A unit altered or dropped after its
// child was created is the meaning of work already in flight being rewritten; units
// nothing has been spent on are not a commitment and may still be added, removed and
// reordered.
//
// An invariant dropped or reworded after the run adopted it is the terms of the run
// being renegotiated by the run, which is the move invariants exist to foreclose, so it
// is refused here whether or not an auditor is wired to check those terms. Adding a term
// is always allowed: the rule is one-directional because tightening the terms mid-run is
// its author's to do and loosening them is nobody's.
//
// A goal that carries none of these records passes straight through, so this changes
// nothing for a goal that neither plans, fans out, nor states any terms.
func admit(spec Spec, status *Status) error {
	if err := status.ValidateLedger(spec.Ledger); err != nil {
		return fault.Wrap(fault.Terminal, "goal_ledger_regressed", err)
	}
	status.SyncLedger(spec.Ledger)
	if err := ValidateUnits(spec.Units); err != nil {
		return fault.Wrap(fault.Terminal, "goal_unit_graph_invalid", err)
	}
	if err := status.ValidateDispatched(spec.Units); err != nil {
		return fault.Wrap(fault.Terminal, "goal_unit_rewritten", err)
	}
	status.SyncUnits(spec.Units)
	// The relaxation check runs before the validity check, which is the reverse of the
	// ledger and the unit graph above. A term that was reworded is usually also worse in
	// some other way (a check dropped, a statement softened past what any auditor could
	// settle), and both diagnoses are true at once. The relaxation is the one worth
	// saying: it names what the run just did, where the validity fault would send the
	// author off to fix the wording of a term they were not allowed to touch.
	if err := status.ValidateInvariantsAdopted(spec.Invariants); err != nil {
		return fault.Wrap(fault.Terminal, "goal_invariant_relaxed", err)
	}
	if err := ValidateInvariants(spec.Invariants); err != nil {
		// An unsearchable term gets its own code because it is the one an author can
		// fix in a line, and a generic "invalid" would bury the instruction to write
		// the search under a class of faults that mostly mean something else.
		code := "goal_invariants_invalid"
		if errors.Is(err, ErrInvariantUnsearchable) {
			code = "goal_invariant_unsearchable"
		}
		return fault.Wrap(fault.Terminal, code, err)
	}
	status.SyncInvariants(spec.Invariants)
	return nil
}

// stopGuard reports the reason a goal that has not converged must stop anyway, with the
// message that reason carries, or "" to keep going. A guard that cannot answer returns an
// error for the caller to classify rather than reading as a stall.
//
// The order is the ranking: the reasons run from the most specific account of the halt to
// the least, and the first to fire is the one the run settles under. Budget and spend come
// first because a run that has used up what it was given has stopped for that reason
// whatever else was also true. No progress comes next, since a run that did nothing at all
// is better described that way than by what it was told. Non-convergence is last: it is
// what remains when a run was busy, was within its allowance, and still got nowhere, and it
// is the only one of the four that would otherwise have spent the whole budget to be
// reached as a budget reason.
func (g *Reconciler) stopGuard(ctx context.Context, r resource.Resource, spec Spec, status Status) (reason, message string, err error) {
	if status.Steps >= maxSteps(spec) {
		return "BudgetExhausted", "step budget exhausted before the stop condition was met", nil
	}
	// Our own ceiling on tokens, cost and share of the plan window, checked alongside the
	// step budget because a step is the wrong unit for cost. Crossing it names what the run
	// spent against what it was allowed, distinct from the step-budget reason and from a
	// provider pause or a transient retry, which this never touches.
	if !spec.Budget.IsZero() {
		reason, message, err := g.spendGuard(ctx, r, spec)
		if err != nil || reason != "" {
			return reason, message, err
		}
	}
	if status.StalledForNoProgress() {
		return "NoProgress", status.NoProgressReason(), nil
	}
	if status.StalledForNonConvergence() {
		return "NotConverging", status.NonConvergenceReason(), nil
	}
	return "", "", nil
}

func maxSteps(s Spec) int {
	if s.MaxSteps > 0 {
		return s.MaxSteps
	}
	return DefaultMaxSteps
}
