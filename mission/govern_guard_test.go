package mission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestEveryModelAndToolCallIsGoverned is the invariant guard for the dispatch
// waist: it drives a real mission and asserts that BOTH the model call and the
// tool call left a start/end pair on the event sink. A regression that calls the
// model (or a tool) directly, bypassing the waist, drops its action from the sink
// and fails here, so the "every action is governed" invariant cannot quietly rot.
func TestEveryModelAndToolCallIsGoverned(t *testing.T) {
	sink := &dispatch.MemorySink{}
	tool := Func(echoDef, func(_ context.Context, in json.RawMessage) (string, error) {
		return string(in), nil
	})
	// Turn 1 calls the tool, turn 2 ends: two model calls and one tool call.
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "echo", json.RawMessage(`{"x":1}`)),
		llmtest.SayText("done"),
	)
	exec := NewExecutor(model, WithTools(tool), WithEventSink(sink))
	driveToDone(t, exec, 5)

	events := sink.Events()
	testkit.RequireLifecycle(t, events) // every start is paired with an end

	starts := map[string]int{}
	for _, e := range events {
		if e.Type == dispatch.EventStart {
			starts[e.Action]++
		}
	}
	if starts[ActionModelGenerate] != 2 {
		t.Fatalf("model call not governed: %d %q starts, want 2 (the call bypassed the waist?)",
			starts[ActionModelGenerate], ActionModelGenerate)
	}
	if starts["echo"] != 1 {
		t.Fatalf("tool call not governed: %d echo starts, want 1", starts["echo"])
	}
}
