package spinesink

import (
	"context"
	"strings"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// approvalEventType is the event type an authorization decision is recorded under on a
// run's stream. It sits beside the waist's own dispatch.* events rather than inside
// them: the gate decides before the action is governed, and a denial has no governed
// call to hang off.
const approvalEventType = "approval.decision"

// ApprovalSink records the approval gate's decisions onto a run's spine stream, so who
// authorized what is part of the same sealed history as the actions themselves. It
// replaces approval.DiscardSink, which is the right default for a library and the wrong
// one for a run whose record is meant to be checkable afterwards: an authorization
// nothing wrote down cannot be audited, and a refused attempt that left no trace reads
// exactly like an attempt that was never made.
//
// Construct one per run with that run's stream id and pass it to approval.WithSink.
// A run whose policy requires nothing records nothing, so the row stays silent.
type ApprovalSink struct {
	log    spine.Log
	stream string
}

// NewApproval returns an ApprovalSink that writes authorization decisions to log on the
// given stream, attributed to the user: the decision is a person's, even when it is the
// gate that reports it.
func NewApproval(log spine.Log, stream string) *ApprovalSink {
	return &ApprovalSink{log: log, stream: stream}
}

// Record appends one approval.decision event, naming the action and scope that were
// asked for, the target the authorization was narrowed to, whether it was granted, the
// approvers whose signatures counted, and the reason for a denial.
//
// It records the grant and the denial alike, which is the point: a gate that only logs
// what it let through is a record of successes, not of decisions. The returned error is
// the sink's contract with the gate, which treats it as best-effort and never admits an
// action on a sink that failed.
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

// scopeString renders a scope as the slash-joined path a reader of the record expects,
// with the empty scope (the one a standalone run uses) rendered as the empty string
// rather than a row of separators.
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
