package goal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
)

// fixedWindow is a WindowSource that reports a constant plan-window fraction, or an
// error, so a test controls exactly what share of the window the guard sees.
type fixedWindow struct {
	frac float64
	err  error
}

func (f fixedWindow) Fraction(context.Context) (float64, error) { return f.frac, f.err }

// newSpendHarness is newHarness with the Budget kind registered too, and it hands back
// a ledger over the same store so a test can record spend on a goal's pool before it
// reconciles. neverStop keeps the goal running so the reconcile always reaches the
// spend guard rather than settling as converged first.
func newSpendHarness(t *testing.T, opts ...Option) (*harness, *budget.Ledger) {
	t.Helper()
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	if err := budget.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg, resource.WithClock(m))
	q := jobs.NewMemory()
	gr := NewReconciler(store, q, m, stopAfter{at: 99}, opts...)
	h := &harness{ctx: context.Background(), store: store, jobs: q, gr: gr, clk: m}
	return h, budget.NewLedger(store)
}

// charge opens a budget for the pool and records a metering on it, so the goal whose
// pool is `pool` reads that spend at the guard.
func (h *harness) charge(t *testing.T, l *budget.Ledger, pool string, m dispatch.Metering) {
	t.Helper()
	if _, err := l.Open(h.ctx, pool, resource.Scope{}, budget.Limits{}); err != nil {
		t.Fatalf("open budget: %v", err)
	}
	if err := l.Charge(h.ctx, pool, resource.Scope{}, m); err != nil {
		t.Fatalf("charge budget: %v", err)
	}
}

// TestSpendGuardTokenCeilingStalls: a goal whose pool has spent its token ceiling
// stalls at the reconcile point with a reason that names what it spent against what it
// was allowed, and it stalls BEFORE dispatching another step (the ceiling is a stop,
// not a soft warning).
func TestSpendGuardTokenCeilingStalls(t *testing.T) {
	h, l := newSpendHarness(t)
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Budget: SpendBudget{Tokens: 100}})
	h.charge(t, l, "g", dispatch.Metering{Tokens: 100})

	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want Stalled", st.Phase)
	}
	if st.InFlight != nil {
		t.Fatal("a step was dispatched despite the spend ceiling being hit")
	}
	if !condReason(st, CondStalled, "SpendBudgetExhausted") {
		t.Fatalf("stalled condition reason wrong: %+v", st.Conditions)
	}
	if !strings.Contains(st.Message, "spent 100 of 100 allowed") {
		t.Fatalf("message does not name spent vs allowed: %q", st.Message)
	}
}

// TestSpendGuardCostCeilingStalls covers the cost axis independently of tokens.
func TestSpendGuardCostCeilingStalls(t *testing.T) {
	h, l := newSpendHarness(t)
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Budget: SpendBudget{Cost: 1.0}})
	h.charge(t, l, "g", dispatch.Metering{Cost: 1.5})

	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled || !condReason(st, CondStalled, "SpendBudgetExhausted") {
		t.Fatalf("did not stall on cost ceiling: %+v", st)
	}
	if !strings.Contains(st.Message, "cost budget exhausted") {
		t.Fatalf("message not the cost reason: %q", st.Message)
	}
}

// TestSpendGuardChargesSharedPool proves a goal that inherits a pool (a fan-out child)
// reads spend from BudgetPool, not from its own name: the whole graph is bounded by the
// one pool's total.
func TestSpendGuardChargesSharedPool(t *testing.T) {
	h, l := newSpendHarness(t)
	ref := h.createGoal(t, "child", Spec{Objective: "o", StopCondition: "c", BudgetPool: "root", Budget: SpendBudget{Tokens: 50}})
	h.charge(t, l, "root", dispatch.Metering{Tokens: 60})

	h.reconcile(t, ref)

	if st := h.status(t, ref); st.Phase != PhaseStalled {
		t.Fatalf("child did not stall on the shared pool's spend: %+v", st)
	}
}

// TestSpendGuardWindowCeilingStalls: with a window source wired, a goal stops when the
// plan window is at or past its share ceiling, under a distinct reason from the
// token/cost stop.
func TestSpendGuardWindowCeilingStalls(t *testing.T) {
	h, _ := newSpendHarness(t, WithWindowSource(fixedWindow{frac: 0.9}))
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Budget: SpendBudget{WindowFraction: 0.8}})

	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled || !condReason(st, CondStalled, "WindowBudgetExhausted") {
		t.Fatalf("did not stall on window share: %+v", st)
	}
	if !strings.Contains(st.Message, "90.0% of the window used") {
		t.Fatalf("message does not name the window share: %q", st.Message)
	}
}

