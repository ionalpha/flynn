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
//
// With a reservation configured (WithReservation), Before instead reserves an
// upper-bound estimate against the pool before admitting, and After settles the
// reservation into the actual metered spend. This closes the concurrent-overshoot
// gap a plain check-then-charge leaves: when many actions share one pool (a
// fan-out of children), each would otherwise read the same under-budget snapshot
// and all pass; the atomic reserve makes them admit against one consistent view.
type Hook struct {
	ledger  *Ledger
	reserve Spent // per-action upper-bound reservation; zero disables reserve-before-dispatch
}

// HookOption configures a budget Hook.
type HookOption func(*Hook)

// WithReservation makes the Hook reserve an upper-bound estimate against the pool
// before each action and settle the actual after, instead of a plain check-then-
// charge. The estimate should be an upper bound on a single action's spend (for a
// model call, the input plus the max output tokens), so the pool can never be
// exceeded even under a concurrent fan-out. A zero estimate (the default) keeps the
// original check-then-charge behaviour.
func WithReservation(est Spent) HookOption {
	return func(h *Hook) { h.reserve = est }
}

// NewHook returns a budget Hook backed by store. Add it to a dispatcher with
// dispatch.WithHook to budget every action that dispatcher governs.
func NewHook(store resource.Store, opts ...HookOption) *Hook {
	h := &Hook{ledger: NewLedger(store)}
	for _, o := range opts {
		o(h)
	}
	return h
}

// reserving reports whether the Hook is in reserve-before-dispatch mode.
func (h *Hook) reserving() bool { return !h.reserve.IsZero() }

// Before rejects an action whose run has exhausted its budget. In reservation mode
// it atomically reserves the estimate and rejects when the pool is fully committed;
// otherwise it reads availability. With no budget bound it admits everything, so an
// unbudgeted run is unconstrained.
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

// After records the action's spend. In reservation mode it settles the reservation
// into the actual metering (releasing the estimate and charging the actual in one
// write, so a zero metering simply releases the reservation); otherwise it charges
// the metering. It is best effort: the work has already run, so a write that fails
// to persist is surfaced through the dispatcher's observability rather than failing
// the action.
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
