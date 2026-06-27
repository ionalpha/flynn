package mission

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

var echoDef = llm.Tool{
	Name:        "echo",
	Description: "echo back the input",
	InputSchema: json.RawMessage(`{"type":"object"}`),
}

func echoTool() Tool {
	return Func(echoDef, func(_ context.Context, input json.RawMessage) (string, error) {
		return string(input), nil
	})
}

// res builds a Goal resource carrying the suite's spec and the given checkpoint as
// status, the shape Executor.Execute decodes.
func res(t *testing.T, cp json.RawMessage) resource.Resource {
	t.Helper()
	spec, err := json.Marshal(goal.Spec{Objective: "do the thing", StopCondition: "it is done"})
	if err != nil {
		t.Fatal(err)
	}
	r := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: spec}
	if len(cp) > 0 {
		status, err := goal.Status{Checkpoint: cp}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		r.Status = status
	}
	return r
}

// driveToDone runs the executor step by step, feeding each step the checkpoint the
// previous one persisted (so it exercises crash-resume: every step re-decodes from
// serialized state), until Convergence reports done.
func driveToDone(t *testing.T, exec *Executor, maxSteps int) (steps int, cp checkpoint, raw json.RawMessage) {
	t.Helper()
	spec := goal.Spec{Objective: "do the thing", StopCondition: "it is done"}
	var prev json.RawMessage
	for steps = 1; steps <= maxSteps; steps++ {
		next, err := exec.Execute(context.Background(), res(t, prev))
		if err != nil {
			t.Fatalf("step %d: %v", steps, err)
		}
		prev = next
		met, _, err := Convergence{}.Met(context.Background(), spec, goal.Status{Checkpoint: next})
		if err != nil {
			t.Fatal(err)
		}
		if met {
			dec, err := decodeCheckpoint(next)
			if err != nil {
				t.Fatal(err)
			}
			return steps, dec, next
		}
	}
	t.Fatalf("did not converge within %d steps", maxSteps)
	return 0, checkpoint{}, nil
}

// TestExecutorDrivesToolThenText is the core loop: the model calls a tool, the
// executor runs it and feeds the result back, and the next turn ends the mission.
func TestExecutorPinsSamplingWhenConfigured(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("done"))
	want := &llm.Sampling{Seed: 99, Temperature: 0, TopP: 0.9}
	exec := NewExecutor(model, WithSampling(want))

	driveToDone(t, exec, 3)

	reqs := model.Requests()
	if len(reqs) == 0 || reqs[0].Sampling == nil {
		t.Fatalf("the model call did not carry pinned sampling: %+v", reqs)
	}
	if *reqs[0].Sampling != *want {
		t.Fatalf("sampling = %+v, want %+v", reqs[0].Sampling, want)
	}
}

func TestExecutorFreeRunningByDefault(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("done"))
	exec := NewExecutor(model)

	driveToDone(t, exec, 3)

	if reqs := model.Requests(); len(reqs) == 0 || reqs[0].Sampling != nil {
		t.Fatalf("a run without WithSampling must be free-running (nil sampling), got %+v", reqs)
	}
}

func TestExecutorDrivesToolThenText(t *testing.T) {
	model := llmtest.NewScripted(
		llmtest.CallTool("t1", "echo", json.RawMessage(`{"msg":"hi"}`)),
		llmtest.SayText("all done"),
	)
	exec := NewExecutor(model, WithTools(echoTool()), WithSystem("be brief"))

	steps, cp, _ := driveToDone(t, exec, 5)
	if steps != 2 {
		t.Fatalf("converged in %d steps, want 2", steps)
	}
	if !cp.Done || cp.Result != "all done" {
		t.Fatalf("final checkpoint wrong: %+v", cp)
	}
	if model.Calls() != 2 {
		t.Fatalf("model called %d times, want 2", model.Calls())
	}

	// The second request must carry the prior turn AND the tool result the loop fed
	// back, so the model actually saw the tool's output.
	reqs := model.Requests()
	last := reqs[len(reqs)-1].Messages
	if last[0].Role != llm.RoleUser || !strings.Contains(last[0].TextContent(), "do the thing") {
		t.Fatalf("conversation did not open with the goal prompt: %+v", last[0])
	}
	foundResult := false
	for _, m := range last {
		for _, b := range m.Blocks {
			if b.Kind == llm.KindToolResult && strings.Contains(b.ToolResult.Content, `"msg":"hi"`) {
				foundResult = true
			}
		}
	}
	if !foundResult {
		t.Fatal("the tool result was not fed back into the conversation")
	}
	// The system prompt rides on every request.
	if reqs[0].System != "be brief" {
		t.Fatalf("system prompt not sent: %q", reqs[0].System)
	}
}

// TestExecutorTextOnlyConvergesInOneStep: a goal the model answers without tools
// finishes in a single turn.
func TestExecutorTextOnlyConvergesInOneStep(t *testing.T) {
	exec := NewExecutor(llmtest.NewScripted(llmtest.SayText("answer")))
	steps, cp, _ := driveToDone(t, exec, 5)
	if steps != 1 || cp.Result != "answer" {
		t.Fatalf("got steps=%d result=%q, want 1/answer", steps, cp.Result)
	}
}

