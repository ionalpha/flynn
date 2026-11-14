package goal

import (
	"context"
	"errors"
	"sync"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

const budgetFault = "budget_exceeded: run budget exhausted for action model.call"

// extendRecorder keeps what Extend recorded.
type extendRecorder struct {
	mu  sync.Mutex
	got []budget.Extended
}

func (r *extendRecorder) RecordExtended(_ context.Context, e budget.Extended) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, e)
	return nil
}

func (r *extendRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// extendRig is a goal over a real reconciler, the budget ledger it shares a store with,
// and the pieces Extend needs.
type extendRig struct {
	h      *harness
	ledger *budget.Ledger
	ref    reconcile.Ref
	rec    *extendRecorder
}

// newExtendRig opens a ledger ceiling for the goal's own pool and creates the goal, so a
// run's two ceilings start equal, as a run assembled by a host has them.
func newExtendRig(t *testing.T, spec Spec, ceiling, limit budget.Limits) *extendRig {
	t.Helper()
	h, base := newSpendHarness(t, WithStepMaxAttempts(1))
	if _, err := base.Open(h.ctx, "g", resource.Scope{}, ceiling); err != nil {
		t.Fatalf("open budget: %v", err)
	}
	spec.Objective, spec.StopCondition = "o", "c"
	return &extendRig{
		h: h, ledger: base.WithExtensionCap(limit), rec: &extendRecorder{},
		ref: h.createGoal(t, "g", spec),
	}
}

// dispatch runs the reconciles that put a step in flight: the first installs the
// finalizer.
func (x *extendRig) dispatch(t *testing.T) {
	t.Helper()
	x.h.reconcile(t, x.ref)
	x.h.reconcile(t, x.ref)
	if st := x.h.status(t, x.ref); st.InFlight == nil {
		t.Fatalf("no step in flight: %+v", st)
	}
}

// stallOnLedger dispatches a step and kills it with the fault the dispatch waist raises
// when the run's ledger ceiling is spent, so the goal stops on the ledger's ceiling and
// not its own.
func (x *extendRig) stallOnLedger(t *testing.T) {
	t.Helper()
	x.dispatch(t)
	x.h.dieWith(t, budgetFault)
	x.h.reconcile(t, x.ref)
	if st := x.h.status(t, x.ref); !StoppedOnBudget(st) {
		t.Fatalf("goal did not stop on the ledger's ceiling: %+v", st)
	}
}

func (x *extendRig) extend(to budget.Limits) (budget.Extended, error) {
	return x.ledger.Extend(x.h.ctx, "g", resource.Scope{}, budget.Extension{
		To: to, Principal: "ada", Reason: "worth finishing", Record: x.rec,
		Goal: NewBudgetReopener(x.h.store, resource.Scope{}, "g", x.h.clk),
	})
}

func (x *extendRig) runCeiling(t *testing.T) budget.Limits {
	t.Helper()
	r, err := x.h.store.Get(x.h.ctx, budget.Kind, resource.Scope{}, "g")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := budget.DecodeSpec(r)
	if err != nil {
		t.Fatal(err)
	}
	return spec.Limits
}

func (x *extendRig) goalSpec(t *testing.T) Spec {
	t.Helper()
	r, err := x.h.store.Get(x.h.ctx, Kind, resource.Scope{}, "g")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := DecodeSpec(r)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// spendToCeiling charges the pool up to the run's ceiling, which is what the spend stop
// reads.
func (x *extendRig) spendToCeiling(t *testing.T) {
	t.Helper()
	spent, err := x.ledger.Spend(x.h.ctx, "g", resource.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	c := x.runCeiling(t)
	if err := x.ledger.Charge(x.h.ctx, "g", resource.Scope{}, dispatch.Metering{Tokens: int(c.Tokens - spent.Spent.Tokens)}); err != nil {
		t.Fatal(err)
	}
}

// dieWith claims the dispatched step and fails it until it is dead, with msg as its error.
func (h *harness) dieWith(t *testing.T, msg string) {
	t.Helper()
	claimed, err := h.jobs.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 1, LeaseFor: int64(1e9)})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("no step job to claim (err=%v)", err)
	}
	if err := h.jobs.Fail(h.ctx, claimed[0].ID, msg, h.clk.Now().UnixNano()); err != nil {
		t.Fatalf("fail step: %v", err)
	}
}

