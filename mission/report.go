package mission

import (
	"context"
	"encoding/json"

	"github.com/ionalpha/flynn/llm"
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
	// EventChildSpawned marks a fan-out delegation: the goal named by Goal spawned the
	// child named by Child to run a sub-objective (carried in Text). It records the
	// parent-to-child edge on the stream, so a live tree of a fan-out can be built from
	// the record rather than a separate store.
	EventChildSpawned EventKind = "child.spawned"
	// EventChildCompleted marks a spawned child folding back into its parent: the child
	// named by Child finished and its answer (in Result, with IsError set when it
	// failed) was folded into the parent named by Goal. It is the closing half of the
	// edge EventChildSpawned opened, so a tree can flip the child from running to done.
	EventChildCompleted EventKind = "child.completed"
)

// Event is one observable moment in a mission's conversation: a turn boundary, the
// model's text, a tool call, or a tool result. It is reported live as the loop
// runs so a caller can render progress without polling. Fields are populated by
// Kind; the rest are zero.
type Event struct {
	Kind EventKind
	// Goal is the id of the goal this event belongs to: the run's root goal for a
	// single conversation, or a delegated child's own id under fan-out. It attributes
	// each event to its originating goal so events from concurrent children on one
	// shared stream stay distinguishable. Empty is treated as the root.
	Goal string
	// Child is the id of a spawned child goal (EventChildSpawned only), the other end
	// of the parent-to-child edge whose parent is Goal.
	Child string
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
	// Usage is the token cost the model reported for the turn (EventTurnCompleted),
	// including the cache read/write split, so a caller can show live spend and
	// cache effectiveness without re-deriving it.
	Usage llm.Usage
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
