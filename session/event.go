package session

import (
	"encoding/json"
	"time"

	"github.com/ionalpha/flynn/spine"
)

// Kind classifies a session event. The set spans a conversation's whole arc: the
// session opening, each model turn and the text and tool calls within it, and the
// terminal outcome. A renderer switches on Kind to draw the live transcript.
type Kind string

const (
	// KindSessionStarted is the first event: the session was opened with an
	// objective.
	KindSessionStarted Kind = "session.started"
	// KindTurnStarted marks the start of one model turn.
	KindTurnStarted Kind = "turn.started"
	// KindAssistant carries the model's natural-language text for a turn.
	KindAssistant Kind = "assistant.message"
	// KindToolCall is the model requesting a tool, with its arguments.
	KindToolCall Kind = "tool.call"
	// KindToolResult is the outcome of a tool call.
	KindToolResult Kind = "tool.result"
	// KindTurnCompleted marks the end of a turn, with why the model stopped.
	KindTurnCompleted Kind = "turn.completed"
	// KindConverged is the terminal success event: the goal's stop condition was
	// met, with the model's final answer.
	KindConverged Kind = "session.converged"
	// KindStalled is the terminal failure event: the goal ran out of budget or a
	// step failed terminally, with the reason.
	KindStalled Kind = "session.stalled"
)

// Event is one record on a session's event stream: an ordered, replayable view of
// the conversation a UI renders live. Seq is monotonic within a session and Time
// is the moment it was recorded; the remaining fields are populated by Kind and
// are otherwise zero. It is the public "streams/sessions/chat" surface, a typed
// projection of the underlying event spine.
type Event struct {
	Seq   int64           `json:"seq"`
	Time  time.Time       `json:"time"`
	Kind  Kind            `json:"kind"`
	Actor spine.ActorType `json:"actor"`

	// Turn is the 1-based model turn the event belongs to (0 for session-level
	// events: started, converged, stalled).
	Turn int `json:"turn,omitempty"`
	// Text carries the objective (started), the model's message (assistant), or the
	// final answer (converged).
	Text string `json:"text,omitempty"`
	// Tool is the tool's name (tool.call, tool.result).
	Tool string `json:"tool,omitempty"`
	// ToolUseID correlates a tool.call with the tool.result that answers it.
	ToolUseID string `json:"toolUseID,omitempty"`
	// Input is the tool's JSON arguments (tool.call).
	Input json.RawMessage `json:"input,omitempty"`
	// Result is the tool's output text (tool.result).
	Result string `json:"result,omitempty"`
	// IsError reports a failed tool call (tool.result).
	IsError bool `json:"isError,omitempty"`
	// StopReason is why the turn ended (turn.completed).
	StopReason string `json:"stopReason,omitempty"`
	// Err carries the stall reason (stalled).
	Err string `json:"error,omitempty"`
}

// payloadKey is the single spine-payload field the event body is serialized under.
// Folding the whole body into one JSON string keeps the round-trip lossless across
// any spine backend (the in-memory log shares maps; a durable log re-encodes the
// payload as JSON, where a plain string survives unchanged).
const payloadKey = "event"

// toAppend renders the event into a spine append on the given stream. Seq and Time
// are assigned by the log on append (Seq) and read back from it; the body carries
// everything else.
func (e Event) toAppend(stream string) spine.AppendInput {
	body, _ := json.Marshal(e)
	return spine.AppendInput{
		Stream:  stream,
		Type:    string(e.Kind),
		Actor:   e.Actor,
		Time:    e.Time,
		Payload: map[string]any{payloadKey: string(body)},
	}
}

// fromSpine reconstructs a session event from a spine event, taking Seq, Time,
// Kind, and Actor from the log's authoritative fields and the rest from the body.
func fromSpine(se spine.Event) Event {
	var e Event
	if s, ok := se.Payload[payloadKey].(string); ok {
		_ = json.Unmarshal([]byte(s), &e)
	}
	e.Seq = se.Seq
	e.Time = se.Time
	e.Kind = Kind(se.Type)
	e.Actor = se.Actor
	return e
}
