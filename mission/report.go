package mission

import (
	"context"
	"encoding/json"

	"github.com/ionalpha/flynn/llm"
)

// EventKind classifies an [Event].
type EventKind string

const (
	// EventTurnStarted is reported before the model is called.
	EventTurnStarted EventKind = "turn.started"
	// EventAssistantText is absent on a turn that only calls tools.
	EventAssistantText EventKind = "assistant.text"
	// EventToolCall is reported before the tool runs.
	EventToolCall EventKind = "tool.call"
	// EventToolResult is reported after the tool returns.
	EventToolResult EventKind = "tool.result"
	// EventTurnCompleted closes the turn.
	EventTurnCompleted EventKind = "turn.completed"
	// EventChildSpawned records the parent-to-child edge, with the sub-objective in Text,
	// so a fan-out tree can be built from the record alone.
	EventChildSpawned EventKind = "child.spawned"
	// EventChildCompleted closes the edge [EventChildSpawned] opened, with the answer in
	// Result.
	EventChildCompleted EventKind = "child.completed"
)

// Event is reported live so a caller renders progress without polling. Only the fields
// its Kind uses are set.
type Event struct {
	Kind EventKind
	// Goal keeps concurrent children on one stream apart. Empty means the root goal.
	Goal  string
	Child string
	// Turn is 1-based.
	Turn int
	Text string
	Tool string
	// ToolUseID pairs a call with its result.
	ToolUseID  string
	Input      json.RawMessage
	Result     string
	IsError    bool
	StopReason string
	Usage      llm.Usage
}

// Reporter streams a mission's events. The loop never depends on it. Report runs on the
// step goroutine and must not block, so a slow consumer hands off to its own queue.
type Reporter interface {
	Report(ctx context.Context, ev Event)
}

type nopReporter struct{}

func (nopReporter) Report(context.Context, Event) {}

var _ Reporter = nopReporter{}
