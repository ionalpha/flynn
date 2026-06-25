package budget

import (
	"context"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

type ctxKey struct{}

// Into returns a context carrying the budget id (the run id) a run charges
// against, so the dispatch waist's Hook reads the pool from the context rather
// than from a parameter. Binding it once at the top of a run applies the budget
// to every action that run dispatches.
func Into(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the budget id bound to ctx and whether one was present.
// Absent an id the run is unbudgeted (unlimited), the zero-config default.
func FromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok && id != ""
}

// Hook enforces a run's budget at the dispatch waist. Before rejects an action
// when the run's pool is already spent to its ceiling; After charges the action's
// metering onto the pool. Because the waist governs every model and tool call,
// one Hook caps the whole run with no per-call wiring. It composes with the
// capability admitter rather than replacing it: capability decides what may run,
// budget decides whether there is still budget to run it.
type Hook struct {
	ledger *Ledger
}

// NewHook returns a budget Hook backed by store. Add it to a dispatcher with
// dispatch.WithHook to budget every action that dispatcher governs.
func NewHook(store resource.Store) *Hook { return &Hook{ledger: NewLedger(store)} }

// Before rejects an action whose run has exhausted its budget. With no budget
// bound it admits everything, so an unbudgeted run is unconstrained.
func (h *Hook) Before(ctx context.Context, a dispatch.Action) error {
	id, ok := FromContext(ctx)
	if !ok {
		return nil
	}
	available, err := h.ledger.Available(ctx, id, resource.Scope(a.Scope))
	if err != nil {
		return err
	}
	if !available {
		return fault.New(fault.BudgetExceeded, "budget_exceeded",
			"run budget exhausted for action "+a.Name)
	}
	return nil
}

// After charges the action's metering onto the run's pool. It is best effort: the
// work has already run, so a charge that fails to persist is surfaced through the
// dispatcher's observability rather than failing the action. A zero metering (a
// rejected or free action) is a no-op.
func (h *Hook) After(ctx context.Context, a dispatch.Action, m dispatch.Metering, _ error) {
	id, ok := FromContext(ctx)
	if !ok {
		return
	}
	_ = h.ledger.Charge(ctx, id, resource.Scope(a.Scope), m)
}

var _ dispatch.Hook = (*Hook)(nil)
