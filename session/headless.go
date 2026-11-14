package session

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/internal/inertext"
	"github.com/ionalpha/flynn/spine"
)

// Stop classifies how a user prompt ended. The set is closed: a reader branches on it, so
// a value outside it is a malformed record and [Stop.Valid] says so.
type Stop string

const (
	// StopConverged is the prompt reaching its goal.
	StopConverged Stop = "converged"
	// StopStalled is the prompt failing terminally or running out of steps.
	StopStalled Stop = "stalled"
	// StopCancelled is the prompt's context being cancelled.
	StopCancelled Stop = "cancelled"
	// StopBudgetExceeded is the session's spend ceiling stopping the prompt.
	StopBudgetExceeded Stop = "budget_exceeded"
	// StopKilled is the work being stopped from outside, with no chance to wind down.
	StopKilled Stop = "killed"
	// StopError is a failure that is not one of the others, including a panic.
	StopError Stop = "error"
)

// Stops lists every value of [Stop].
func Stops() []Stop {
	return []Stop{StopConverged, StopStalled, StopCancelled, StopBudgetExceeded, StopKilled, StopError}
}

// Valid reports whether s is one of the closed set.
func (s Stop) Valid() bool {
	for _, v := range Stops() {
		if s == v {
			return true
		}
	}
	return false
}

// ClientAttachment describes one client connecting to a session.
//
// Everything except the claimed identity is something the host observed or negotiated:
// the protocol major both sides agreed on, the capabilities granted, the transport the
// connection arrived on and, where the operating system reports it, the process on the
// other end. The claimed name and version are what the client said about itself. A client
// can say anything, so they are kept for a human reading the record and no decision reads
// them; Attested is false in every record, and a reader that finds it true in a stored one
// should treat the record as damaged.
type ClientAttachment struct {
	// ID is the host's own identifier for the connection. A decision names a client by
	// this and never by what the client called itself.
	ID string `json:"id"`
	// Major is the protocol major version negotiated.
	Major int `json:"major"`
	// Capabilities are the optional features granted for this connection.
	Capabilities []string `json:"capabilities,omitempty"`
	// Transport names how the client connected: stdio, a unix socket, and so on.
	Transport string `json:"transport,omitempty"`
	// PeerPID is the process id of the peer where the operating system reports one, and 0
	// where it does not.
	PeerPID int `json:"peerPid,omitempty"`
	// ClaimedName is the name the client gave itself.
	ClaimedName string `json:"claimedName,omitempty"`
	// ClaimedVersion is the version the client gave itself.
	ClaimedVersion string `json:"claimedVersion,omitempty"`
	// Attested is false: nothing verified the claimed identity.
	Attested bool `json:"attested"`
}

// maxClaimLen bounds a claimed name or version, which is peer-supplied text.
const maxClaimLen = 128

// sanitized returns c with the peer-supplied text made inert and bounded, and Attested
// false whatever the caller or a stored record said.
func (c ClientAttachment) sanitized() ClientAttachment {
	c.ID = inertext.Limit(inertext.Escape(c.ID), maxClaimLen)
	c.Transport = inertext.Limit(inertext.Escape(c.Transport), maxClaimLen)
	c.ClaimedName = inertext.Limit(inertext.Escape(c.ClaimedName), maxClaimLen)
	c.ClaimedVersion = inertext.Limit(inertext.Escape(c.ClaimedVersion), maxClaimLen)
	caps := make([]string, 0, len(c.Capabilities))
	for _, name := range c.Capabilities {
		caps = append(caps, inertext.Limit(inertext.Escape(name), maxClaimLen))
	}
	c.Capabilities = caps
	if len(caps) == 0 {
		c.Capabilities = nil
	}
	c.Attested = false
	return c
}

// Ceiling is a spend limit: tokens and cost, each with the same meaning as the matching
// field of a budget limit. A zero field means that dimension has no ceiling.
type Ceiling struct {
	Tokens int64   `json:"tokens,omitempty"`
	Cost   float64 `json:"cost,omitempty"`
}

// BudgetExtension is a person raising the session's ceiling. Total is the sum of every
// raise so far, this one included, so the record shows how far the ceiling has moved from
// where the session began and not only from the last step.
type BudgetExtension struct {
	Old   Ceiling `json:"old"`
	New   Ceiling `json:"new"`
	Total Ceiling `json:"total"`
}

