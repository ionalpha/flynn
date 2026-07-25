package budget

import "context"

type tierKey struct{}

// TierInto returns a context that attributes any spend charged under it to the named
// model tier, so a shared pool's per-tier ledger records which tier the tokens and
// cost went to. It is bound alongside the pool (see Into): the pool decides what is
// charged, the tier decides which column of the ledger it lands in. An empty tier, or
// no TierInto at all, leaves the spend unattributed (charged to the aggregate Spent
// only), which is the zero-config default.
func TierInto(ctx context.Context, tier string) context.Context {
	return context.WithValue(ctx, tierKey{}, tier)
}

// TierFromContext returns the tier bound to ctx and whether a non-empty one was
// present. Absent a tier the spend is attributed to no column, only to the aggregate.
func TierFromContext(ctx context.Context) (string, bool) {
	tier, ok := ctx.Value(tierKey{}).(string)
	return tier, ok && tier != ""
}
