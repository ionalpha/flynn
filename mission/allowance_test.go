package mission

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ionalpha/flynn/allowance"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

// runWithAllowances drives one turn in which the model calls the echo tool, under a gate
// that marks that tool as reaching outside the workspace, and reports whether the tool
// actually ran and what the model was told.
func runWithAllowances(t *testing.T, marked string, declared []goal.Allowance) (int32, string) {
	t.Helper()
	var runs int32
	tool := Func(echoDef, func(_ context.Context, in json.RawMessage) (string, error) {
		atomic.AddInt32(&runs, 1)
		return string(in), nil
	})
	rec := &recordingReporter{}
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "echo", json.RawMessage(`{"x":1}`)),
		llmtest.SayText("done"),
	)
	exec := NewExecutor(model,
		WithTools(tool), WithObserver(rec), WithAllowance(allowance.NewActions(marked)))

	spec, err := json.Marshal(goal.Spec{
		Objective: "do the thing", StopCondition: "it is done", Allowances: declared,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Execute(context.Background(), resource.Resource{
		APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: spec,
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	res := firstOfKind(rec.events(), EventToolResult)
	if res == nil {
		t.Fatal("no tool result event reported")
	}
	return atomic.LoadInt32(&runs), res.Result
}

// TestAnUndeclaredIrreversibleActionDoesNotRun is the gate doing the only thing it exists
// for: the objective implied the action, nobody wrote it down, and it did not happen.
func TestAnUndeclaredIrreversibleActionDoesNotRun(t *testing.T) {
	runs, result := runWithAllowances(t, "echo", nil)
	if runs != 0 {
		t.Fatalf("the tool ran %d times without ever being declared", runs)
	}
	if !strings.Contains(result, "not declared in advance") {
		t.Errorf("result = %q, want it to say the action was never declared", result)
	}
}

// TestADeclaredIrreversibleActionRuns is the same run with the authority written down: the
// declaration travels on the goal, so one executor drives goals of differing authority here
// exactly as it does for the grant.
func TestADeclaredIrreversibleActionRuns(t *testing.T) {
	runs, result := runWithAllowances(t, "echo", []goal.Allowance{{Action: "echo"}})
	if runs != 1 {
		t.Fatalf("the tool ran %d times after being declared, want 1", runs)
	}
	if strings.Contains(result, "not declared") {
		t.Errorf("result = %q, want the tool's own output", result)
	}
}

// TestAnUnmarkedActionNeedsNoDeclaration is every action on every run that predates this:
// the gate is installed, the action is not marked, and nothing changes.
func TestAnUnmarkedActionNeedsNoDeclaration(t *testing.T) {
	runs, _ := runWithAllowances(t, "deploy", nil)
	if runs != 1 {
		t.Fatalf("an unmarked tool ran %d times, want 1", runs)
	}
}

// TestANilAllowancePolicyInstallsNoGate keeps the standalone agent zero-config: an option
// called with nothing to mark leaves the waist as it was.
func TestANilAllowancePolicyInstallsNoGate(t *testing.T) {
	var runs int32
	tool := Func(echoDef, func(_ context.Context, in json.RawMessage) (string, error) {
		atomic.AddInt32(&runs, 1)
		return string(in), nil
	})
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "echo", json.RawMessage(`{"x":1}`)),
		llmtest.SayText("done"),
	)
	exec := NewExecutor(model, WithTools(tool), WithAllowance(nil))
	driveToDone(t, exec, 5)
	if runs != 1 {
		t.Fatalf("the tool ran %d times with no policy wired, want 1", runs)
	}
}
