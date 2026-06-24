package mission

import (
	"context"
	"encoding/json"
)

// EventKind classifies a conversational event a mission reports as it runs.
type EventKind string

const (
	// EventTurnStarted marks the beginning of one model turn, before the model is
	// called.
	EventTurnStarted EventKind = "turn.started"
	// EventAssistantText carries the natural-language text the model produced this
	// turn (empty turns that only call tools produce none).
	EventAssistantText EventKind = "assistant.text"
	// EventToolCall is the model asking to invoke a tool, with its arguments. It is
	// reported before the tool runs.
	EventToolCall EventKind = "tool.call"
	// EventToolResult is the outcome of a tool call, reported after it runs.
	EventToolResult EventKind = "tool.result"
	// EventTurnCompleted marks the end of a turn, carrying why the model stopped.
	EventTurnCompleted EventKind = "turn.completed"
)

// Event is one observable moment in a mission's conversation: a turn boundary, the
// model's text, a tool call, or a tool result. It is reported live as the loop
// runs so a caller can render progress without polling. Fields are populated by
// Kind; the rest are zero.
type Event struct {
	Kind EventKind
	// Turn is the 1-based index of the model turn this event belongs to.
	Turn int
	// Text is the assistant's text (EventAssistantText).
	Text string
	// Tool is the tool's name (EventToolCall, EventToolResult).
	Tool string
	// ToolUseID correlates a call with its result across the two events.
	ToolUseID string
	// Input is the tool's JSON arguments (EventToolCall).
	Input json.RawMessage
	// Result is the tool's output text (EventToolResult).
	Result string
	// IsError reports that the tool call failed (EventToolResult).
	IsError bool
	// StopReason is why the turn ended (EventTurnCompleted): the llm.StopReason.
	StopReason string
}

// Reporter receives a mission's conversational events as they happen, so a caller
// can stream live progress (turns, the model's text, tool calls and their
// results). It is additive observability layered beside the dispatch event spine:
// the default is a no-op and the loop's behaviour never depends on it. Report runs
// on the worker's step goroutine and must not block; a slow consumer should hand
// off to its own queue.
type Reporter interface {
	Report(ctx context.Context, ev Event)
}

// nopReporter is the zero-config default: it drops every event, so a mission with
// no observer behaves exactly as before.
type nopReporter struct{}

func (nopReporter) Report(context.Context, Event) {}

var _ Reporter = nopReporter{}
