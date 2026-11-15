package governed

import (
	"context"
	"errors"
	"time"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/mission"
)

// recordingPrompter records each request before the prompt and exactly one resolution
// after it, on every return path. A resolution is a person's only when the prompter
// answered in time; expiry, cancellation and disconnection are the system's.
type recordingPrompter struct {
	inner mission.ApprovalPrompter
	sink  *spinesink.ApprovalSink
	clk   clock.Clock
}

var _ mission.ApprovalPrompter = recordingPrompter{}

// recording wraps prompter. A nil prompter stays nil, because a recorder would make a
// non-interactive run look interactive.
func (s *approvalStack) recording(prompter mission.ApprovalPrompter) mission.ApprovalPrompter {
	if s == nil || prompter == nil {
		return prompter
	}
	return recordingPrompter{inner: prompter, sink: s.sink, clk: clock.System{}}
}

// Prompt declines when the request cannot be recorded, and refuses an allow whose
// resolution cannot be recorded, so no action runs without a recorded question and
// answer. An unrecorded denial stays a denial. An answer after ctx is done (the grace
// period) is not honoured, because the record has already declined it.
func (r recordingPrompter) Prompt(ctx context.Context, req mission.ApprovalRequest) (mission.ApprovalDecision, error) {
	id := ids.New()
	var expires time.Time
	if req.Grace > 0 {
		expires = r.clk.Now().Add(req.Grace)
	}
	if err := r.sink.Request(ctx, spinesink.Request{
		ID: id, Action: req.Action, Scope: req.Scope, Host: req.Host, ExpiresAt: expires,
	}); err != nil {
		return mission.ApprovalDecision{}, err
	}

	dec, err := r.inner.Prompt(ctx, req)
	if err == nil && ctx.Err() != nil {
		dec, err = mission.ApprovalDecision{}, ctx.Err()
	}
	reason := resolution(ctx, dec, err)

	ans := spinesink.Answer{
		RequestID: id, Action: req.Action, Scope: req.Scope, Host: req.Host, Reason: reason,
	}
	if reason == approval.ReasonAllowed {
		ans.Grant = dec.Scope
	}
	if reason.ByPerson() {
		ans.FeedbackPresent = dec.Feedback != ""
	}
	// Written even when ctx is done, because that is when expiry and cancellation happen.
	if werr := r.sink.Answer(context.WithoutCancel(ctx), ans); werr != nil && reason == approval.ReasonAllowed {
		return mission.ApprovalDecision{}, werr
	}
	return dec, err
}

// resolution checks disconnection before the context, because it is stated outright. Any
// other prompter failure is recorded as disconnected, the closest reason in the set.
func resolution(ctx context.Context, dec mission.ApprovalDecision, err error) approval.Reason {
	switch {
	case err == nil && dec.Allow:
		return approval.ReasonAllowed
	case err == nil:
		return approval.ReasonDenied
	case errors.Is(err, approval.ErrDisconnected):
		return approval.ReasonDisconnected
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return approval.ReasonExpired
	case errors.Is(ctx.Err(), context.Canceled):
		return approval.ReasonCancelled
	default:
		return approval.ReasonDisconnected
	}
}
