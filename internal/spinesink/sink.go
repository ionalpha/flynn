// Package spinesink records dispatch, approval, budget and egress events onto a run's
// spine stream. It sits outside both spine and dispatch, so the spine core takes no
// dependency on dispatch or state, which would form an import cycle.
package spinesink

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// Sink records the dispatch waist's lifecycle events (start, end, rejected) onto a
// spine stream. Construct one per run with its stream id and pass it via
// dispatch.WithEventSink.
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

// Append implements dispatch.EventSink. The dispatcher's timestamp (e.At, unix nanos)
// is preserved, so the two layers agree on time.
func (s *Sink) Append(ctx context.Context, e dispatch.Event) error {
	// The correlation id pairs this invocation's start with its end or rejection, so a
	// verifier can prove no action completed without a preceding admission and no
	// denied action also completed.
	payload := map[string]any{"action": e.Action, "call": e.Call}
	// The trust level puts the run's containment posture (which actions ran as
	// model-authored or external) in the sealed record as well as the live trace.
	if e.Trust != "" {
		payload["trust"] = e.Trust
	}
	if e.Err != "" {
		payload["error_class"] = e.Err
	}
	// The code names the rule that refused. Every gate refuses with the same few
	// classes, so with the class alone the record shows that a run was blocked and
	// never what blocked it.
	if e.Code != "" {
		payload["error_code"] = e.Code
	}
	// The goal makes a fan-out's per-child governance (which child was admitted, which
	// hit a limit) derivable from one stream.
	if e.Goal != "" {
		payload["goal"] = e.Goal
	}
	if e.Scope != (state.Scope{}) {
		payload["scope"] = map[string]string{
			"instance":  e.Scope.Instance,
			"project":   e.Scope.Project,
			"workspace": e.Scope.Workspace,
		}
	}
	_, err := s.log.Append(ctx, spine.AppendInput{
		Stream:    s.stream,
		Type:      e.Type,
		Actor:     s.actor,
		Payload:   payload,
		Time:      time.Unix(0, e.At).UTC(),
		Principal: capability.PrincipalFromContext(ctx),
	})
	return err
}

var _ dispatch.EventSink = (*Sink)(nil)
