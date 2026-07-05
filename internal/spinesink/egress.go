package spinesink

import (
	"context"

	"github.com/ionalpha/flynn/netguard"
	"github.com/ionalpha/flynn/spine"
)

// egressEventType is the event type an egress decision is recorded under on a run's
// stream. It is the wire contract the session package projects into KindEgressDecision;
// the two constants are pinned equal by a session round-trip test so they cannot drift.
const egressEventType = "net.egress"

// EgressSink records netguard's egress decisions onto a run's spine stream as
// net.egress events, so a run's outbound network verdicts are part of the same recorded
// history as its governed actions. Construct one per run with that run's stream id and
// install it on the run's context with netguard.WithObserver, so every dial netguard
// makes under that context reports here. A run that makes no netguard-gated egress (a
// local, loopback model, say) records nothing, so the egress row stays silent.
type EgressSink struct {
	log    spine.Log
	stream string
}

// NewEgress returns an EgressSink that writes egress decisions to log on the given
// stream, attributed to the agent.
func NewEgress(log spine.Log, stream string) *EgressSink {
	return &EgressSink{log: log, stream: stream}
}

// Observe is a netguard.Observer: it appends one net.egress event per decision, naming
// the destination host, the verdict (allowed or blocked), and the reason. It runs on a
// dial goroutine, so it uses a background context (a cancelled dial still records its
// verdict) and drops a failed append rather than surfacing it: the record is
// observability, and the dial's own allow/deny already enforced the policy.
func (s *EgressSink) Observe(d netguard.Decision) {
	verdict := "blocked"
	if d.Allowed {
		verdict = "allowed"
	}
	_, _ = s.log.Append(context.Background(), spine.AppendInput{
		Stream: s.stream,
		Type:   egressEventType,
		Actor:  spine.ActorAgent,
		Payload: map[string]any{
			"host":    d.Host,
			"verdict": verdict,
			"reason":  d.Reason,
		},
	})
}
