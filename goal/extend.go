package goal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// BudgetExtendedReason is the reason on the stall condition when a person's extension
// released a run stopped on its budget. Like [AllowanceResumeReason] it is written as
// the stall going False, so the record shows the stop being answered and not only
// ending.
const BudgetExtendedReason = "BudgetExtended"

// spendStopReason is the stall reason the spend guard settles a goal under (see
// Reconciler.spendGuard); it is the only stall that names this goal's own ceiling.
const spendStopReason = "SpendBudgetExhausted"

// StoppedOnBudget reports whether a goal's status records a stop caused by a spent
// token or cost ceiling, which is the one stop a person's extension answers.
//
// Two shapes count. The goal's own ceiling settles it under the spend stop. The run's
// ledger ceiling is enforced at the dispatch waist, so it surfaces as a step that died
// with a [fault.BudgetExceeded] fault, and the step failure carries the fault's code in
// its message. The step budget (MaxSteps), a pause on the plan window and every other
// stall are different questions: more tokens do not answer them, so they are not
// extendable.
func StoppedOnBudget(s Status) bool {
	if s.Phase != PhaseStalled {
		return false
	}
	for _, c := range s.Conditions {
		if c.Type != CondStalled || c.Status != "True" {
			continue
		}
		switch c.Reason {
		case spendStopReason:
			return true
		case "StepFailed":
			return strings.Contains(c.Message, string(fault.BudgetExceeded))
		}
	}
	return false
}

// BudgetReopener is the goal side of [budget.Ledger.Extend] for one goal. It implements
// [budget.Reopener].
type BudgetReopener struct {
	store resource.Store
	scope resource.Scope
	name  string
	clk   clock.Clock
}

// NewBudgetReopener returns the reopener for the goal called name in scope.
func NewBudgetReopener(store resource.Store, scope resource.Scope, name string, clk clock.Clock) *BudgetReopener {
	return &BudgetReopener{store: store, scope: scope, name: name, clk: clk}
}

var _ budget.Reopener = (*BudgetReopener)(nil)

// Admit refuses a goal that cannot be extended without writing anything. Reopen runs the
// same check against the record it writes, so this is the early refusal and not the
// guard: a goal killed between the two still stays killed.
func (b *BudgetReopener) Admit(ctx context.Context) error {
	r, err := b.store.Get(ctx, Kind, b.scope, b.name)
	if err != nil {
		return err
	}
	_, _, err = admitExtension(r)
	return err
}

// Reopen raises the goal's spend ceiling to ceiling and releases the budget stop, in one
// compare-and-set write, so the goal can never be left running on a ceiling that
// disagrees with the run's or reopened without the ceiling that lets it proceed.
//
// A goal's own ceiling is only ever raised: if it already sits above the run's, it keeps
// its value. An axis the goal never set is set to the run's, so the two agree and the
// goal does not stop again below the run's ceiling. The status is moved to running with
// its stall cleared, because a stalled goal whose spec hash did not change (its ceiling
// was never set) would otherwise be skipped as already settled.
//
// A goal that is no longer stopped on its budget when this writes has been reopened by
// a concurrent extension (or has moved on): its ceiling is still raised, so the two
// ceilings agree whichever extension wrote last, and its status is left as it is. A kill
// is the exception: a killed goal is never touched, and the error says so.
func (b *BudgetReopener) Reopen(ctx context.Context, ceiling budget.Limits) error {
	_, err := resource.Update(ctx, b.store, Kind, b.scope, b.name, func(r *resource.Resource) error {
		spec, status, err := decodeForExtension(*r)
		if err != nil {
			return err
		}
		if ceiling.Tokens > spec.Budget.Tokens {
			spec.Budget.Tokens = ceiling.Tokens
		}
		if ceiling.Cost > spec.Budget.Cost {
			spec.Budget.Cost = ceiling.Cost
		}
		if StoppedOnBudget(status) {
			now := b.clk.Now()
			status.Phase = PhaseRunning
			status.Message = ""
			status.Unwired = false
			status.InFlight = nil
			status.SetCondition(Condition{
				Type: CondStalled, Status: "False", Reason: BudgetExtendedReason,
				Message: "a person raised the budget the run stopped on",
			}, now)
			status.SetCondition(Condition{Type: CondReconciling, Status: "True", Reason: BudgetExtendedReason}, now)
		}
		rawSpec, err := json.Marshal(spec)
		if err != nil {
			return fault.Wrap(fault.Terminal, "goal_spec_encode", err)
		}
		rawStatus, err := status.Encode()
		if err != nil {
			return fault.Wrap(fault.Terminal, "goal_status_encode", err)
		}
		r.Spec, r.Status = rawSpec, rawStatus
		return nil
	})
	if errors.Is(err, resource.ErrConflict) {
		return fault.Wrap(fault.Transient, "goal_write_conflict", err)
	}
	return err
}

// admitExtension decodes the goal and decides whether it may be extended. A kill comes
// first: a run an operator stopped stays stopped whatever else is true of it, and a kill
// still on the spec is as final as one the reconciler has already settled.
func admitExtension(r resource.Resource) (Spec, Status, error) {
	spec, status, err := decodeForExtension(r)
	if err != nil {
		return Spec{}, Status{}, err
	}
	if !StoppedOnBudget(status) {
		return Spec{}, Status{}, budget.ErrNotBudgetStop
	}
	return spec, status, nil
}

// decodeForExtension decodes the goal and refuses a killed one.
func decodeForExtension(r resource.Resource) (Spec, Status, error) {
	spec, err := DecodeSpec(r)
	if err != nil {
		return Spec{}, Status{}, fault.Wrap(fault.Terminal, "goal_spec_decode", err)
	}
	status, err := DecodeStatus(r)
	if err != nil {
		return Spec{}, Status{}, fault.Wrap(fault.Terminal, "goal_status_decode", err)
	}
	if spec.Kill != nil || status.Killed != nil {
		return Spec{}, Status{}, budget.ErrKilled
	}
	return spec, status, nil
}
