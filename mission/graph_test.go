package mission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

// fakeFanout records spawned sub-goals and returns their results on demand, so a test can drive a
// parent through the spawn, wait, and fold phases under its own control. Children are "done" only
// once done is set, which lets a test assert the parent waits before folding.
type fakeFanout struct {
	spawned []SubGoal
	results map[string]ChildResult
	n       int
	done    bool
	fail    bool // make Spawn fail, to exercise the rejected-spawn path
}

func (f *fakeFanout) Spawn(_ context.Context, _ resource.Resource, sub SubGoal) (string, error) {
	if f.fail {
		return "", errors.New("spawner refused")
	}
	f.n++
	id := fmt.Sprintf("child-%d", f.n)
	f.spawned = append(f.spawned, sub)
	if f.results == nil {
		f.results = map[string]ChildResult{}
	}
	f.results[id] = ChildResult{ID: id, Result: "answer: " + sub.Objective}
	return id, nil
}

func (f *fakeFanout) Poll(_ context.Context, ids []string) ([]ChildResult, bool, error) {
	if !f.done {
		return nil, false, nil
	}
	out := make([]ChildResult, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.results[id])
	}
	return out, true, nil
}

// step runs one Execute against the checkpoint carried in prev and returns the next checkpoint, the
// shape the reconciler drives the goal through.
func step(t *testing.T, exec *Executor, prev json.RawMessage) (json.RawMessage, checkpoint) {
	t.Helper()
	next, err := exec.Execute(context.Background(), res(t, prev))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	cp, err := decodeCheckpoint(next)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return next, cp
}

func spawnCall(id, objective string) llm.Response {
	return llmtest.CallTool(id, ActionSpawn, json.RawMessage(`{"objective":"`+objective+`"}`))
}

// TestFanoutSpawnWaitFold proves the whole loop: the model spawns a child, the parent waits while
// the child runs, and once it finishes the parent folds its result in as the spawn call's result
// and converges on its own final answer.
func TestFanoutSpawnWaitFold(t *testing.T) {
	fan := &fakeFanout{}
	model := llmtest.NewScripted(
		spawnCall("s1", "research the topic"),
		llmtest.SayText("synthesized from the child"),
	)
	exec := NewExecutor(model, WithFanout(fan))

	// Turn 1: the model spawns a child; the parent records it and waits.
	raw, cp := step(t, exec, nil)
	if len(fan.spawned) != 1 || fan.spawned[0].Objective != "research the topic" {
		t.Fatalf("child not spawned: %+v", fan.spawned)
	}
	if len(cp.Pending) != 1 || cp.Pending[0].ChildID != "child-1" {
		t.Fatalf("parent should be waiting on child-1, got %+v", cp.Pending)
	}

	// While the child runs, the parent waits without calling the model again.
	raw, cp = step(t, exec, raw)
	if len(cp.Pending) != 1 {
		t.Fatalf("parent should still be waiting, got %+v", cp.Pending)
	}
	if model.Calls() != 1 {
		t.Fatalf("a waiting parent must not call the model, calls = %d", model.Calls())
	}

	// The child finishes; the next step folds its result in (clearing the wait), and the one after
	// lets the model conclude from it.
	fan.done = true
	raw, cp = step(t, exec, raw)
	if len(cp.Pending) != 0 {
		t.Fatalf("fold should clear the wait, got %+v", cp.Pending)
	}
	last := cp.Messages[len(cp.Messages)-1]
	if last.Role != llm.RoleUser || last.Blocks[0].ToolResult == nil ||
		!strings.Contains(last.Blocks[0].ToolResult.Content, "answer: research the topic") {
		t.Fatalf("child result not folded as the spawn result: %+v", last)
	}

	_, cp = step(t, exec, raw)
	if !cp.Done || cp.Result != "synthesized from the child" {
		t.Fatalf("parent did not converge from the folded result: %+v", cp)
	}
}

// TestFanoutDisabledByDefault proves a goal without a Fanout is a single conversation: the spawn
// tool is not even offered to the model.
func TestFanoutDisabledByDefault(t *testing.T) {
	exec := NewExecutor(llmtest.NewScripted(llmtest.SayText("done")))
	for _, d := range exec.defs {
		if d.Name == ActionSpawn {
			t.Fatal("spawn tool offered without WithFanout")
		}
	}
	execF := NewExecutor(llmtest.NewScripted(llmtest.SayText("done")), WithFanout(&fakeFanout{}))
	found := false
	for _, d := range execF.defs {
		if d.Name == ActionSpawn {
			found = true
		}
	}
	if !found {
		t.Fatal("spawn tool not offered with WithFanout")
	}
}

