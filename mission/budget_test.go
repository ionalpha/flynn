package mission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

// budgetStore returns an in-memory store with the Budget kind registered, so a run's
// spend pool can be opened and charged.
func budgetStore(t *testing.T) resource.Store {
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

// goalRes builds a Goal resource with the given name and budget pool (empty pool = a
// standalone root that is its own pool), the shape Executor.Execute decodes.
func goalRes(t *testing.T, name, pool string) resource.Resource {
	t.Helper()
	spec, err := json.Marshal(goal.Spec{Objective: "do the thing", StopCondition: "it is done", BudgetPool: pool})
	if err != nil {
		t.Fatal(err)
	}
	return resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: name, Spec: spec}
}

// TestExecutorEnforcesBudgetCeiling proves WithBudget wires the run's spend pool into
// the waist: once the pool is spent to its ceiling, the model call is refused before
// it runs (a BudgetExceeded fault), so an exhausted run cannot spend further.
func TestExecutorEnforcesBudgetCeiling(t *testing.T) {
	ctx := context.Background()
	store := budgetStore(t)
	led := budget.NewLedger(store)
	// Open a budget for the run (pool = the root goal's own name "g") and spend it past
	// the ceiling before the run makes a single call.
	if _, err := led.Open(ctx, "g", resource.Scope{}, budget.Limits{Cost: 1.0}); err != nil {
		t.Fatal(err)
	}
	if err := led.Charge(ctx, "g", resource.Scope{}, dispatch.Metering{Cost: 2.0}); err != nil {
		t.Fatal(err)
	}

	model := llmtest.NewScripted(llmtest.SayText("should never run"))
	exec := NewExecutor(model, WithBudget(budget.NewHook(store)))

	_, err := exec.Execute(ctx, goalRes(t, "g", ""))
	if fault.Classify(err) != fault.BudgetExceeded {
		t.Fatalf("exhausted run: err = %v, want BudgetExceeded", err)
	}
	if model.Calls() != 0 {
		t.Fatalf("model ran %d times on an exhausted budget; the ceiling must refuse it before it spends", model.Calls())
	}
}

// TestExecutorBudgetPoolIsSharedAcrossFanout proves a child charges the run's shared
// pool rather than a budget of its own: a child inherits the root's BudgetPool, so a
// pool already at its ceiling refuses the child's model call even though nothing is
// keyed to the child's own name. This is what bounds a whole fan-out by one ceiling.
func TestExecutorBudgetPoolIsSharedAcrossFanout(t *testing.T) {
	ctx := context.Background()
	store := budgetStore(t)
	led := budget.NewLedger(store)
	// One pool, keyed by the root, already exhausted; the child names its own goal id
	// but inherits "root" as its pool.
	if _, err := led.Open(ctx, "root", resource.Scope{}, budget.Limits{Tokens: 100}); err != nil {
		t.Fatal(err)
	}
	if err := led.Charge(ctx, "root", resource.Scope{}, dispatch.Metering{Tokens: 100}); err != nil {
		t.Fatal(err)
	}

	model := llmtest.NewScripted(llmtest.SayText("should never run"))
	exec := NewExecutor(model, WithBudget(budget.NewHook(store)))

	_, err := exec.Execute(ctx, goalRes(t, "child", "root"))
	if fault.Classify(err) != fault.BudgetExceeded {
		t.Fatalf("child on an exhausted shared pool: err = %v, want BudgetExceeded", err)
	}
	if model.Calls() != 0 {
		t.Fatalf("child ran %d model calls; the shared pool must refuse it before it spends", model.Calls())
	}
	// No budget was ever keyed to the child's own name: enforcement used the shared pool.
	if _, gerr := store.Get(ctx, budget.Kind, resource.Scope{}, "child"); gerr == nil {
		t.Fatal("a budget exists under the child's own name; the child must charge the shared pool, not its own")
	}
}

// TestExecutorAttributesSpendToModelTier proves the run's spend is booked to the tier
// it runs on: a goal carrying a model charges its pool under that model as the tier
// key, so the shared pool's per-tier ledger shows where the tokens went, not only a
// total. This is the per-tier ledger F15's savings are measured against.
func TestExecutorAttributesSpendToModelTier(t *testing.T) {
	ctx := context.Background()
	store := budgetStore(t)
	led := budget.NewLedger(store)
	if _, err := led.Open(ctx, "g", resource.Scope{}, budget.Limits{}); err != nil {
		t.Fatal(err)
	}

	// A model turn that reports real usage, so the waist charges a non-zero metering.
	turn := llmtest.SayText("done")
	turn.Usage = llm.Usage{InputTokens: 10, OutputTokens: 5}
	model := llmtest.NewScripted(turn)
	exec := NewExecutor(model, WithBudget(budget.NewHook(store)))

	spec, err := json.Marshal(goal.Spec{Objective: "o", StopCondition: "c", Model: "premium-model"})
	if err != nil {
		t.Fatal(err)
	}
	res := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: spec}
	if _, err := exec.Execute(ctx, res); err != nil {
		t.Fatalf("execute: %v", err)
	}

	st, err := led.Spend(ctx, "g", resource.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Spent.Tokens != 15 {
		t.Fatalf("aggregate spend = %d tokens, want 15", st.Spent.Tokens)
	}
	if got := st.ByTier["premium-model"]; got.Tokens != 15 {
		t.Fatalf("per-tier spend = %+v, want 15 tokens under premium-model (byTier=%+v)", got, st.ByTier)
	}
}

// TestExecutorUnbudgetedRunIsUnlimited proves the always-wired budget hook is inert
// when no budget resource is bound: a run whose pool has no budget spends freely, so
// wiring the hook does not change the zero-config posture.
func TestExecutorUnbudgetedRunIsUnlimited(t *testing.T) {
	ctx := context.Background()
	store := budgetStore(t)

	model := llmtest.NewScripted(llmtest.SayText("done"))
	exec := NewExecutor(model, WithBudget(budget.NewHook(store)))

	if _, err := exec.Execute(ctx, goalRes(t, "g", "")); err != nil {
		t.Fatalf("unbudgeted run must be unlimited, got error: %v", err)
	}
	if model.Calls() != 1 {
		t.Fatalf("unbudgeted run made %d model calls, want 1", model.Calls())
	}
}
