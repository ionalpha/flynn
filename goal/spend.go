package goal

import (
	"context"
	"fmt"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/resource"
)

// WindowSource reports how much of the current plan window has been consumed, as a
// fraction in [0,1]: 0 is an untouched window, 1 is a spent one. It is the seam an app
// implements over its own subscription's plan-window data. Flynn defines the port and
// enforces a goal's WindowFraction ceiling against it, but ships no source of its own,
// so a goal's window bound has no effect until one is wired with WithWindowSource.
// Keeping the source out of the runtime is deliberate: the window belongs to the
// account the app runs under, not to the agent, and the shape of that data differs per
// app. A nil source leaves the window axis unbounded, which is the zero-config default.
type WindowSource interface {
	Fraction(ctx context.Context) (float64, error)
}

// spendGuard reports whether the goal has crossed one of its own spend ceilings and,
// if so, the condition reason and the message that name what it spent against what it
// was allowed. An empty reason means the goal is still within budget.
//
// The ceilings are ours, so crossing one is a stop, evaluated here at the same
// reconcile point as the step budget. It reads recorded spend only and never
// reclassifies an error: a transient provider blip still backs off and retries, and a
// provider's own limit is still a pause, because neither flows through this guard. The
// token and cost axes read the spend recorded on the goal's pool (its BudgetPool, or
// its own name when it is its own pool); a goal whose pool has no budget resource, or
// no spend yet, reads as zero and never trips. The window axis is checked only when a
// source is wired.
func (g *Reconciler) spendGuard(ctx context.Context, r resource.Resource, spec Spec) (reason, message string, err error) {
	b := spec.Budget
	if b.Tokens > 0 || b.Cost > 0 {
		pool := spec.BudgetPool
		if pool == "" {
			pool = r.Name
		}
		status, err := budget.NewLedger(g.store).Spend(ctx, pool, r.Scope)
		if err != nil {
			return "", "", err
		}
		spent := status.Spent
		if b.Tokens > 0 && spent.Tokens >= b.Tokens {
			return "SpendBudgetExhausted",
				fmt.Sprintf("token budget exhausted: spent %d of %d allowed", spent.Tokens, b.Tokens), nil
		}
		if b.Cost > 0 && spent.Cost >= b.Cost {
			return "SpendBudgetExhausted",
				fmt.Sprintf("cost budget exhausted: spent %.4f of %.4f allowed", spent.Cost, b.Cost), nil
		}
	}
	if b.WindowFraction > 0 && g.window != nil {
		frac, err := g.window.Fraction(ctx)
		if err != nil {
			return "", "", err
		}
		if frac >= b.WindowFraction {
			return "WindowBudgetExhausted",
				fmt.Sprintf("plan-window budget exhausted: %.1f%% of the window used, ceiling %.1f%%", frac*100, b.WindowFraction*100), nil
		}
	}
	return "", "", nil
}
