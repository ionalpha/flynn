package spinesink

import (
	"context"
	"fmt"
	"time"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/internal/inertext"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// approvalRequestedType is the event type an open approval request is recorded under. It
// shares the approval.* prefix with [approvalEventType] and is written before the prompt
// opens.
const approvalRequestedType = "approval.requested"

// maxIdentifier bounds an identifier a caller supplies (a request id, a client id). They
// are minted by the host, so the bound is a backstop, not a limit anyone meets.
const maxIdentifier = 128

// Request is an approval request about to be put to a person.
type Request struct {
	// ID names the request, so the answer can say which question it answers.
	ID string
	// Action, Scope and Host are what the approval would authorize. The preview is built
	// from these three and nothing else.
	Action string
	Scope  state.Scope
	Host   string
	// Tainted reports that the request follows content the run took from outside.
	Tainted bool
	// ExpiresAt is when the request stops waiting and declines.
	ExpiresAt time.Time
}

// Answer is how a request ended.
type Answer struct {
	// RequestID names the request this resolves.
	RequestID string
	// Action, Scope and Host repeat the request, so the record reads on its own.
	Action string
	Scope  state.Scope
	Host   string
	// Reason is how it ended. It decides both the verdict and who the record names as the
	// author: a person for allowed and denied, the system for every other value.
	Reason approval.Reason
	// Grant is the narrower target the person granted, when they narrowed it.
	Grant string
	// FeedbackPresent reports that the person attached feedback to a denial. The text goes
	// to the model and is not recorded.
	FeedbackPresent bool
	// Client is the id of the attached client that answered, empty when the system closed
	// the request.
	Client string
}

// Request records that an approval request is open, as an approval.requested event
// attributed to the system. It must be called before the prompt is shown: a client that
// attaches while a prompt is open learns of it from the stream, and a request that was
// asked but never written down would be one the record could not account for.
//
// The preview is built here from the action, scope and host by [approval.Preview], so it is
// the same line whoever the writer is. The error is returned for the caller to act on; a
// caller that cannot record the request should not open the prompt.
func (s *ApprovalSink) Request(ctx context.Context, r Request) error {
	_, err := s.log.Append(ctx, spine.AppendInput{
		Stream: s.stream,
		Type:   approvalRequestedType,
		Actor:  spine.ActorSystem,
		Payload: map[string]any{
			"requestId": inertext.Limit(inertext.Escape(r.ID), maxIdentifier),
			"action":    inertext.Escape(r.Action),
			"scope":     inertext.Escape(scopeString(r.Scope)),
			"host":      inertext.Escape(r.Host),
			"preview":   approval.Preview(r.Action, scopeString(r.Scope), r.Host),
			"tainted":   r.Tainted,
			"expiresAt": r.ExpiresAt.UTC().Format(time.RFC3339Nano),
		},
	})
	return err
}

// Answer records how a request ended, as an approval.decision event that carries an
// outcome. A reason outside the closed set is refused and nothing is written: the reason
// is what a reader branches on, and a string a peer could choose must not reach one.
//
// The actor follows the reason. An answer at the prompt is a person's and is written as the
// human; an expiry, a cancellation or a disconnect is the system closing the request, and
// is written as the system, because no person decided anything.
func (s *ApprovalSink) Answer(ctx context.Context, a Answer) error {
	if !a.Reason.Valid() {
		return fmt.Errorf("spinesink: %q is not a resolution reason", string(a.Reason))
	}
	actor := spine.ActorSystem
	if a.Reason.ByPerson() {
		actor = spine.ActorHuman
	}
	_, err := s.log.Append(ctx, spine.AppendInput{
		Stream: s.stream,
		Type:   approvalEventType,
		Actor:  actor,
		Payload: map[string]any{
			"requestId":       inertext.Limit(inertext.Escape(a.RequestID), maxIdentifier),
			"action":          inertext.Escape(a.Action),
			"scope":           inertext.Escape(scopeString(a.Scope)),
			"host":            inertext.Escape(a.Host),
			"outcome":         string(a.Reason.Outcome()),
			"reason":          string(a.Reason),
			"grant":           inertext.Limit(inertext.Escape(a.Grant), approval.PreviewLimit),
			"feedbackPresent": a.FeedbackPresent,
			"client":          inertext.Limit(inertext.Escape(a.Client), maxIdentifier),
		},
	})
	return err
}
