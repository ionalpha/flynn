package mission

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestPlanOptionsZeroPlanIsLean proves a strong model's zero plan adds no options, so the lean
// path is untouched.
func TestPlanOptionsZeroPlanIsLean(t *testing.T) {
	if opts := PlanOptions(harness.Plan{}); len(opts) != 0 {
		t.Fatalf("the zero plan must yield no options, got %d", len(opts))
	}
}

// TestPlanOptionsAppliesScaffolding proves each scaffolding field of a plan reaches the executor:
// simplified schemas and a tightened context budget show up in the request the model receives, and
// verify passes defer convergence.
func TestPlanOptionsAppliesScaffolding(t *testing.T) {
	plan := harness.Plan{
		SimplifyToolSchemas: true,
		VerifyPasses:        1,
		MaxContext:          8000,
	}
	def := llm.Tool{
		Name:        "echo",
		Description: "echo back the input",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string","description":"the message"}}}`),
	}
	tool := Func(def, echoTool().Invoke)
	model := llmtest.NewScripted(
		llmtest.SayText("done"),    // claims done; the one verify pass defers it
		llmtest.SayText("checked"), // converges
	)
	exec := NewExecutor(model, append([]Option{WithTools(tool)}, PlanOptions(plan)...)...)

	steps, cp, _ := driveToDone(t, exec, 5)
	if steps != 2 {
		t.Fatalf("verify pass did not defer convergence: converged in %d steps, want 2", steps)
	}
	if cp.VerifyUsed != 1 {
		t.Fatalf("VerifyUsed = %d, want 1", cp.VerifyUsed)
	}
	// Schema simplification reached the model.
	if reqs := model.Requests(); strings.Contains(string(reqs[0].Tools[0].InputSchema), "description") {
		t.Fatalf("schemas were not simplified: %s", reqs[0].Tools[0].InputSchema)
	}
}

// TestPlanOptionsContextBudgetReservesOutput proves the context cap is translated to an input
// budget that leaves room for the model's reply rather than filling the whole window.
func TestPlanOptionsContextBudgetReservesOutput(t *testing.T) {
	plan := harness.Plan{MaxContext: 8000}
	exec := NewExecutor(llmtest.NewScripted(llmtest.SayText("done")), PlanOptions(plan)...)
	want := 8000 * inputContextNumer / inputContextDenom
	if exec.compactBudget != want {
		t.Fatalf("compaction budget = %d, want %d (input share of the window)", exec.compactBudget, want)
	}
}
