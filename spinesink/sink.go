// Package spinesink adapts the dispatch waist to the event spine: it implements
// dispatch.EventSink by translating each dispatched action's lifecycle events
// into spine events on a run's stream. It is the glue between two ports, so it
// lives outside both — keeping the spine core free of any dependency on dispatch
// or state (which would otherwise form an import cycle).
package spinesink

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// Sink records the dispatch waist's lifecycle events (start/end/rejected) onto a
// spine stream. Construct one per run with that run's stream id, then pass it via
// dispatch.WithEventSink — every action that run dispatches is then captured on
// the spine.
type Sink struct {
	log    spine.Log
	stream string
	actor  spine.ActorType
}

// New returns a Sink that writes dispatch events to log on the given stream,
// attributed to the agent.
func New(log spine.Log, stream string) *Sink {
	return &Sink{log: log, stream: stream, actor: spine.ActorAgent}
}

// Append implements dispatch.EventSink by translating a dispatch event into a
// spine event. The dispatcher's timestamp (e.At, unix nanos) is preserved so the
// two layers agree on time.
func (s *Sink) Append(ctx context.Context, e dispatch.Event) error {
	payload := map[string]any{"action": e.Action}
	if e.Err != "" {
		payload["error_class"] = e.Err
	}
	if e.Scope != (state.Scope{}) {
		payload["scope"] = map[string]string{
			"instance":  e.Scope.Instance,
			"project":   e.Scope.Project,
			"workspace": e.Scope.Workspace,
		}
	}
	_, err := s.log.Append(ctx, spine.AppendInput{
		Stream:  s.stream,
		Type:    e.Type,
		Actor:   s.actor,
		Payload: payload,
		Time:    time.Unix(0, e.At).UTC(),
	})
	return err
}

var _ dispatch.EventSink = (*Sink)(nil)