func TestStoppedOnBudget(t *testing.T) {
	stalled := func(reason, message string) Status {
		var s Status
		s.stall(reason, message, testNow)
		return s
	}
	tests := []struct {
		name string
		s    Status
		want bool
	}{
		{"spend ceiling", stalled("SpendBudgetExhausted", "token budget exhausted"), true},
		{"ledger ceiling at the waist", stalled("StepFailed", "step failed: "+budgetFault), true},
		{"step failed otherwise", stalled("StepFailed", "step failed: boom"), false},
		{"step budget", stalled("BudgetExhausted", "step budget exhausted"), false},
		{"plan window", stalled("WindowBudgetExhausted", "window"), false},
		{"killed", stalled(KilledReason, "stopped by the operator"), false},
		{"running", Status{Phase: PhaseRunning}, false},
		{"converged", Status{Phase: PhaseConverged}, false},
		{"zero", Status{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StoppedOnBudget(tc.s); got != tc.want {
				t.Errorf("StoppedOnBudget = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExtendReopensAGoalThatHitItsSpendCeiling(t *testing.T) {
	x := newExtendRig(t, Spec{Budget: SpendBudget{Tokens: 100}}, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 1_000})
	x.spendToCeiling(t)
	x.h.reconcile(t, x.ref)
	if st := x.h.status(t, x.ref); !StoppedOnBudget(st) {
		t.Fatalf("goal did not stop on its budget: %+v", st)
	}

	got, err := x.extend(budget.Limits{Tokens: 400})
	if err != nil {
		t.Fatal(err)
	}
	if got.To.Tokens != 400 || x.rec.count() != 1 {
		t.Fatalf("extended %+v, recorded %d", got, x.rec.count())
	}
	if c := x.runCeiling(t); c.Tokens != 400 {
		t.Errorf("run ceiling = %+v, want 400", c)
	}
	if b := x.goalSpec(t).Budget; b.Tokens != 400 {
		t.Errorf("goal ceiling = %+v, want it moved with the run's", b)
	}
	st := x.h.status(t, x.ref)
	if st.Phase != PhaseRunning || hasCond(st, CondStalled, "True") {
		t.Fatalf("goal not released: %+v", st)
	}
	answered := false
	for _, c := range st.Conditions {
		answered = answered || (c.Type == CondStalled && c.Status == "False" && c.Reason == BudgetExtendedReason)
	}
	if !answered {
		t.Errorf("the stall should read as answered: %+v", st.Conditions)
	}

	// The next pass works again instead of skipping the goal as settled.
	x.h.reconcile(t, x.ref)
	if st := x.h.status(t, x.ref); st.InFlight == nil || st.Phase != PhaseRunning {
		t.Fatalf("reopened goal did not dispatch a step: %+v", st)
	}
}

// A run whose goal sets no ceiling of its own stops at the dispatch waist on the
// ledger's. The goal spec hash does not move for it, so only the status change reopens
// it; and afterwards both ceilings are the same number.
func TestExtendReopensAGoalStoppedByTheLedgerCeiling(t *testing.T) {
	x := newExtendRig(t, Spec{}, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 1_000})
	x.stallOnLedger(t)
	st := x.h.status(t, x.ref)
	if !StoppedOnBudget(st) {
		t.Fatalf("goal did not stop on the ledger's ceiling: %+v", st)
	}
	if _, err := x.extend(budget.Limits{Tokens: 250}); err != nil {
		t.Fatal(err)
	}
	if b := x.goalSpec(t).Budget; b.Tokens != 250 {
		t.Errorf("goal ceiling = %+v, want the run's 250", b)
	}
	x.h.reconcile(t, x.ref)
	if st := x.h.status(t, x.ref); st.InFlight == nil {
		t.Fatalf("goal stayed settled after the extension: %+v", st)
	}
}

// A goal whose own ceiling is already above the run's keeps it: an extension never
// lowers a ceiling, on either side.
func TestExtendNeverLowersTheGoalsOwnCeiling(t *testing.T) {
	x := newExtendRig(t, Spec{Budget: SpendBudget{Tokens: 900}}, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 1_000})
	x.stallOnLedger(t)
	if _, err := x.extend(budget.Limits{Tokens: 200}); err != nil {
		t.Fatal(err)
	}
	if b := x.goalSpec(t).Budget; b.Tokens != 900 {
		t.Errorf("goal ceiling = %+v, want 900 kept", b)
	}
}

func TestExtendRefusalsLeaveBothCeilingsAndTheGoalAlone(t *testing.T) {
	stalledOnBudget := func(t *testing.T, x *extendRig) {
		x.stallOnLedger(t)
	}
	tests := []struct {
		name  string
		spec  Spec
		setup func(t *testing.T, x *extendRig)
		want  error
	}{
		{"still running", Spec{}, func(t *testing.T, x *extendRig) { x.dispatch(t) }, budget.ErrNotBudgetStop},
		{"failed for another reason", Spec{}, func(t *testing.T, x *extendRig) {
			x.dispatch(t)
			x.h.dieWith(t, "boom")
			x.h.reconcile(t, x.ref)
			if st := x.h.status(t, x.ref); st.Phase != PhaseStalled {
				t.Fatalf("setup: goal did not stall: %+v", st)
			}
		}, budget.ErrNotBudgetStop},
		{"step budget spent", Spec{MaxSteps: 1}, func(t *testing.T, x *extendRig) {
			x.dispatch(t)
			x.h.completeStep(t)
			x.h.reconcile(t, x.ref)
			if st := x.h.status(t, x.ref); !condReason(st, CondStalled, "BudgetExhausted") {
				t.Fatalf("setup: goal did not stop on its step budget: %+v", st)
			}
		}, budget.ErrNotBudgetStop},
		{"killed after the budget stop", Spec{}, func(t *testing.T, x *extendRig) {
			stalledOnBudget(t, x)
			x.h.putSpec(t, x.ref, Spec{Objective: "o", StopCondition: "c", Kill: &Kill{Reason: "no"}})
		}, budget.ErrKilled},
		{"killed and settled", Spec{}, func(t *testing.T, x *extendRig) {
			x.h.reconcile(t, x.ref)
			x.h.putSpec(t, x.ref, Spec{Objective: "o", StopCondition: "c", Kill: &Kill{Reason: "no"}})
			x.h.reconcile(t, x.ref)
		}, budget.ErrKilled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x := newExtendRig(t, tc.spec, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 1_000})
			tc.setup(t, x)
			before := x.h.status(t, x.ref)
			specBefore := x.goalSpec(t)

			_, err := x.extend(budget.Limits{Tokens: 500})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if c := x.runCeiling(t); c.Tokens != 100 {
				t.Errorf("run ceiling = %+v after a refusal", c)
			}
			if x.goalSpec(t).Budget != specBefore.Budget {
				t.Errorf("goal ceiling moved on a refusal")
			}
			after := x.h.status(t, x.ref)
			if after.Phase != before.Phase || len(after.Conditions) != len(before.Conditions) {
				t.Errorf("goal status changed on a refusal: %+v -> %+v", before, after)
			}
			if x.rec.count() != 0 {
				t.Errorf("a refusal was recorded")
			}
		})
	}
}

