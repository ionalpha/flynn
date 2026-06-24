package mission

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/llm/llmtest"
)

// recordingReporter captures every reported event in order, for assertion.
type recordingReporter struct {
	mu  sync.Mutex
	evs []Event
}

func (r *recordingReporter) Report(_ context.Context, ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, ev)
}

func (r *recordingReporter) events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.evs))
	copy(out, r.evs)
	return out
}

func kinds(evs []Event) []EventKind {
	out := make([]EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

// TestExecutorReportsConversationEvents drives a two-turn mission (a tool call,
// then a final answer) and asserts the executor reports the full conversational
// spine in order, with the right turn indices and payloads.
func TestExecutorReportsConversationEvents(t *testing.T) {
	rec := &recordingReporter{}
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "echo", json.RawMessage(`{"x":1}`)),
		llmtest.SayText("all done"),
	)
	exec := NewExecutor(model, WithTools(echoTool()), WithObserver(rec))
	driveToDone(t, exec, 5)

	evs := rec.events()
	want := []EventKind{
		EventTurnStarted, EventToolCall, EventToolResult, EventTurnCompleted, // turn 1
		EventTurnStarted, EventAssistantText, EventTurnCompleted, // turn 2
	}
	got := kinds(evs)
	if len(got) != len(want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d kind = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	// Turn indices: the first four events are turn 1, the last three are turn 2.
	for i, e := range evs {
		wantTurn := 1
		if i >= 4 {
			wantTurn = 2
		}
		if e.Turn != wantTurn {
			t.Fatalf("event %d (%s) turn = %d, want %d", i, e.Kind, e.Turn, wantTurn)
		}
	}

	// The tool call carries the model's request verbatim.
	call := evs[1]
	if call.Tool != "echo" || call.ToolUseID != "c1" || string(call.Input) != `{"x":1}` {
		t.Fatalf("tool call event = %+v", call)
	}
	// The tool result carries the echoed output and is not an error.
	result := evs[2]
	if result.Tool != "echo" || result.ToolUseID != "c1" || result.IsError || !strings.Contains(result.Result, `"x":1`) {
		t.Fatalf("tool result event = %+v", result)
	}
	// The first turn ended on tool use, the second on end-of-turn.
	if evs[3].StopReason != string(llmtest.CallTool("", "", nil).StopReason) {
		t.Fatalf("turn 1 stop reason = %q", evs[3].StopReason)
	}
	if evs[5].Text != "all done" {
		t.Fatalf("assistant text = %q", evs[5].Text)
	}
	if evs[6].StopReason != string(llmtest.SayText("").StopReason) {
		t.Fatalf("turn 2 stop reason = %q", evs[6].StopReason)
	}
}

// TestExecutorWithoutObserverIsSilent confirms the reporter is purely additive: a
// mission with no observer (and one given a nil observer) drives to completion
// exactly as before, with no panic.
func TestExecutorWithoutObserverIsSilent(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("done"))
	exec := NewExecutor(model, WithObserver(nil))
	_, cp, _ := driveToDone(t, exec, 3)
	if !cp.Done || cp.Result != "done" {
		t.Fatalf("checkpoint = %+v", cp)
	}
}
