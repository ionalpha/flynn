package goal

import (
	"context"
	"errors"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// admit refuses with a terminal fault any change to what the run is judged against. A bad
// unit graph is refused before dispatch, because finding it mid fan-out leaves children
// running. A dispatched unit may not change; an unspent one may. An invariant or steer
// may be added but never dropped or reworded: tightening is the author's, loosening is
// nobody's.
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
	// Relaxation before validity, because a reworded term is usually also invalid and the
	// validity fault would send the author to fix wording they may not touch.
	if err := status.ValidateInvariantsAdopted(spec.Invariants); err != nil {
		return fault.Wrap(fault.Terminal, "goal_invariant_relaxed", err)
	}
	if err := ValidateInvariants(spec.Invariants); err != nil {
		// Its own code, because the author can fix it in a line.
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
	// So the record never reads as a run stopping on its own when a person stopped it.
	if err := status.ValidateKillGiven(spec.Kill); err != nil {
		return fault.Wrap(fault.Terminal, "goal_kill_withdrawn", err)
	}
	return nil
}

// stopGuard returns "" to keep going, and an error when a guard cannot answer. The order
// ranks the most specific account first: budget, then a check the host cannot run, then
// no progress, then non-convergence, which catches a busy run before it spends the whole
// budget.
func (g *Reconciler) stopGuard(ctx context.Context, r resource.Resource, spec Spec, status Status) (reason, message string, err error) {
	if status.Steps >= maxSteps(spec) {
		return "BudgetExhausted", "step budget exhausted before the stop condition was met", nil
	}
	// Beside the step budget because a step is the wrong unit for cost.
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