// A kill that lands between admission and the reopen write is caught by the write's own
// check: the run stays killed even though the ceiling had already been raised, which is
// the safe direction.
func TestReopenRechecksAgainstTheRecordItWrites(t *testing.T) {
	x := newExtendRig(t, Spec{}, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 1_000})
	x.stallOnLedger(t)

	re := NewBudgetReopener(x.h.store, resource.Scope{}, "g", x.h.clk)
	if err := re.Admit(x.h.ctx); err != nil {
		t.Fatal(err)
	}
	x.h.putSpec(t, x.ref, Spec{Objective: "o", StopCondition: "c", Kill: &Kill{}})
	if err := re.Reopen(x.h.ctx, budget.Limits{Tokens: 500}); !errors.Is(err, budget.ErrKilled) {
		t.Fatalf("Reopen = %v, want ErrKilled", err)
	}
	if st := x.h.status(t, x.ref); st.Phase != PhaseStalled {
		t.Fatalf("a killed goal was reopened: %+v", st)
	}
}

// An extension arriving as the reconciler is stalling the goal either finds it already
// stopped and reopens it, or finds it still running and is refused with nothing
// changed. It never raises a ceiling for a goal it then fails to reopen, never leaves
// the two ceilings apart, and never loses the stall to the extension's write.
func TestExtendRacingTheStallIsAllOrNothing(t *testing.T) {
	for i := range 60 {
		x := newExtendRig(t, Spec{}, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 1_000})
		x.dispatch(t)
		x.h.dieWith(t, budgetFault)

		var wg sync.WaitGroup
		var extendErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, err := x.h.gr.Reconcile(x.h.ctx, x.ref); err != nil {
				t.Errorf("reconcile: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			_, extendErr = x.extend(budget.Limits{Tokens: 300})
		}()
		close(start)
		wg.Wait()

		switch {
		case extendErr == nil:
			if c := x.runCeiling(t); c.Tokens != 300 {
				t.Fatalf("iteration %d: extension succeeded but run ceiling = %+v", i, c)
			}
			if b := x.goalSpec(t).Budget; b.Tokens != 300 {
				t.Fatalf("iteration %d: extension succeeded but goal ceiling = %+v", i, b)
			}
			if x.rec.count() != 1 {
				t.Fatalf("iteration %d: recorded %d extensions", i, x.rec.count())
			}
			if st := x.h.status(t, x.ref); StoppedOnBudget(st) {
				t.Fatalf("iteration %d: extension succeeded and the goal is still stopped: %+v", i, st)
			}
		case errors.Is(extendErr, budget.ErrNotBudgetStop):
			if c := x.runCeiling(t); c.Tokens != 100 {
				t.Fatalf("iteration %d: refused but run ceiling = %+v", i, c)
			}
			if x.rec.count() != 0 {
				t.Fatalf("iteration %d: refused but recorded", i)
			}
			// The stall has landed by now; asking again succeeds.
			x.h.reconcile(t, x.ref)
			if _, err := x.extend(budget.Limits{Tokens: 300}); err != nil {
				t.Fatalf("iteration %d: second ask: %v", i, err)
			}
		default:
			t.Fatalf("iteration %d: extend = %v, want success or ErrNotBudgetStop", i, extendErr)
		}
	}
}

