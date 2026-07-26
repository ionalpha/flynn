package budget_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// approx compares two costs within floating-point rounding tolerance, since a sum of
// decimal costs (0.10 + 0.05) is not representable exactly in binary.
func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// status reads the full recorded budget status for a run.
func status(t *testing.T, s resource.Store, id string) budget.Status {
	t.Helper()
	r, err := s.Get(context.Background(), budget.Kind, resource.Scope{}, id)
	if err != nil {
		t.Fatalf("get budget: %v", err)
	}
	st, err := budget.DecodeStatus(r)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st
}

// TestChargeAttributesToTier proves a charge made under a tier context lands in both
// the aggregate Spent and that tier's column, so the per-tier ledger is a faithful
// breakdown of the same total the ceiling is enforced against.
func TestChargeAttributesToTier(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	l := budget.NewLedger(s)
	if _, err := l.Open(ctx, "run", resource.Scope{}, budget.Limits{}); err != nil {
		t.Fatal(err)
	}

	cheap := budget.TierInto(ctx, "cheap")
	premium := budget.TierInto(ctx, "premium")
	if err := l.Charge(cheap, "run", resource.Scope{}, dispatch.Metering{Tokens: 30, Cost: 0.10}); err != nil {
		t.Fatal(err)
	}
	if err := l.Charge(premium, "run", resource.Scope{}, dispatch.Metering{Tokens: 70, Cost: 0.90}); err != nil {
		t.Fatal(err)
	}
	if err := l.Charge(cheap, "run", resource.Scope{}, dispatch.Metering{Tokens: 5, Cost: 0.05}); err != nil {
		t.Fatal(err)
	}

	st := status(t, s, "run")
	if st.Spent.Tokens != 105 {
		t.Fatalf("aggregate tokens = %d, want 105", st.Spent.Tokens)
	}
	if got := st.ByTier["cheap"]; got.Tokens != 35 || !approx(got.Cost, 0.15) {
		t.Fatalf("cheap tier = %+v, want {35, 0.15}", got)
	}
	if got := st.ByTier["premium"]; got.Tokens != 70 || !approx(got.Cost, 0.90) {
		t.Fatalf("premium tier = %+v, want {70, 0.90}", got)
	}
	// Attribution is a partition of the aggregate when every charge is tiered.
	var sum int64
	for _, v := range st.ByTier {
		sum += v.Tokens
	}
	if sum != st.Spent.Tokens {
		t.Fatalf("per-tier tokens sum to %d, want the aggregate %d", sum, st.Spent.Tokens)
	}
}

// TestSettleAttributesToTier proves the reserve-then-settle path attributes the
// settled actual to the tier too, so a budget driven through the concurrent-safe
// reservation path still produces a per-tier breakdown.
func TestSettleAttributesToTier(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	l := budget.NewLedger(s)
	if _, err := l.Open(ctx, "run", resource.Scope{}, budget.Limits{}); err != nil {
		t.Fatal(err)
	}

	est := budget.Spent{Tokens: 100}
	if _, err := l.Reserve(ctx, "run", resource.Scope{}, est); err != nil {
		t.Fatal(err)
	}
	tctx := budget.TierInto(ctx, "premium")
	if err := l.Settle(tctx, "run", resource.Scope{}, est, dispatch.Metering{Tokens: 80, Cost: 1.25}); err != nil {
		t.Fatal(err)
	}

	st := status(t, s, "run")
	if !st.Reserved.IsZero() {
		t.Fatalf("reservation not released: %+v", st.Reserved)
	}
	if st.Spent.Tokens != 80 {
		t.Fatalf("aggregate tokens = %d, want 80", st.Spent.Tokens)
	}
	if got := st.ByTier["premium"]; got.Tokens != 80 || got.Cost != 1.25 {
		t.Fatalf("premium tier = %+v, want {80, 1.25}", got)
	}
}

