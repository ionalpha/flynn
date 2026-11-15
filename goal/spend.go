package goal

import (
	"context"
	"fmt"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/resource"
)

// WindowSource reports the consumed fraction of the plan window, in [0,1]. The app
// supplies it because the window belongs to its account. A goal with a WindowFraction and
// no source stalls with WindowSourceMissing, because an unchecked ceiling looks the same
// as one that held; wiring a source resumes it.
type WindowSource interface {
	Fraction(ctx context.Context) (float64, error)
}

// spendGuard returns an empty reason while the goal is within its ceilings. It reads
// recorded spend only, so a provider's errors and limits keep their own handling. A pool
// with no budget resource reads as zero.
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
	if b.WindowFraction > 0 {
		if g.window == nil {
			return "WindowSourceMissing",
				fmt.Sprintf("the goal caps its share of the plan window at %.1f%% and no window source is wired to measure it", b.WindowFraction*100), nil
		}
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
