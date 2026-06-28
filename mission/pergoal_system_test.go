package mission

import (
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestPerGoalSystemPromptUsed proves a goal carrying its own system prompt runs
// under that prompt, so a delegated child can run as a different agent than its
// parent. The executor's default prompt is the fallback.
func TestPerGoalSystemPromptUsed(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("done"))
	exec := NewExecutor(model, WithSystem("default prompt"))
	driveSpec(t, exec, goal.Spec{Objective: "o", StopCondition: "done", System: "agent prompt"}, 3)

	reqs := model.Requests()
	if len(reqs) == 0 {
		t.Fatal("model was not called")
	}
	if reqs[0].System != "agent prompt" {
		t.Fatalf("request system = %q, want the goal's own prompt", reqs[0].System)
	}
}

func TestDefaultSystemPromptWhenGoalHasNone(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("done"))
	exec := NewExecutor(model, WithSystem("default prompt"))
	driveSpec(t, exec, goal.Spec{Objective: "o", StopCondition: "done"}, 3)

	reqs := model.Requests()
	if len(reqs) == 0 || reqs[0].System != "default prompt" {
		t.Fatalf("request system = %q, want the executor default", reqs[0].System)
	}
}