// TestUntieredChargeLeavesLedgerEmpty confirms the zero-config default: a charge made
// with no tier bound records only the aggregate, so an unattributed run marshals the
// same status it did before per-tier attribution existed.
func TestUntieredChargeLeavesLedgerEmpty(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	l := budget.NewLedger(s)
	if _, err := l.Open(ctx, "run", resource.Scope{}, budget.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := l.Charge(ctx, "run", resource.Scope{}, dispatch.Metering{Tokens: 40}); err != nil {
		t.Fatal(err)
	}
	st := status(t, s, "run")
	if st.Spent.Tokens != 40 {
		t.Fatalf("aggregate tokens = %d, want 40", st.Spent.Tokens)
	}
	if st.ByTier != nil {
		t.Fatalf("untiered charge recorded a per-tier entry: %+v", st.ByTier)
	}
}

// TestTierFromContext covers the accessor: an empty tier reads as absent so it is
// never used as a map key, and a bound tier reads back.
func TestTierFromContext(t *testing.T) {
	if tier, ok := budget.TierFromContext(context.Background()); ok || tier != "" {
		t.Fatalf("bare context = (%q, %v), want (\"\", false)", tier, ok)
	}
	if tier, ok := budget.TierFromContext(budget.TierInto(context.Background(), "")); ok || tier != "" {
		t.Fatalf("empty tier = (%q, %v), want (\"\", false)", tier, ok)
	}
	if tier, ok := budget.TierFromContext(budget.TierInto(context.Background(), "cheap")); !ok || tier != "cheap" {
		t.Fatalf("bound tier = (%q, %v), want (\"cheap\", true)", tier, ok)
	}
}

// TestSpendReadsRecordedStatus proves Spend returns the recorded totals for a bound
// budget and the zero Status for an unbudgeted run, so the reconciler's spend guard
// reads real numbers when a pool exists and a clean zero when none does.
func TestSpendReadsRecordedStatus(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	l := budget.NewLedger(s)

	// No budget resource: reads as nothing spent, no error.
	if st, err := l.Spend(ctx, "absent", resource.Scope{}); err != nil || !st.Spent.IsZero() {
		t.Fatalf("Spend(absent) = (%+v, %v), want (zero, nil)", st, err)
	}

	if _, err := l.Open(ctx, "run", resource.Scope{}, budget.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := l.Charge(budget.TierInto(ctx, "cheap"), "run", resource.Scope{}, dispatch.Metering{Tokens: 25, Cost: 0.5}); err != nil {
		t.Fatal(err)
	}
	st, err := l.Spend(ctx, "run", resource.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Spent.Tokens != 25 || st.Spent.Cost != 0.5 {
		t.Fatalf("Spend spent = %+v, want {25, 0.5}", st.Spent)
	}
	if got := st.ByTier["cheap"]; got.Tokens != 25 {
		t.Fatalf("Spend cheap tier = %+v, want tokens 25", got)
	}
}

// getFailStore is a store whose Get always fails, so a read path's error branch can be
// exercised without a real backend outage.
type getFailStore struct {
	resource.Store
	err error
}

func (s getFailStore) Get(context.Context, string, resource.Scope, string) (resource.Resource, error) {
	return resource.Resource{}, s.err
}

// TestSpendSurfacesStoreError proves Spend does not swallow a real read failure as
// "nothing spent": a store error that is not ErrNotFound is returned, so the reconciler
// retries rather than reading a broken pool as under budget.
func TestSpendSurfacesStoreError(t *testing.T) {
	boom := fault.New(fault.Transient, "store_down", "store unavailable")
	l := budget.NewLedger(getFailStore{Store: newStore(t), err: boom})
	if _, err := l.Spend(context.Background(), "run", resource.Scope{}); !errors.Is(err, boom) {
		t.Fatalf("Spend error = %v, want the store failure", err)
	}
}

// TestConcurrentTierChargesAllLand proves per-tier attribution stays correct under
// contention: many goroutines charging different tiers on one pool converge on the
// exact per-tier and aggregate totals (run with -race).
func TestConcurrentTierChargesAllLand(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	l := budget.NewLedger(s)
	if _, err := l.Open(ctx, "run", resource.Scope{}, budget.Limits{}); err != nil {
		t.Fatal(err)
	}

	const each = 40
	tiers := []string{"cheap", "premium", "local"}
	var wg sync.WaitGroup
	for _, tier := range tiers {
		wg.Add(1)
		go func(tier string) {
			defer wg.Done()
			tctx := budget.TierInto(ctx, tier)
			for range each {
				if err := l.Charge(tctx, "run", resource.Scope{}, dispatch.Metering{Tokens: 1}); err != nil {
					t.Errorf("charge %s: %v", tier, err)
				}
			}
		}(tier)
	}
	wg.Wait()

	st := status(t, s, "run")
	if st.Spent.Tokens != int64(len(tiers)*each) {
		t.Fatalf("aggregate tokens = %d, want %d", st.Spent.Tokens, len(tiers)*each)
	}
	for _, tier := range tiers {
		if got := st.ByTier[tier].Tokens; got != each {
			t.Fatalf("tier %s tokens = %d, want %d", tier, got, each)
		}
	}
}
