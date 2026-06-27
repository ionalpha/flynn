package budget_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// reserved reads the reserved (not-yet-spent) total on a run's budget.
func reserved(t *testing.T, s resource.Store, id string) budget.Spent {
	t.Helper()
	r, err := s.Get(context.Background(), budget.Kind, resource.Scope{}, id)
	if err != nil {
		t.Fatalf("get budget: %v", err)
	}
	st, err := budget.DecodeStatus(r)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st.Reserved
}

func openBudget(t *testing.T, l *budget.Ledger, id string, limit budget.Limits) {
	t.Helper()
	if _, err := l.Open(context.Background(), id, resource.Scope{}, limit); err != nil {
		t.Fatalf("open budget: %v", err)
	}
}

func TestReserveAdmitsUntilPoolIsCommitted(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	l := budget.NewLedger(s)
	openBudget(t, l, "run", budget.Limits{Tokens: 100})

	est := budget.Spent{Tokens: 30}
	admitted := 0
	for range 6 {
		ok, err := l.Reserve(ctx, "run", resource.Scope{}, est)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if ok {
			admitted++
		}
	}
	// Admit while committed < limit: 0,30,60,90 are all under 100, so the 4th
	// admit (committed -> 120) is the last; the pool is then over-committed and
	// further reserves are refused. Overshoot is bounded to one estimate.
	if admitted != 4 {
		t.Fatalf("admitted %d, want 4", admitted)
	}
	if got := reserved(t, s, "run"); got.Tokens != 120 {
		t.Fatalf("reserved %d tokens, want 120", got.Tokens)
	}
}

func TestUnbudgetedReserveAlwaysAdmits(t *testing.T) {
	l := budget.NewLedger(newStore(t))
	ok, err := l.Reserve(context.Background(), "no-budget", resource.Scope{}, budget.Spent{Tokens: 1_000_000})
	if err != nil || !ok {
		t.Fatalf("an unbudgeted run must always admit: ok=%v err=%v", ok, err)
	}
}

// TestReserveIsAtomicUnderConcurrency is the core guarantee: when many actions
// race to reserve against one shared pool, the atomic check-and-reserve admits
// exactly the number the pool allows and no more, so a concurrent fan-out cannot
// all pass an under-budget check and overshoot. Run with -race.
func TestReserveIsAtomicUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	l := budget.NewLedger(s)
	openBudget(t, l, "run", budget.Limits{Tokens: 100})

	const racers = 50
	est := budget.Spent{Tokens: 10}
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := l.Reserve(ctx, "run", resource.Scope{}, est)
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			if ok {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()

	// committed climbs 10 at a time and the gate refuses once it reaches 100, so
	// exactly 10 reserves admit no matter the interleaving.
	if got := admitted.Load(); got != 10 {
		t.Fatalf("admitted %d, want exactly 10 (no overshoot under concurrency)", got)
	}
	if got := reserved(t, s, "run"); got.Tokens != 100 {
		t.Fatalf("reserved %d tokens, want 100", got.Tokens)
	}
}

func TestSettleReleasesReservationAndChargesActual(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	l := budget.NewLedger(s)
	openBudget(t, l, "run", budget.Limits{Tokens: 100})

	est := budget.Spent{Tokens: 30}
	if _, err := l.Reserve(ctx, "run", resource.Scope{}, est); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := l.Settle(ctx, "run", resource.Scope{}, est, dispatch.Metering{Tokens: 18}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := reserved(t, s, "run"); !got.IsZero() {
		t.Fatalf("reserved should be released to zero, got %+v", got)
	}
	if got := spent(t, s, "run"); got.Tokens != 18 {
		t.Fatalf("spent %d tokens, want the actual 18", got.Tokens)
	}
}

func TestReleaseFloorsAtZero(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	l := budget.NewLedger(s)
	openBudget(t, l, "run", budget.Limits{Tokens: 100})

	if _, err := l.Reserve(ctx, "run", resource.Scope{}, budget.Spent{Tokens: 10}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Releasing more than was reserved must not drive the reserved total negative.
	if err := l.Release(ctx, "run", resource.Scope{}, budget.Spent{Tokens: 30}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := reserved(t, s, "run"); got.Tokens != 0 {
		t.Fatalf("reserved %d, want 0 (floored)", got.Tokens)
	}
}

// TestReservingHookRejectsThroughTheWaist proves the reservation hook closes the
// overshoot gap end to end at the dispatch waist: once the pool is committed,
// further actions are refused with a BudgetExceeded fault and never run.
func TestReservingHookRejectsThroughTheWaist(t *testing.T) {
	s := newStore(t)
	l := budget.NewLedger(s)
	openBudget(t, l, "run", budget.Limits{Tokens: 100})

	h := budget.NewHook(s, budget.WithReservation(budget.Spent{Tokens: 60}))
	d := dispatch.New(dispatch.WithHook(h))
	ctx := budget.Into(context.Background(), "run")

	ran := 0
	work := func(context.Context) (dispatch.Metering, error) { ran++; return dispatch.Metering{Tokens: 50}, nil }

	// First action: committed 0 -> reserve 60, runs, settles to spent 50.
	if err := d.Govern(ctx, dispatch.Action{Name: "model.generate"}, work); err != nil {
		t.Fatalf("first action should be admitted: %v", err)
	}
	// Second: committed is spent 50 < 100 -> reserve 60 (committed 110), runs, spent 100.
	if err := d.Govern(ctx, dispatch.Action{Name: "model.generate"}, work); err != nil {
		t.Fatalf("second action should be admitted: %v", err)
	}
	// Third: committed (spent 100) >= 100 -> refused, work must not run.
	err := d.Govern(ctx, dispatch.Action{Name: "model.generate"}, work)
	if got := fault.Classify(err); got != fault.BudgetExceeded {
		t.Fatalf("third action should be BudgetExceeded, got %s: %v", got, err)
	}
	if ran != 2 {
		t.Fatalf("work ran %d times, want 2 (the over-budget action must not run)", ran)
	}
}
