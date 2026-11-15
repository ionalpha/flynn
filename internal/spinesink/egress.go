package spinesink

import (
	"context"

	"github.com/ionalpha/flynn/netguard"
	"github.com/ionalpha/flynn/spine"
)

// egressEventType is the wire contract the session package projects into
// KindEgressDecision; a session round-trip test pins the two constants equal.
const egressEventType = "net.egress"

// EgressSink records netguard's egress decisions onto a run's spine stream as
// net.egress events. Construct one per run and install it with netguard.WithObserver,
// so every dial netguard makes under that context reports here.
type EgressSink struct {
	log    spine.Log
	stream string
}

// NewEgress returns an EgressSink that writes to log on stream, attributed to the
// agent.
func NewEgress(log spine.Log, stream string) *EgressSink {
	return &EgressSink{log: log, stream: stream}
}

// Observe is a netguard.Observer that appends one net.egress event per decision (host,
// verdict, reason). It runs on a dial goroutine, so it uses a background context, so a
// cancelled dial still records its verdict, and drops a failed append, because the
// dial's own allow or deny already enforced the policy.
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
