package budget

import (
	"context"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

type ctxKey struct{}

// Into binds the budget id (the run id) every action under ctx charges.
func Into(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the bound budget id. Without one the run is unlimited.
func FromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok && id != ""
}

// Hook is the budget's [dispatch.Hook]. Before refuses an action once the pool is spent
// and After charges its metering. With [WithReservation], Before reserves an estimate
// and After settles it, so a fan-out cannot all pass on one under-budget snapshot.
type Hook struct {
	ledger  *Ledger
	reserve Spent // zero disables reservation
}

// HookOption configures a budget Hook.
type HookOption func(*Hook)

// WithReservation reserves est before each action. An upper bound on one action's spend
// (input plus max output tokens for a model call) keeps a concurrent fan-out under the
// ceiling. Zero, the default, checks then charges.
func WithReservation(est Spent) HookOption {
	return func(h *Hook) { h.reserve = est }
}

// NewHook returns a Hook backed by store, for [dispatch.WithHook].
func NewHook(store resource.Store, opts ...HookOption) *Hook {
	h := &Hook{ledger: NewLedger(store)}
	for _, o := range opts {
		o(h)
	}
	return h
}

func (h *Hook) reserving() bool { return !h.reserve.IsZero() }

// Before refuses with fault.BudgetExceeded once the pool is spent, or fully committed
// when reserving.
func (h *Hook) Before(ctx context.Context, a dispatch.Action) error {
	id, ok := FromContext(ctx)
	if !ok {
		return nil
	}
	scope := resource.Scope(a.Scope)
	if h.reserving() {
		admitted, err := h.ledger.Reserve(ctx, id, scope, h.reserve)
		if err != nil {
			return err
		}
		if !admitted {
			return fault.New(fault.BudgetExceeded, "budget_exceeded",
				"run budget exhausted for action "+a.Name)
		}
		return nil
	}
	available, err := h.ledger.Available(ctx, id, scope)
	if err != nil {
		return err
	}
	if !available {
		return fault.New(fault.BudgetExceeded, "budget_exceeded",
			"run budget exhausted for action "+a.Name)
	}
	return nil
}

// After charges or settles the action's spend. The action has already run, so a failed
// write is dropped rather than failing it.
func (h *Hook) After(ctx context.Context, a dispatch.Action, m dispatch.Metering, _ error) {
	id, ok := FromContext(ctx)
	if !ok {
		return
	}
	scope := resource.Scope(a.Scope)
	if h.reserving() {
		_ = h.ledger.Settle(ctx, id, scope, h.reserve, m)
		return
	}
	_ = h.ledger.Charge(ctx, id, scope, m)
}

var _ dispatch.Hook = (*Hook)(nil)
