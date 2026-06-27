package mission

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestVerifyPassesDeferConvergence proves a verify pass does not take the model's first claim
// of completion at face value: the model says "done", but with one pass configured the run asks
// it to re-check (a second turn) before it converges.
func TestVerifyPassesDeferConvergence(t *testing.T) {
	model := llmtest.NewScripted(
		llmtest.SayText("first answer"),  // claims done; a verify pass should defer this
		llmtest.SayText("checked, done"), // re-check turn; passes spent, converges here
	)
	exec := NewExecutor(model, WithVerifyPasses(1))

	steps, cp, _ := driveToDone(t, exec, 5)
	if steps != 2 {
		t.Fatalf("converged in %d steps, want 2 (one extra verify turn)", steps)
	}
	if !cp.Done || cp.Result != "checked, done" {
		t.Fatalf("final checkpoint wrong: %+v", cp)
	}
	if cp.VerifyUsed != 1 {
		t.Fatalf("VerifyUsed = %d, want 1", cp.VerifyUsed)
	}
	// The second turn must have been prompted with the self-check.
	reqs := model.Requests()
	last := reqs[len(reqs)-1].Messages
	if !strings.Contains(last[len(last)-1].TextContent(), "re-check that the objective") {
		t.Fatalf("verify turn was not prompted with the self-check: %+v", last[len(last)-1])
	}
}

// TestVerifyPassesAllowModelRepair proves the verify pass is a real repair opportunity: prompted
// to re-check, the model uses a tool and only then concludes, and the tool call is honored.
func TestVerifyPassesAllowModelRepair(t *testing.T) {
	model := llmtest.NewScripted(
		llmtest.SayText("looks done"),                                // premature claim
		llmtest.CallTool("t1", "echo", json.RawMessage(`{"fix":1}`)), // verify pass repairs
		llmtest.SayText("now actually done"),                         // concludes
	)
	exec := NewExecutor(model, WithTools(echoTool()), WithVerifyPasses(1))

	steps, cp, _ := driveToDone(t, exec, 6)
	if steps != 3 {
		t.Fatalf("converged in %d steps, want 3", steps)
	}
	if cp.Result != "now actually done" {
		t.Fatalf("final result = %q", cp.Result)
	}
	if cp.VerifyUsed != 1 {
		t.Fatalf("VerifyUsed = %d, want 1", cp.VerifyUsed)
	}
}

// TestVerifyPassesBounded proves the budget is finite: with two passes and a model that keeps
// claiming completion, the run converges after exactly the two extra turns rather than looping.
func TestVerifyPassesBounded(t *testing.T) {
	model := llmtest.NewScripted(
		llmtest.SayText("done 1"),
		llmtest.SayText("done 2"),
		llmtest.SayText("done 3"),
	)
	exec := NewExecutor(model, WithVerifyPasses(2))

	steps, cp, _ := driveToDone(t, exec, 5)
	if steps != 3 {
		t.Fatalf("converged in %d steps, want 3 (initial + 2 verify)", steps)
	}
	if cp.VerifyUsed != 2 || cp.Result != "done 3" {
		t.Fatalf("checkpoint wrong: %+v", cp)
	}
}

// TestVerifyPassesDefaultOff proves the default trusts the first completion, so a reliable model
// is not made to pay for verification it does not need.
func TestVerifyPassesDefaultOff(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("done"))
	exec := NewExecutor(model)

	steps, cp, _ := driveToDone(t, exec, 3)
	if steps != 1 {
		t.Fatalf("converged in %d steps, want 1", steps)
	}
	if cp.VerifyUsed != 0 {
		t.Fatalf("VerifyUsed = %d, want 0", cp.VerifyUsed)
	}
}

// TestVerifyBudgetSurvivesResume proves the pass count is durable: a checkpoint that already
// spent its single pass converges immediately on the next claim of completion rather than
// granting a fresh pass, so a crash mid-run cannot multiply the budget.
func TestVerifyBudgetSurvivesResume(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("done"))
	exec := NewExecutor(model, WithVerifyPasses(1))

	// Resume from a checkpoint that has already used its one pass.
	cp := checkpoint{
		Messages:   []llm.Message{llm.Text(llm.RoleUser, "do the thing")},
		VerifyUsed: 1,
	}
	raw, err := encodeCheckpoint(cp)
	if err != nil {
		t.Fatal(err)
	}
	next, err := exec.Execute(t.Context(), res(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeCheckpoint(next)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Done {
		t.Fatalf("a run whose verify budget is spent must converge on completion, got %+v", got)
	}
}