// TestExecutorToolErrorRecovers: a tool that fails becomes an error result the
// model can react to; the step itself does not fail.
func TestExecutorToolErrorRecovers(t *testing.T) {
	boom := Func(llm.Tool{Name: "boom", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, json.RawMessage) (string, error) { return "", errors.New("kaboom") })
	model := llmtest.NewScripted(
		llmtest.CallTool("t1", "boom", json.RawMessage(`{}`)),
		llmtest.SayText("recovered"),
	)
	exec := NewExecutor(model, WithTools(boom))

	steps, cp, _ := driveToDone(t, exec, 5)
	if steps != 2 || cp.Result != "recovered" {
		t.Fatalf("got steps=%d result=%q, want 2/recovered", steps, cp.Result)
	}
	// The failed call must reach the model as an error result, not a dropped turn.
	last := model.Requests()[1].Messages
	sawErr := false
	for _, m := range last {
		for _, b := range m.Blocks {
			if b.Kind == llm.KindToolResult && b.ToolResult.IsError && strings.Contains(b.ToolResult.Content, "kaboom") {
				sawErr = true
			}
		}
	}
	if !sawErr {
		t.Fatal("tool error was not reported back to the model")
	}
}

// TestExecutorUnknownTool: a call to a tool that was never registered surfaces as
// an error result rather than crashing the step.
func TestExecutorUnknownTool(t *testing.T) {
	model := llmtest.NewScripted(
		llmtest.CallTool("t1", "ghost", json.RawMessage(`{}`)),
		llmtest.SayText("ok"),
	)
	exec := NewExecutor(model) // no tools registered
	steps, _, _ := driveToDone(t, exec, 5)
	if steps != 2 {
		t.Fatalf("converged in %d steps, want 2", steps)
	}
	res := model.Requests()[1].Messages[2] // user turn carrying the tool result
	if !res.Blocks[0].ToolResult.IsError || !strings.Contains(res.Blocks[0].ToolResult.Content, "unknown tool") {
		t.Fatalf("unknown tool not reported as error: %+v", res.Blocks[0].ToolResult)
	}
}

// TestExecutorContinuesOnTruncation: a turn cut off at the token ceiling does not
// converge; the loop asks the model to continue and finishes on the next turn.
func TestExecutorContinuesOnTruncation(t *testing.T) {
	truncated := llm.Response{
		Message:    llm.Text(llm.RoleAssistant, "partial..."),
		StopReason: llm.StopMaxTokens,
	}
	model := llmtest.NewScripted(truncated, llmtest.SayText("finished"))
	exec := NewExecutor(model)

	steps, cp, _ := driveToDone(t, exec, 5)
	if steps != 2 || cp.Result != "finished" {
		t.Fatalf("got steps=%d result=%q, want 2/finished", steps, cp.Result)
	}
	// The continuation nudge must have been appended after the truncated turn.
	cont := model.Requests()[1].Messages
	if last := cont[len(cont)-1]; last.Role != llm.RoleUser || last.TextContent() != "Continue." {
		t.Fatalf("truncated turn was not continued: %+v", last)
	}
}

// TestConvergenceNotMetMidConversation: a checkpoint that is not done does not
// converge, and an empty one (no turns yet) does not either.
func TestConvergenceNotMetMidConversation(t *testing.T) {
	spec := goal.Spec{Objective: "o", StopCondition: "c"}
	if met, _, _ := (Convergence{}).Met(context.Background(), spec, goal.Status{}); met {
		t.Fatal("empty checkpoint must not be converged")
	}
	mid, _ := encodeCheckpoint(checkpoint{Messages: []llm.Message{llm.Text(llm.RoleUser, "x")}})
	if met, _, _ := (Convergence{}).Met(context.Background(), spec, goal.Status{Checkpoint: mid}); met {
		t.Fatal("in-progress checkpoint must not be converged")
	}
}

