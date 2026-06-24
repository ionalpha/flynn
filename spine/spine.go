// Package spine is the agent's canonical event log: one durable, ordered,
// replayable stream of events per run. It is the single source of truth from
// which state, audit, metrics, replay, and live progress are all derived — the
// "event spine" the architecture is built around.
//
// Generalising the orchestration event spine into this substrate is the single
// biggest refactor-avoider: checkpoint/replay, rollback, monitoring, and
// human-in-the-loop approvals all project from one log instead of parallel,
// drifting ones. The durable (SQLite) Log lands later; MemoryLog defines the
// semantics it must match.
package spine

import (
	"context"
	"time"
)

// ActorType identifies who produced an event.
type ActorType string

const (
	// ActorAgent is the agent itself (a worker or coordinator run).
	ActorAgent ActorType = "agent"
	// ActorHuman is a person (an approval, a correction, an instruction).
	ActorHuman ActorType = "human"
	// ActorSystem is the runtime (scheduling, heartbeats, lifecycle).
	ActorSystem ActorType = "system"
)

// DefaultSchemaVersion is the version stamped on an event whose SchemaVersion is
// left unset (0) at append time. It is the baseline shape every event type starts
// at, so existing logs and callers that do not yet care about versioning all read
// as version 1.
const DefaultSchemaVersion = 1

// Event is one immutable, ordered record on a stream. Seq is monotonic within a
// stream; events are never mutated or deleted.
type Event struct {
	Stream  string
	Seq     int64
	Time    time.Time
	Type    string
	Actor   ActorType
	Payload map[string]any

	// SchemaVersion is the version of this event's payload shape for its Type.
	// Because Payload is a map, adding a key is backward compatible, but renaming,
	// removing, or retyping one is not. The version lets newer code recognise an
	// older payload and migrate it (see UpcastRegistry) instead of misreading it.
	// The log stamps an unset (0) version to DefaultSchemaVersion on append, so
	// every stored event carries an explicit version.
	SchemaVersion int

	// Trace linkage cross-references the OpenTelemetry span layer so ops traces
	// and the semantic spine line up. May be empty until the tracing adapter
	// populates them. CausationID is the event (or span) that caused this one,
	// enabling exact causal replay.
	TraceID     string
	SpanID      string
	CausationID string

	// OriginInstanceID is the instance that first produced this event. Set from
	// the start so multi-instance (fleet/P2P) sync never forces an ID refactor.
	OriginInstanceID string
}

// AppendInput appends one event to a stream. The Log assigns Seq, assigns Time
// from its clock when Time is zero, and stamps SchemaVersion to
// DefaultSchemaVersion when it is left unset (0).
type AppendInput struct {
	Stream           string
	Type             string
	Actor            ActorType
	Payload          map[string]any
	SchemaVersion    int
	Time             time.Time
	TraceID          string
	SpanID           string
	CausationID      string
	OriginInstanceID string
}

// Query reads a contiguous slice of a stream in Seq order.
type Query struct {
	Stream   string
	AfterSeq int64 // exclusive lower bound; 0 returns from the start
	Limit    int   // <= 0 means no limit
}

// Log is the append-only event store — the spine port. Append assigns a
// monotonic Seq within a stream; Read returns events in Seq order. A host
// supplies a durable implementation; the agent ships MemoryLog.
type Log interface {
	Append(ctx context.Context, in AppendInput) (Event, error)
	Read(ctx context.Context, q Query) ([]Event, error)
}

// Fold replays events through a reducer to project state: state is a function of
// the log. This is the core of the event-sourcing substrate — every view
// (progress, metrics, the current session) is a Fold over the spine.
func Fold[S any](events []Event, initial S, reduce func(S, Event) S) S {
	state := initial
	for _, e := range events {
		state = reduce(state, e)
	}
	return state
}
