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

	// KindActionAdmitted is a governed action admitted by the dispatch waist, the
	// waist's start event projected onto the conversation stream. The waist records
	// every governed action (a tool, the model call, a delegation) on the run's own
	// stream, so its admission decisions interleave with the turns they belong to.
	// This projection never changes the stored event type (dispatch.start), which
	// stays the verifiable governance record a sealer signs and a verifier checks.
	KindActionAdmitted Kind = "action.admitted"
	// KindActionCompleted is a governed action that finished, carrying a fault class
	// when it ran but errored. Projected from the waist's end event (dispatch.end).
	KindActionCompleted Kind = "action.completed"
	// KindActionRejected is a governed action the waist refused, carrying the denial's
	// fault class. Projected from the waist's rejection event (dispatch.rejected).
	KindActionRejected Kind = "action.rejected"

	// KindRecordSealed marks the run's stream sealed into a signed record, projected
	// from the spine.record event the sealer stores on the stream.
	KindRecordSealed Kind = "record.sealed"
	// KindRecordVerified marks a sealed record checked against its tiers. It is emitted
	// when verification runs, so a replay reproduces the record badge's transitions.
	KindRecordVerified Kind = "record.verified"
)

// The dispatch waist and the sealer write these event types onto a run's stream
// alongside the conversation. They are the wire contract this package projects into
// the session vocabulary above; the values mirror dispatch's own constants and the
// sealer's record type, and decodeGovernance/record round-trip tests pin them so the
// two cannot silently drift.
const (
	typeDispatchStart    = "dispatch.start"
	typeDispatchEnd      = "dispatch.end"
	typeDispatchRejected = "dispatch.rejected"
	typeRecordSealed     = "spine.record"
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
	// Usage is the token cost of a turn (turn.completed), nil for other events. It
	// is carried on the stream, not just metered internally, so a live UI and a
	// replay both show per-turn spend and cache effectiveness.
	Usage *Usage `json:"usage,omitempty"`
	// Err carries the stall reason (stalled).
	Err string `json:"error,omitempty"`

	// Action is the governed action's name (action.admitted/completed/rejected): a
	// tool, the model call, or the spawn that delegates a sub-goal. It is broader than
	// Tool, which names only a model-requested tool.
	Action string `json:"action,omitempty"`
	// Call correlates an action.admitted with the action.completed or action.rejected
	// that answers it, so a renderer pairs an admission with its outcome and a panel
	// can show only the actions still in flight.
	Call int64 `json:"call,omitempty"`
	// Trust is the action's trust level (action.* events): how far the work is
	// trusted, which is the containment a gate requires to admit it.
	Trust string `json:"trust,omitempty"`
	// Fault is the fault class on a refused or failed action: the denial reason on
	// action.rejected (capability_denied, budget_exceeded, needs_approval, ...) or the
	// failure class on an action.completed that ran but errored (transient, ...). Empty
	// on a clean completion.
	Fault string `json:"fault,omitempty"`
}

// Usage is the token cost of one turn, projected onto the conversation stream. It
// mirrors the model port's accounting in the chat surface's own terms so a session
// consumer does not depend on the model package. InputTokens is the total input
// processed including any served from cache; CacheReadTokens is the cached subset
// (the discounted win), so cache-hit-rate is CacheReadTokens over InputTokens.
type Usage struct {
	InputTokens      int `json:"inputTokens,omitempty"`
	OutputTokens     int `json:"outputTokens,omitempty"`
	CacheReadTokens  int `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
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

// fromSpine reconstructs a session event from a spine event, taking Seq, Time, and
// Actor from the log's authoritative fields and the body from the payload. Two shapes
// share a run's stream: the conversation events this package writes (the whole body
// folded under one payload key) and the governance and record events the dispatch
// waist and the sealer write (their own payload shape). decodeBody dispatches on the
// stored type so both project into the one session vocabulary.
func fromSpine(se spine.Event) Event {
	e := decodeBody(se)
	e.Seq = se.Seq
	e.Time = se.Time
	e.Actor = se.Actor
	return e
}

// decodeBody builds the typed event body from a spine event by its stored type. The
// governance and record types are projected without touching the stored record, so a
// sealer still signs and a verifier still checks the original dispatch.start/end/
// rejected and spine.record events; a session consumer just reads them in the session's
// own vocabulary.
func decodeBody(se spine.Event) Event {
	switch se.Type {
	case typeDispatchStart:
		return governanceEvent(KindActionAdmitted, se)
	case typeDispatchEnd:
		// A completed action carries a fault class when it ran but errored (a transient
		// failure, say); a clean completion leaves Fault empty.
		return governanceEvent(KindActionCompleted, se)
	case typeDispatchRejected:
		return governanceEvent(KindActionRejected, se)
	case typeRecordSealed:
		return Event{Kind: KindRecordSealed}
	default:
		var e Event
		if s, ok := se.Payload[payloadKey].(string); ok {
			_ = json.Unmarshal([]byte(s), &e)
		}
		e.Kind = Kind(se.Type)
		return e
	}
}

// governanceEvent projects one dispatch-waist event into the session vocabulary,
// reading the waist's payload shape (action, call, trust, error_class). The
// correlation id survives a durable log's JSON round trip as a float, so it is read
// through a numeric coercion rather than a direct int64 assertion.
func governanceEvent(kind Kind, se spine.Event) Event {
	e := Event{Kind: kind}
	e.Action, _ = se.Payload["action"].(string)
	e.Trust, _ = se.Payload["trust"].(string)
	e.Fault, _ = se.Payload["error_class"].(string)
	e.Call = asInt64(se.Payload["call"])
	return e
}

// asInt64 reads a numeric payload value that may be an int64 (the in-memory log shares
// the map verbatim) or a float64 (a durable log re-encodes the payload as JSON and
// decodes numbers as floats), returning 0 for anything else.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	default:
		return 0
	}
}