// TestExecutorNoopWhenDone: executing an already-finished mission returns the
// checkpoint unchanged and does not call the model again.
func TestExecutorNoopWhenDone(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("done"))
	exec := NewExecutor(model)
	_, _, raw := driveToDone(t, exec, 5)

	again, err := exec.Execute(context.Background(), res(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(raw) {
		t.Fatalf("done mission was advanced: %s -> %s", raw, again)
	}
	if model.Calls() != 1 {
		t.Fatalf("model called %d times, want 1 (no call once done)", model.Calls())
	}
}

// denyTool is an Admitter that rejects a named tool, standing in for a capability
// or budget gate.
type denyTool struct{ name string }

func (d denyTool) Admit(_ context.Context, a dispatch.Action) error {
	if a.Name == d.name {
		return fault.New(fault.Terminal, "capability_denied", "capability not granted: "+a.Name)
	}
	return nil
}

// TestExecutorAdmitterGovernsToolCalls proves tool calls flow through the
// governance gate: a rejected call never runs and comes back to the model as an
// error result it can adapt to.
func TestExecutorAdmitterGovernsToolCalls(t *testing.T) {
	calls := 0
	tool := Func(echoDef, func(_ context.Context, input json.RawMessage) (string, error) {
		calls++
		return string(input), nil
	})
	model := llmtest.NewScripted(
		llmtest.CallTool("t1", "echo", json.RawMessage(`{"x":1}`)),
		llmtest.SayText("ok"),
	)
	exec := NewExecutor(model, WithTools(tool), WithAdmitter(denyTool{"echo"}))
	driveToDone(t, exec, 5)

	if calls != 0 {
		t.Fatalf("denied tool ran %d times; admission must prevent the side effect", calls)
	}
	last := model.Requests()[1].Messages
	found := false
	for _, m := range last {
		for _, b := range m.Blocks {
			if b.Kind == llm.KindToolResult && b.ToolResult.IsError && strings.Contains(b.ToolResult.Content, "capability not granted") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("admitter rejection was not surfaced to the model as an error result")
	}
}

// TestExecutorRecordsToolDispatchEvents proves every tool call is recorded on the
// event spine through the waist, which is what makes the run auditable and
// replayable.
func TestExecutorRecordsToolDispatchEvents(t *testing.T) {
	sink := &dispatch.MemorySink{}
	model := llmtest.NewScripted(
		llmtest.CallTool("t1", "echo", json.RawMessage(`{}`)),
		llmtest.SayText("done"),
	)
	exec := NewExecutor(model, WithTools(echoTool()), WithEventSink(sink))
	driveToDone(t, exec, 5)

	var start, end bool
	for _, e := range sink.Events() {
		if e.Action != "echo" {
			continue
		}
		switch e.Type {
		case dispatch.EventStart:
			start = true
		case dispatch.EventEnd:
			end = true
		}
	}
	if !start || !end {
		t.Fatalf("tool dispatch not bracketed on the event spine: %+v", sink.Events())
	}
}

// TestMissionConvergesProperty is the loop's behavioural contract: for any number
// of tool-call turns followed by a final text turn, the executor converges in
// exactly one step per turn, runs each tool, never calls the model after it is
// done, and surfaces the model's final answer. Driving step-by-step through
// serialized checkpoints also exercises crash-resume on every iteration.
func TestMissionConvergesProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		toolTurns := rapid.IntRange(0, 6).Draw(rt, "toolTurns")
		final := rapid.StringMatching(`[a-z ]{1,12}`).Draw(rt, "final")

		turns := make([]llm.Response, 0, toolTurns+1)
		for range toolTurns {
			turns = append(turns, llmtest.CallTool("t", "echo", json.RawMessage(`{}`)))
		}
		turns = append(turns, llmtest.SayText(final))

		model := llmtest.NewScripted(turns...)
		exec := NewExecutor(model, WithTools(echoTool()))

		steps, cp, _ := driveToDone(t, exec, toolTurns+2)
		if steps != toolTurns+1 {
			rt.Fatalf("converged in %d steps, want %d", steps, toolTurns+1)
		}
		if cp.Result != final {
			rt.Fatalf("final result = %q, want %q", cp.Result, final)
		}
		if model.Calls() != toolTurns+1 {
			rt.Fatalf("model called %d times, want %d", model.Calls(), toolTurns+1)
		}
	})
}

// TestExecutorDeclaresCacheHint checks the loop tells the model which prefix is
// stable: the static prefix every turn, and a message boundary that rolls forward
// as the conversation grows, so a caching backend reuses the frozen history.
func TestExecutorDeclaresCacheHint(t *testing.T) {
	model := llmtest.NewScripted(
		llmtest.CallTool("t1", "echo", json.RawMessage(`{"msg":"hi"}`)),
		llmtest.SayText("all done"),
	)
	exec := NewExecutor(model, WithTools(echoTool()), WithSystem("be brief"))
	driveToDone(t, exec, 5)

	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("want 2 model calls, got %d", len(reqs))
	}
	for i, r := range reqs {
		if !r.Cache.Prefix {
			t.Fatalf("call %d did not mark the static prefix as cacheable", i)
		}
		if r.Cache.Key == "" {
			t.Fatalf("call %d did not key the cache to the run", i)
		}
		if r.Cache.StableMessages != len(r.Messages) {
			t.Fatalf("call %d: StableMessages=%d, want full history %d", i, r.Cache.StableMessages, len(r.Messages))
		}
	}
	// The boundary rolls forward: the second call has more stable history than the
	// first (the tool call, its result, and the prompt).
	if reqs[1].Cache.StableMessages <= reqs[0].Cache.StableMessages {
		t.Fatalf("rolling boundary did not advance: %d then %d", reqs[0].Cache.StableMessages, reqs[1].Cache.StableMessages)
	}
	// The cache key is constant across the conversation's turns, which is what lets a
	// cache-affinity provider keep them on the same backend.
	if reqs[0].Cache.Key != reqs[1].Cache.Key {
		t.Fatalf("cache key changed between turns: %q then %q", reqs[0].Cache.Key, reqs[1].Cache.Key)
	}
}