// Two people extending the same stopped run at once: exactly the caps' worth is
// granted and the goal ends up with the run's ceiling.
func TestConcurrentExtendsKeepTheTwoCeilingsEqual(t *testing.T) {
	x := newExtendRig(t, Spec{Budget: SpendBudget{Tokens: 100}}, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 150})
	x.spendToCeiling(t)
	x.h.reconcile(t, x.ref)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = x.extend(budget.Limits{Tokens: int64(120 + 10*i)})
		}()
	}
	wg.Wait()

	run, spec := x.runCeiling(t), x.goalSpec(t).Budget
	if run.Tokens-100 > 150 {
		t.Fatalf("run ceiling %d is past the cap", run.Tokens)
	}
	if spec.Tokens != run.Tokens {
		t.Fatalf("goal ceiling %d, run ceiling %d: the two disagree", spec.Tokens, run.Tokens)
	}
}

// After any sequence of extends, successful or refused, the goal's ceiling equals the
// run's and neither is ever lower than it was.
func TestExtendKeepsGoalAndRunCeilingsEqualAndNeverLowerProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		start := rapid.Int64Range(10, 500).Draw(rt, "start")
		limit := rapid.Int64Range(1, 800).Draw(rt, "cap")
		x := newExtendRig(t, Spec{Budget: SpendBudget{Tokens: start}}, budget.Limits{Tokens: start}, budget.Limits{Tokens: limit})
		x.spendToCeiling(t)
		x.h.reconcile(t, x.ref)

		rounds := rapid.IntRange(1, 6).Draw(rt, "rounds")
		for range rounds {
			before := x.runCeiling(t)
			goalBefore := x.goalSpec(t).Budget
			stateBefore := x.h.status(t, x.ref)
			to := rapid.Int64Range(0, before.Tokens+limit+50).Draw(rt, "to")

			_, err := x.extend(budget.Limits{Tokens: to})
			run, goalNow := x.runCeiling(t), x.goalSpec(t).Budget
			if run.Tokens < before.Tokens || goalNow.Tokens < goalBefore.Tokens {
				rt.Fatalf("a ceiling fell: run %d -> %d, goal %d -> %d", before.Tokens, run.Tokens, goalBefore.Tokens, goalNow.Tokens)
			}
			if run.Tokens-start > limit {
				rt.Fatalf("run ceiling %d is %d past its start, cap %d", run.Tokens, run.Tokens-start, limit)
			}
			if err != nil {
				if run != before || goalNow != goalBefore {
					rt.Fatalf("extend failed (%v) and still changed a ceiling", err)
				}
				if got := x.h.status(t, x.ref); got.Phase != stateBefore.Phase {
					rt.Fatalf("extend failed (%v) and still moved the goal", err)
				}
				continue
			}
			if run.Tokens != goalNow.Tokens {
				rt.Fatalf("run ceiling %d, goal ceiling %d after a successful extend", run.Tokens, goalNow.Tokens)
			}
			// Stall it again so the next round has something to answer.
			x.spendToCeiling(t)
			x.h.reconcile(t, x.ref) // dispatches or stalls, depending on the spend guard
			x.h.reconcile(t, x.ref)
			if st := x.h.status(t, x.ref); !StoppedOnBudget(st) {
				if st.InFlight != nil {
					x.h.completeStep(t)
				}
				x.h.reconcile(t, x.ref)
			}
		}
	})
}
