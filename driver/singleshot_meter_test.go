package driver_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

// TestSingleShotMetersCacheWritesButNotCacheReadsTwice pins what the one model call
// charges the run's pool: input (cache reads are inside it), plus cache writes, plus
// output.
func TestSingleShotMetersCacheWritesButNotCacheReadsTwice(t *testing.T) {
	ctx := context.Background()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := budget.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg)
	led := budget.NewLedger(store)
	if _, err := led.Open(ctx, "g", resource.Scope{}, budget.Limits{}); err != nil {
		t.Fatal(err)
	}

	turn := llmtest.SayText("answer")
	turn.Usage = llm.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 60, CacheWriteTokens: 7}
	spec := baseSpec()
	spec.Model = llmtest.NewScripted(turn)
	spec.Budget = budget.NewHook(store)
	d, err := driver.Default().Resolve(driver.NameSingleShot)
	if err != nil {
		t.Fatal(err)
	}
	exec, _, err := d.Build(spec)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	raw, err := json.Marshal(goal.Spec{Objective: "o", StopCondition: "done", Driver: driver.NameSingleShot})
	if err != nil {
		t.Fatal(err)
	}
	res := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: raw}
	if _, err := exec.Execute(ctx, res); err != nil {
		t.Fatalf("execute: %v", err)
	}
	st, err := led.Spend(ctx, "g", resource.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Spent.Tokens != 127 {
		t.Fatalf("spend = %d tokens, want 127 (input 100 + cache writes 7 + output 20)", st.Spent.Tokens)
	}
}
