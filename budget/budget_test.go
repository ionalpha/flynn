package budget_test

import (
	"context"
	"sync"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

// newStore returns an in-memory resource store with the Budget kind registered.
func newStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := budget.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	return resource.NewMemory(reg)
}

func spent(t *testing.T, s resource.Store, id string) budget.Spent {
	t.Helper()
	r, err := s.Get(context.Background(), budget.Kind, resource.Scope{}, id)
	if err != nil {
		t.Fatalf("get budget: %v", err)
	}
	st, err := budget.DecodeStatus(r)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st.Spent
}

// TestChargeAccumulatesProperty is the rigor property: charging a run a sequence
// of meterings records exactly their sum, so the ledger is a faithful running
// total no matter how many actions a run takes.
func TestChargeAccumulatesProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		s := newStore(t)
		l := budget.NewLedger(s)
		if _, err := l.Open(ctx, "run", resource.Scope{}, budget.Limits{}); err != nil {
			rt.Fatalf("open: %v", err)
		}

		var sum int64
		n := rapid.IntRange(0, 25).Draw(rt, "charges")
		for range n {
			tok := rapid.IntRange(0, 5000).Draw(rt, "tokens")
			if err := l.Charge(ctx, "run", resource.Scope{}, dispatch.Metering{Tokens: tok}); err != nil {
				rt.Fatalf("charge: %v", err)
			}
			sum += int64(tok)
		}
		if got := spent(t, s, "run").Tokens; got != sum {
			rt.Fatalf("spent tokens = %d, want %d", got, sum)
		}
	})
}

// TestChargeIsAtomicUnderConcurrency proves the shared pool stays correct when
// concurrent runs charge it at once: the optimistic-concurrency retry makes every
// charge land, so the total is the exact sum even under contention (run with -race).
func TestChargeIsAtomicUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	l := budget.NewLedger(s)
	if _, err := l.Open(ctx, "run", resource.Scope{}, budget.Limits{}); err != nil {
		t.Fatal(err)
	}

	const workers, each = 8, 25
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				if err := l.Charge(ctx, "run", resource.Scope{}, dispatch.Metering{Tokens: 1}); err != nil {
					t.Errorf("charge: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	if got := spent(t, s, "run").Tokens; got != workers*each {
		t.Fatalf("concurrent charges lost writes: spent = %d, want %d", got, workers*each)
	}
}

// TestUnbudgetedRunIsUnlimited confirms the zero-config posture: with no budget id
// bound, the hook admits every action and charges nothing.
func TestUnbudgetedRunIsUnlimited(t *testing.T) {
	s := newStore(t)
	h := budget.NewHook(s)
	// No budget.Into, so the context carries no id.
	if err := h.Before(context.Background(), dispatch.Action{Name: "model.generate"}); err != nil {
		t.Fatalf("unbudgeted run must be admitted: %v", err)
	}
}

// TestHookEnforcesCeilingThroughTheWaist drives real actions through a dispatcher
// carrying the budget hook and proves the ceiling is enforced: actions run while
// budget remains and are rejected with BudgetExceeded once spend reaches the
// limit. The check is pre-execution and the charge is post-execution, so an action
// that crosses the line still runs (the recorded spend settles just past the cap),
// and the next action is denied.
func TestHookEnforcesCeilingThroughTheWaist(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	l := budget.NewLedger(s)
	if _, err := l.Open(ctx, "run", resource.Scope{}, budget.Limits{Tokens: 100}); err != nil {
		t.Fatal(err)
	}

	d := dispatch.New(dispatch.WithHook(budget.NewHook(s)))
	runCtx := budget.Into(ctx, "run")
	act := dispatch.Action{Name: "model.generate", Scope: state.Scope{}}
	spend60 := func(context.Context) (dispatch.Metering, error) {
		return dispatch.Metering{Tokens: 60}, nil
	}

	// Spend 0 -> 60: admitted.
	if err := d.Govern(runCtx, act, spend60); err != nil {
		t.Fatalf("first action (spend 0/100) must run: %v", err)
	}
	// Spend 60 -> 120: still admitted (60 < 100), settles past the cap.
	if err := d.Govern(runCtx, act, spend60); err != nil {
		t.Fatalf("second action (spend 60/100) must run: %v", err)
	}
	// Spend 120 >= 100: rejected before running.
	err := d.Govern(runCtx, act, spend60)
	if fault.Classify(err) != fault.BudgetExceeded {
		t.Fatalf("third action over budget = %v, want BudgetExceeded", err)
	}
	if got := spent(t, s, "run").Tokens; got != 120 {
		t.Fatalf("recorded spend = %d, want 120 (two charged, third rejected)", got)
	}
}
