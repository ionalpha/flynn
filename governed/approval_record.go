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

// recordingPrompter writes an approval request onto the run's stream before the prompt
// opens and how it ended after the prompt closes, around whatever prompter resolves the
// pause. The prompter does not know it is recorded, and the approval flow that calls it is
// unchanged.
//
// Every request gets exactly one resolution record, because Prompt has one way out: the
// answer is written on every path from the inner prompter's return, whatever it returned.
// The record is the system's when the system closed the request (the grace period ran out,
// the run was cancelled, the client went away) and a person's only when the prompter
// returned an answer in time.
type recordingPrompter struct {
	inner mission.ApprovalPrompter
	sink  *spinesink.ApprovalSink
	clk   clock.Clock
}

var _ mission.ApprovalPrompter = recordingPrompter{}

// recording wraps prompter so its requests and resolutions are recorded. A nil prompter
// stays nil, because no prompter is the non-interactive answer and giving it a recorder
// would make it look interactive. A nil stack returns the prompter untouched.
func (s *approvalStack) recording(prompter mission.ApprovalPrompter) mission.ApprovalPrompter {
	if s == nil || prompter == nil {
		return prompter
	}
	return recordingPrompter{inner: prompter, sink: s.sink, clk: clock.System{}}
}

// Prompt records the request, asks the inner prompter, and records the resolution.
//
// A request that cannot be recorded is not put to the person: a prompt the record cannot
// account for would let an allow exist with no question behind it, so the failure declines.
// The same holds the other way. An allow whose resolution cannot be recorded is not
// returned as an allow, because the sealed history would show a request with no answer and
// an action that ran anyway. A denial that cannot be recorded is still a denial.
//
// An answer that arrives after ctx is done is not honoured. ctx carries the grace period,
// and a prompter that returns an allow at the moment the deadline fires has raced the
// expiry; the request was already declined as far as the record is concerned, so the
// action is too.
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
	// The resolution is written even when ctx is done, which is exactly when an expiry or
	// a cancellation needs recording.
	if werr := r.sink.Answer(context.WithoutCancel(ctx), ans); werr != nil && reason == approval.ReasonAllowed {
		return mission.ApprovalDecision{}, werr
	}
	return dec, err
}

// resolution names how a prompt came out, from the inner prompter's result and the state
// of the context it was given. The client going away is checked first because it is stated
// outright; a context that ended says why through its error.
//
// A prompter that failed for another reason is recorded as disconnected: the client that
// was asked could not answer, which is what the closed set has for it.
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
