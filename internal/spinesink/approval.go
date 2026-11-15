package spinesink

import (
	"context"
	"strings"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// approvalEventType is the event type an authorization decision is recorded under. It
// sits beside the dispatch.* events because the gate decides before the action is
// governed, and a denial has no governed call to attach to.
const approvalEventType = "approval.decision"

// ApprovalSink records the approval gate's decisions onto a run's spine stream, so who
// authorized what is in the same sealed history as the actions. It replaces
// approval.DiscardSink for a run whose record must be checkable: an unrecorded
// authorization cannot be audited, and an unrecorded refusal reads like an attempt
// that was never made.
//
// Construct one per run with that run's stream id and pass it to approval.WithSink.
type ApprovalSink struct {
	log    spine.Log
	stream string
}

// NewApproval returns an ApprovalSink that writes to log on stream, attributed to the
// user: the decision is a person's even when the gate reports it.
func NewApproval(log spine.Log, stream string) *ApprovalSink {
	return &ApprovalSink{log: log, stream: stream}
}

// Record appends one approval.decision event naming the action and scope asked for,
// the target the authorization was narrowed to, whether it was granted, the approvers
// whose signatures counted, and the reason for a denial. Grants and denials are both
// recorded, so the log is a record of decisions. The gate treats the returned error
// as best effort and never admits an action on a sink that failed.
func (s *ApprovalSink) Record(ctx context.Context, d approval.Decision) error {
	verdict := "denied"
	if d.Granted {
		verdict = "granted"
	}
	_, err := s.log.Append(ctx, spine.AppendInput{
		Stream: s.stream,
		Type:   approvalEventType,
		Actor:  spine.ActorHuman,
		Payload: map[string]any{
			"action":    d.Envelope.Action,
			"scope":     scopeString(d.Envelope.Scope),
			"detail":    d.Envelope.Detail,
			"host":      d.Envelope.Host,
			"principal": d.Envelope.Principal,
			"verdict":   verdict,
			"approvers": strings.Join(d.KeyIDs, ","),
			"reason":    d.Reason,
		},
	})
	return err
}

// scopeString renders a scope as a slash-joined path, and the empty scope of a
// standalone run as the empty string.
func scopeString(s state.Scope) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{s.Instance, s.Project, s.Workspace} {
		if p == "" {
			break
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "/")
}

var _ approval.Sink = (*ApprovalSink)(nil)
