package spine

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/state"
)

// Sink adapts the event log to dispatch.EventSink: it records the dispatch
// waist's lifecycle events (start/end/rejected) onto a stream. Construct one per
// run with that run's stream id, then pass it via dispatch.WithEventSink — every
// action that run dispatches is then captured on the spine.
type Sink struct {
	log    Log
	stream string
	actor  ActorType
}

// NewSink returns a Sink that writes dispatch events to log on the given stream,
// attributed to the agent.
func NewSink(log Log, stream string) *Sink {
	return &Sink{log: log, stream: stream, actor: ActorAgent}
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
	_, err := s.log.Append(ctx, AppendInput{
		Stream:  s.stream,
		Type:    e.Type,
		Actor:   s.actor,
		Payload: payload,
		Time:    time.Unix(0, e.At).UTC(),
	})
	return err
}

var _ dispatch.EventSink = (*Sink)(nil)