// errNotRaised is why a ceiling change is refused: an extension only ever raises.
var errNotRaised = errors.New("session: a budget extension must raise the ceiling and lower neither limit")

// raise checks that next is a raise over old and returns the amount. A ceiling only moves
// up: a change that lowers either limit, leaves both where they were, or is not a finite
// number is refused, so the record can never show a person "extending" a budget downward
// and a lowered ceiling cannot be dressed as one.
func raise(old, next Ceiling) (Ceiling, error) {
	if old.Tokens < 0 || next.Tokens < old.Tokens ||
		math.IsNaN(old.Cost) || math.IsNaN(next.Cost) || math.IsInf(old.Cost, 0) || math.IsInf(next.Cost, 0) ||
		old.Cost < 0 || next.Cost < old.Cost {
		return Ceiling{}, errNotRaised
	}
	if next.Tokens == old.Tokens && next.Cost == old.Cost {
		return Ceiling{}, errNotRaised
	}
	return Ceiling{Tokens: next.Tokens - old.Tokens, Cost: next.Cost - old.Cost}, nil
}

// PromptEnd is how one user prompt ended.
type PromptEnd struct {
	// Stop classifies the ending. A value outside the closed set is recorded as
	// [StopError].
	Stop Stop
	// Reason is a short explanation. It is made inert and bounded before it is recorded.
	Reason string
	// Steps is the number of steps the prompt took.
	Steps int
	// Usage is what the prompt spent, if known.
	Usage *Usage
}

// maxReasonLen bounds the free-text reason on a prompt ending.
const maxReasonLen = 512

// RunPrompt runs one user prompt and writes its prompt.ended record exactly once, on every
// way out of run: it returns an end, it returns an error, its context is cancelled, it
// panics, or its goroutine exits. A prompt that wrote no ending would leave a client
// waiting on a prompt that is already over, so the guarantee is made here, in the one
// place every prompt passes through, and not left to each exit path in the caller.
//
// run reports how the prompt went by returning a [PromptEnd]. When run returns an error
// instead, the prompt ends as [StopCancelled] if ctx was cancelled and as [StopError]
// otherwise. A panic ends it as [StopError] with a fixed reason and then continues to
// panic. The ending is written with a context that ignores ctx's cancellation, because
// the cancelled prompt is the one that most needs its ending recorded.
//
// A second call with an id that already has an ending runs run but records nothing more.
func (s *Session) RunPrompt(ctx context.Context, id string, run func(context.Context) (PromptEnd, error)) (end PromptEnd, err error) {
	finished := false
	defer func() {
		if r := recover(); r != nil {
			s.EndPrompt(ctx, id, PromptEnd{Stop: StopError, Reason: "the prompt panicked"})
			panic(r)
		}
		if !finished {
			s.EndPrompt(ctx, id, PromptEnd{Stop: StopError, Reason: "the prompt's goroutine exited early"})
		}
	}()

	end, err = run(ctx)
	finished = true
	switch {
	case err != nil && ctx.Err() != nil:
		end = PromptEnd{Stop: StopCancelled, Reason: "the prompt was cancelled"}
	case err != nil:
		end = PromptEnd{Stop: StopError, Reason: err.Error()}
	}
	s.EndPrompt(ctx, id, end)
	return end, err
}

// EndPrompt writes the prompt.ended record for the prompt id and reports whether it wrote
// one. It writes at most one per id for the life of the session; a later call for the
// same id is dropped, so a cancellation racing a normal finish cannot close a prompt
// twice. An empty id is refused.
func (s *Session) EndPrompt(ctx context.Context, id string, end PromptEnd) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	if _, dup := s.ended[id]; dup {
		s.mu.Unlock()
		return false
	}
	if s.ended == nil {
		s.ended = make(map[string]struct{})
	}
	s.ended[id] = struct{}{}
	s.mu.Unlock()

	stop := end.Stop
	if !stop.Valid() {
		stop = StopError
	}
	s.emit(context.WithoutCancel(ctx), Event{
		Kind:     KindPromptEnded,
		Actor:    spine.ActorSystem,
		PromptID: inertext.Limit(inertext.Escape(id), maxClaimLen),
		Stop:     stop,
		Reason:   inertext.Limit(inertext.Escape(end.Reason), maxReasonLen),
		Steps:    end.Steps,
		Usage:    end.Usage,
	})
	return true
}

