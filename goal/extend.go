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

// BudgetExtendedReason is written with the stall going False, like
// [AllowanceResumeReason], so the record shows the stop answered.
const BudgetExtendedReason = "BudgetExtended"

const spendStopReason = "SpendBudgetExhausted"

// StoppedOnBudget reports a token or cost stop, the only stop an extension answers. The
// run's ledger ceiling is enforced in dispatch, so it shows as a failed step carrying the
// [fault.BudgetExceeded] code. MaxSteps and the plan window are not extendable, because
// more tokens do not answer them.
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

// BudgetReopener is the [budget.Reopener] for one goal, used by [budget.Ledger.Extend].
type BudgetReopener struct {
	store resource.Store
	scope resource.Scope
	name  string
	clk   clock.Clock
}

// NewBudgetReopener does no I/O; the goal is read by Admit and Reopen.
func NewBudgetReopener(store resource.Store, scope resource.Scope, name string, clk clock.Clock) *BudgetReopener {
	return &BudgetReopener{store: store, scope: scope, name: name, clk: clk}
}

var _ budget.Reopener = (*BudgetReopener)(nil)

// Admit is the early refusal. Reopen repeats the check on the record it writes, so a goal
// killed in between stays killed.
func (b *BudgetReopener) Admit(ctx context.Context) error {
	r, err := b.store.Get(ctx, Kind, b.scope, b.name)
	if err != nil {
		return err
	}
	_, _, err = admitExtension(r)
	return err
}

// Reopen raises the ceiling and releases the stop in one compare-and-set write, so the
// goal never runs on a ceiling that disagrees with the run's. A ceiling is only raised.
// The status moves to running explicitly, because an unchanged spec hash would leave the
// goal settled. A goal a concurrent extension already reopened still gets the raise. A
// killed goal is never touched.
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

// admitExtension checks the kill first, because a kill on the spec is final even before
// the reconciler settles it.
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
