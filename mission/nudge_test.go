package mission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

// TestAppendNudgeRidesTheLastUserTurn: when the transcript ends on a user turn (the tool
// results being fed back), the nudge is folded into it as another text block rather than
// opening a second consecutive user message, which would break role alternation.
func TestAppendNudgeRidesTheLastUserTurn(t *testing.T) {
	msgs := []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Kind: llm.KindText, Text: "tool results"}}}}
	out := appendNudge(msgs, "you are stalling")
	if len(out) != 1 {
		t.Fatalf("opened a new message instead of riding the user turn: %d messages", len(out))
	}
	last := out[0].Blocks
	if last[len(last)-1].Text != "you are stalling" {
		t.Fatalf("nudge not folded into the user turn: %+v", last)
	}
}

// TestAppendNudgeOpensATurnAfterAnAssistantMessage: when the last message is the model's
// (no user turn to ride), the nudge opens its own user turn, preserving alternation.
func TestAppendNudgeOpensATurnAfterAnAssistantMessage(t *testing.T) {
	msgs := []llm.Message{llm.Text(llm.RoleAssistant, "I am done")}
	out := appendNudge(msgs, "you are stalling")
	if len(out) != 2 {
		t.Fatalf("did not open a user turn: %d messages", len(out))
	}
	if out[1].Role != llm.RoleUser || out[1].TextContent() != "you are stalling" {
		t.Fatalf("nudge turn wrong: %+v", out[1])
	}
}

// TestExecutorDeliversStallingNudge: a status carrying a ProgressNudge causes the next
// step to hand the model that warning inline, so a stalling goal is actually told it is
// stalling before it is stopped.
func TestExecutorDeliversStallingNudge(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("ok"))
	exec := NewExecutor(model)

	spec, err := json.Marshal(goal.Spec{Objective: "do the thing", StopCondition: "it is done"})
	if err != nil {
		t.Fatal(err)
	}
	status, err := goal.Status{ProgressNudge: "Idle streak: 2 steps without progress."}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	r := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: spec, Status: status}

	if _, err := exec.Execute(context.Background(), r); err != nil {
		t.Fatalf("execute: %v", err)
	}

	reqs := model.Requests()
	if len(reqs) == 0 {
		t.Fatal("model was not called")
	}
	if !requestHasText(reqs[0], "Idle streak: 2 steps without progress.") {
		t.Fatalf("the stalling nudge was not delivered to the model: %+v", reqs[0].Messages)
	}
}

// TestExecutorNoNudgeWhenMakingProgress: with no ProgressNudge on the status, no warning
// is injected — the model's turn carries only the objective.
func TestExecutorNoNudgeWhenMakingProgress(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("ok"))
	exec := NewExecutor(model)
	spec, err := json.Marshal(goal.Spec{Objective: "do the thing", StopCondition: "it is done"})
	if err != nil {
		t.Fatal(err)
	}
	r := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: spec}

	if _, err := exec.Execute(context.Background(), r); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if requestHasText(model.Requests()[0], "Idle streak") {
		t.Fatal("a stalling nudge was injected on a fresh, progressing goal")
	}
}

// requestHasText reports whether any message in the request carries a text block
// containing want.
func requestHasText(req llm.Request, want string) bool {
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Kind == llm.KindText && strings.Contains(b.Text, want) {
				return true
			}
		}
	}
	return false
}