// AttachClient records a client connecting. The record is the system's, and the claimed
// name and version in it are marked unattested however the caller set them.
func (s *Session) AttachClient(ctx context.Context, c ClientAttachment) {
	att := c.sanitized()
	s.emit(ctx, Event{Kind: KindClientAttached, Actor: spine.ActorSystem, Client: att.ID, Attachment: &att})
}

// ExtendBudget records a person raising the session's ceiling from old to next, and adds
// the raise to the running total. It refuses a change that is not a raise; see
// [BudgetExtension]. The record is the person's: it is written with the human actor, so
// the caller is expected to have a person's decision in hand.
func (s *Session) ExtendBudget(ctx context.Context, old, next Ceiling) error {
	delta, err := raise(old, next)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.raised.Tokens += delta.Tokens
	s.raised.Cost += delta.Cost
	total := s.raised
	s.mu.Unlock()
	s.emit(ctx, Event{
		Kind:   KindBudgetExtended,
		Actor:  spine.ActorHuman,
		Budget: &BudgetExtension{Old: old, New: next, Total: total},
	})
	return nil
}

// sanitized makes the peer-supplied text on a decoded default-path event inert. Only the
// fields a peer can influence are touched, so an existing event kind decodes exactly as
// before.
func (e Event) sanitized() Event {
	if e.Attachment != nil {
		att := e.Attachment.sanitized()
		e.Attachment = &att
	}
	return e
}

// approvalRequestedEvent projects an approval.requested record written by the approval
// sink. The preview is escaped and bounded again on the way out, so a record written by
// anything other than the sink still reaches a client as inert text.
func approvalRequestedEvent(se spine.Event) Event {
	e := Event{Kind: KindApprovalRequested}
	e.RequestID = payloadText(se, "requestId")
	e.Action = payloadText(se, "action")
	e.Scope = payloadText(se, "scope")
	e.Host = payloadText(se, "host")
	e.Preview = inertext.Limit(inertext.Escape(payloadText(se, "preview")), approval.PreviewLimit)
	e.Tainted, _ = se.Payload["tainted"].(bool)
	if s, ok := se.Payload["expiresAt"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			e.ExpiresAt = t
		}
	}
	return e
}

// approvalDecisionEvent projects an approval.decision record. Two writers share the type.
// The approval gate records its verdict before any prompt opens, with a verdict key and no
// request; the approval sink's Answer records how a request ended, with an outcome key.
// The outcome key tells them apart.
//
// A resolution is read fail-closed: it allows only when both the outcome and the reason
// say so, and any other combination is a denial.
func approvalDecisionEvent(se spine.Event) Event {
	e := Event{Kind: KindApprovalDecision}
	e.Action = payloadText(se, "action")
	e.Scope = payloadText(se, "scope")
	e.Host = payloadText(se, "host")
	if _, resolution := se.Payload["outcome"]; !resolution {
		e.Grant = payloadText(se, "detail")
		e.Outcome = approval.OutcomeDeny
		if v, _ := se.Payload["verdict"].(string); v == "granted" {
			e.Outcome = approval.OutcomeAllow
		}
		e.Text = inertext.Limit(inertext.Escape(payloadText(se, "reason")), maxReasonLen)
		return e
	}
	e.RequestID = payloadText(se, "requestId")
	e.Grant = payloadText(se, "grant")
	e.Client = payloadText(se, "client")
	e.FeedbackPresent, _ = se.Payload["feedbackPresent"].(bool)
	e.Reason = payloadText(se, "reason")
	e.Outcome = approval.OutcomeDeny
	if o, _ := se.Payload["outcome"].(string); approval.Outcome(o) == approval.OutcomeAllow && approval.Reason(e.Reason) == approval.ReasonAllowed {
		e.Outcome = approval.OutcomeAllow
	}
	return e
}

// payloadText reads a string payload key and makes it inert. A durable log hands a
// payload back as decoded JSON, so a missing key or a value of another type reads as the
// empty string.
func payloadText(se spine.Event, key string) string {
	s, _ := se.Payload[key].(string)
	return inertext.Escape(s)
}