// TestFanoutTwoChildrenOrdered proves several children spawned in one turn are all waited on and
// folded back in the order the model issued the calls, alongside a normal tool result.
func TestFanoutTwoChildrenOrdered(t *testing.T) {
	fan := &fakeFanout{}
	mixed := llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "a", Name: ActionSpawn, Input: json.RawMessage(`{"objective":"first"}`)}},
			{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "b", Name: "echo", Input: json.RawMessage(`{"x":1}`)}},
			{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "c", Name: ActionSpawn, Input: json.RawMessage(`{"objective":"second"}`)}},
		}},
		StopReason: llm.StopToolUse,
	}
	model := llmtest.NewScripted(mixed, llmtest.SayText("done"))
	exec := NewExecutor(model, WithTools(echoTool()), WithFanout(fan))

	raw, cp := step(t, exec, nil)
	if len(cp.Pending) != 3 {
		t.Fatalf("all three calls should be pending slots, got %+v", cp.Pending)
	}
	fan.done = true
	_, cp = step(t, exec, raw)
	blocks := cp.Messages[len(cp.Messages)-1].Blocks
	if len(blocks) != 3 {
		t.Fatalf("folded message should carry three results, got %d", len(blocks))
	}
	// Order preserved: spawn(first), echo, spawn(second).
	if !strings.Contains(blocks[0].ToolResult.Content, "answer: first") ||
		blocks[1].ToolResult.ToolUseID != "b" ||
		!strings.Contains(blocks[2].ToolResult.Content, "answer: second") {
		t.Fatalf("results out of order: %+v", blocks)
	}
}

// TestFanoutSpawnRejectedIsErrorResult proves a spawn the spawner refuses becomes an error result
// the model can react to, and does not leave the parent waiting on a child that never started.
func TestFanoutSpawnRejectedIsErrorResult(t *testing.T) {
	fan := &fakeFanout{fail: true}
	model := llmtest.NewScripted(spawnCall("s1", "do a thing"), llmtest.SayText("handled the failure"))
	exec := NewExecutor(model, WithFanout(fan))

	raw, cp := step(t, exec, nil)
	if len(cp.Pending) != 0 {
		t.Fatalf("a rejected spawn must not enter a wait, got %+v", cp.Pending)
	}
	last := cp.Messages[len(cp.Messages)-1]
	if last.Blocks[0].ToolResult == nil || !last.Blocks[0].ToolResult.IsError {
		t.Fatalf("rejected spawn should be an error result: %+v", last)
	}
	_, cp = step(t, exec, raw)
	if !cp.Done {
		t.Fatalf("model should continue after a rejected spawn, got %+v", cp)
	}
}

// TestFanoutGrantGatesSpawn proves spawning is governed: a run whose grant does not permit the
// spawn action cannot fan out, and the attempt comes back as an error result rather than a child.
func TestFanoutGrantGatesSpawn(t *testing.T) {
	fan := &fakeFanout{}
	model := llmtest.NewScripted(spawnCall("s1", "delegate"), llmtest.SayText("ok"))
	// A grant that allows the model call but not spawn.
	exec := NewExecutor(model, WithFanout(fan), WithGrant(capability.NewGrant(ActionModelGenerate)))

	_, cp := step(t, exec, nil)
	if len(fan.spawned) != 0 {
		t.Fatalf("an ungranted spawn must not reach the spawner, got %+v", fan.spawned)
	}
	if len(cp.Pending) != 0 || cp.Messages[len(cp.Messages)-1].Blocks[0].ToolResult == nil ||
		!cp.Messages[len(cp.Messages)-1].Blocks[0].ToolResult.IsError {
		t.Fatalf("ungranted spawn should be an error result, got %+v", cp)
	}
}

// TestFanoutWaitSurvivesResume proves the wait is durable: a checkpoint persisted mid-fan-out
// decodes back to a waiting parent, so a crash resumes waiting instead of re-spawning.
func TestFanoutWaitSurvivesResume(t *testing.T) {
	fan := &fakeFanout{}
	model := llmtest.NewScripted(spawnCall("s1", "work"), llmtest.SayText("done"))
	exec := NewExecutor(model, WithFanout(fan))

	raw, _ := step(t, exec, nil) // spawns, now waiting
	// Re-decode the persisted checkpoint, as a resumed worker would.
	cp, err := decodeCheckpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.Pending) != 1 {
		t.Fatalf("resumed checkpoint lost the wait: %+v", cp)
	}
	// Resuming and polling (children still running) must not spawn again.
	_, _ = step(t, exec, raw)
	if len(fan.spawned) != 1 {
		t.Fatalf("resume re-spawned the child: %+v", fan.spawned)
	}
}