// TestSpendGuardWindowUnboundedWithoutSource: a WindowFraction ceiling has no effect
// when no source is wired (Flynn ships none), so the goal runs normally rather than
// stalling or erroring on a bound it cannot evaluate.
func TestSpendGuardWindowUnboundedWithoutSource(t *testing.T) {
	h, _ := newSpendHarness(t) // no WithWindowSource
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Budget: SpendBudget{WindowFraction: 0.1}})

	h.reconcile(t, ref)

	if st := h.status(t, ref); st.Phase != PhaseRunning || st.InFlight == nil {
		t.Fatalf("window bound with no source should not stop the goal: %+v", st)
	}
}

// TestSpendGuardWindowSourceErrorPropagates: a window source that fails does not
// silently pass the guard; the error is returned so a transient source blip retries
// rather than being read as "within budget".
func TestSpendGuardWindowSourceErrorPropagates(t *testing.T) {
	srcErr := fault.New(fault.Transient, "window_unavailable", "window source down")
	h, _ := newSpendHarness(t, WithWindowSource(fixedWindow{err: srcErr}))
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Budget: SpendBudget{WindowFraction: 0.8}})

	_, err := h.gr.Reconcile(h.ctx, ref)
	if fault.Classify(err) != fault.Transient {
		t.Fatalf("window source error = %v, want a transient error surfaced", err)
	}
	if st := h.status(t, ref); st.Phase == PhaseStalled {
		t.Fatal("a failed window read must not settle the goal as stalled")
	}
}

// TestSpendGuardWithinBudgetDispatches: a goal under all its ceilings reconciles
// normally, dispatching its next step. This is the negative control for the guard.
func TestSpendGuardWithinBudgetDispatches(t *testing.T) {
	h, l := newSpendHarness(t, WithWindowSource(fixedWindow{frac: 0.2}))
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Budget: SpendBudget{Tokens: 1000, Cost: 10, WindowFraction: 0.8}})
	h.charge(t, l, "g", dispatch.Metering{Tokens: 100, Cost: 1})

	h.reconcile(t, ref)

	if st := h.status(t, ref); st.Phase != PhaseRunning || st.InFlight == nil {
		t.Fatalf("goal within budget should dispatch, not stall: %+v", st)
	}
}

// TestSpendGuardNoBudgetIsUnbounded: a goal that sets no budget never consults the
// ledger and never stalls on spend, even with plenty recorded on a pool of its name.
func TestSpendGuardNoBudgetIsUnbounded(t *testing.T) {
	h, l := newSpendHarness(t)
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"}) // zero Budget
	h.charge(t, l, "g", dispatch.Metering{Tokens: 1_000_000, Cost: 999})

	h.reconcile(t, ref)

	if st := h.status(t, ref); st.Phase != PhaseRunning || st.InFlight == nil {
		t.Fatalf("a goal with no budget must be unbounded by spend: %+v", st)
	}
}

// budgetGetFails wraps a store so reads of the Budget kind fail, leaving every other
// read intact. It isolates the spend guard's ledger read from the goal's own reads.
type budgetGetFails struct {
	resource.Store
	err error
}

func (s budgetGetFails) Get(ctx context.Context, kind string, scope resource.Scope, name string) (resource.Resource, error) {
	if kind == budget.Kind {
		return resource.Resource{}, s.err
	}
	return s.Store.Get(ctx, kind, scope, name)
}

// TestSpendGuardStoreErrorPropagates: a failed pool read is surfaced, not treated as
// "within budget". The reconcile returns the error so the controller retries rather
// than dispatching another step against a pool it could not read.
func TestSpendGuardStoreErrorPropagates(t *testing.T) {
	boom := fault.New(fault.Transient, "store_down", "store unavailable")
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	if err := budget.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := budgetGetFails{Store: resource.NewMemory(reg, resource.WithClock(m)), err: boom}
	gr := NewReconciler(store, jobs.NewMemory(), m, stopAfter{at: 99})
	h := &harness{ctx: context.Background(), store: store, jobs: jobs.NewMemory(), gr: gr, clk: m}
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Budget: SpendBudget{Tokens: 100}})

	if _, err := h.gr.Reconcile(h.ctx, ref); !errors.Is(err, boom) {
		t.Fatalf("reconcile error = %v, want the store failure surfaced", err)
	}
}

// condReason reports whether the named condition is present, True, and carries reason.
func condReason(st Status, typ, reason string) bool {
	for _, c := range st.Conditions {
		if c.Type == typ {
			return c.Status == "True" && c.Reason == reason
		}
	}
	return false
}
