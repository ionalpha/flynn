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

// admit checks the desired-state records a reconcile reads (ledger, unit graph, terms,
// steers, kill order) and brings the status's observation of each into line. Everything
// it refuses is a terminal spec fault: each means the definition the run is judged
// against changed underneath it.
//
// A unit graph with a cycle, a dangling edge or an unprovable unit is refused whole
// before dispatch, since finding it mid fan-out leaves children running against it. A
// unit altered or dropped after its child was created is work in flight being
// rewritten; units nothing has been spent on may still change.
//
// An invariant or steer dropped or reworded after adoption is refused whether or not an
// auditor is wired. Adding one is always allowed: tightening is the author's, loosening
// is nobody's, and a further steer is how an operator corrects a wrong one.
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
	// Relaxation is checked before validity, the reverse of the ledger and unit graph
	// above. A reworded term is usually also invalid, and both diagnoses are true, but
	// the relaxation names what the run just did where the validity fault would send
	// the author to fix wording they were not allowed to touch.
	if err := status.ValidateInvariantsAdopted(spec.Invariants); err != nil {
		return fault.Wrap(fault.Terminal, "goal_invariant_relaxed", err)
	}
	if err := ValidateInvariants(spec.Invariants); err != nil {
		// An unsearchable term gets its own code: an author can fix it in a line, and
		// a generic "invalid" would bury that instruction.
		code := "goal_invariants_invalid"
		if errors.Is(err, ErrInvariantUnsearchable) {
			code = "goal_invariant_unsearchable"
		}
		return fault.Wrap(fault.Terminal, code, err)
	}
	status.SyncInvariants(spec.Invariants)
	// Withdrawal is checked before validity, as for invariants.
	if err := status.ValidateSteersGiven(spec.Steers); err != nil {
		return fault.Wrap(fault.Terminal, "goal_steer_withdrawn", err)
	}
	if err := ValidateSteers(spec.Steers); err != nil {
		return fault.Wrap(fault.Terminal, "goal_steers_invalid", err)
	}
	status.SyncSteers(spec.Steers)
	// A kill taken off the spec after the run stopped under it is refused so the
	// record never reads as a run stopping on its own when a person stopped it.
	if err := status.ValidateKillGiven(spec.Kill); err != nil {
		return fault.Wrap(fault.Terminal, "goal_kill_withdrawn", err)
	}
	return nil
}

// stopGuard reports the reason a goal that has not converged must stop anyway, with
// the message that reason carries, or "" to keep going. A guard that cannot answer
// returns an error for the caller to classify rather than reading as a stall.
//
// The order is the ranking, from the most specific account of the halt to the least,
// and the first to fire is the one the run settles under. Budget and spend come first
// because a run that used up what it was given stopped for that reason whatever else
// was true. No progress is next: a run that did nothing is better described that way
// than by what it was told. A check the host cannot run comes before that, since a run
// that did nothing under a check that cannot pass is better described by the check.
// Non-convergence is last, what remains when a run was busy,
// within its allowance, and still got nowhere; it is the one that would otherwise
// have spent the whole budget to be reached as a budget reason.
func (g *Reconciler) stopGuard(ctx context.Context, r resource.Resource, spec Spec, status Status) (reason, message string, err error) {
	if status.Steps >= maxSteps(spec) {
		return "BudgetExhausted", "step budget exhausted before the stop condition was met", nil
	}
	// Our own ceiling on tokens, cost and share of the plan window, checked beside
	// the step budget because a step is the wrong unit for cost. It never touches a
	// provider pause or a transient retry.
	if !spec.Budget.IsZero() {
		reason, message, err := g.spendGuard(ctx, r, spec)
		if err != nil || reason != "" {
			return reason, message, err
		}
	}
	if status.StalledForEnvironment() {
		return "CheckCannotRun", status.EnvironmentReason(), nil
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
