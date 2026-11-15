package budget

import "context"

type tierKey struct{}

// TierInto attributes spend charged under ctx to tier in [Status.ByTier]. The pool
// bound with [Into] is still what is charged.
func TierInto(ctx context.Context, tier string) context.Context {
	return context.WithValue(ctx, tierKey{}, tier)
}

// TierFromContext returns the bound tier, if non-empty.
func TierFromContext(ctx context.Context) (string, bool) {
	tier, ok := ctx.Value(tierKey{}).(string)
	return tier, ok && tier != ""
}
