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

// Stop classifies how a user prompt ended. A reader branches on it, so a value
// outside the closed set is a malformed record; [Stop.Valid] reports it.
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
	// StopKilled is the work being stopped from outside, with no chance to wind
	// down.
	StopKilled Stop = "killed"
	// StopError is any other failure, including a panic.
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

// ClientAttachment describes one client connecting to a session. Everything
// but the claimed name and version was observed or negotiated by the host. A
// client can claim anything, so those two are kept for a human reader and no
// decision reads them. Attested is false in every record; a stored record with
// it true is damaged.
type ClientAttachment struct {
	// ID is the host's identifier for the connection. Decisions name a client by
	// this and never by its claimed name.
	ID string `json:"id"`
	// Major is the negotiated protocol major version.
	Major int `json:"major"`
	// Capabilities are the optional features granted to this connection.
	Capabilities []string `json:"capabilities,omitempty"`
	// Transport is how the client connected: stdio, a unix socket, and so on.
	Transport string `json:"transport,omitempty"`
	// PeerPID is the peer's process id where the OS reports one, else 0.
	PeerPID int `json:"peerPid,omitempty"`

	ClaimedName string `json:"claimedName,omitempty"`

	ClaimedVersion string `json:"claimedVersion,omitempty"`
	// Attested is false: nothing verified the claimed identity.
	Attested bool `json:"attested"`
}

// maxClaimLen bounds a claimed name or version, which is peer-supplied text.
const maxClaimLen = 128

// sanitized makes the peer-supplied text inert and bounded and sets Attested
// false, whatever the caller or a stored record said.
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

// Ceiling is a spend limit in tokens and cost, with the meanings of the budget
// limit's fields. A zero field has no ceiling.
type Ceiling struct {
	Tokens int64   `json:"tokens,omitempty"`
	Cost   float64 `json:"cost,omitempty"`
}

// BudgetExtension is a person raising the session's ceiling. Total sums every
// raise including this one, so the record shows the distance from the
// session's starting ceiling.
type BudgetExtension struct {
	Old   Ceiling `json:"old"`
	New   Ceiling `json:"new"`
	Total Ceiling `json:"total"`
}

var errNotRaised = errors.New("session: a budget extension must raise the ceiling and lower neither limit")

// raise returns the amount next raises old by. A change that lowers either
// limit, leaves both unchanged, or is not finite is refused, so a lowered
// ceiling cannot be recorded as an extension.
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
	// Reason is a short explanation, made inert and bounded before recording.
	Reason string

	Steps int
	// Usage is what the prompt spent, if known.
	Usage *Usage
}

const maxReasonLen = 512

// RunPrompt runs one user prompt and writes its prompt.ended record once on
// every way out of run: a returned end, an error, cancellation, a panic or a
// goroutine exit. A prompt with no ending leaves a client waiting on it, so the
// guarantee is made here, where every prompt passes, instead of in each caller
// exit path.
//
// When run returns an error the prompt ends as [StopCancelled] if ctx was
// cancelled and [StopError] otherwise. A panic ends it as [StopError] with a
// fixed reason and keeps panicking. The ending is written with a context that
// ignores ctx's cancellation, because a cancelled prompt most needs its ending
// recorded. A second call with an ended id runs run and records nothing.
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

// EndPrompt writes the prompt.ended record for id and reports whether it wrote
// one. It writes at most one per id per session, so a cancellation racing a
// normal finish cannot close a prompt twice. An empty id is refused.
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

// AttachClient records a client connecting. The claimed name and version are
// marked unattested however the caller set them.
func (s *Session) AttachClient(ctx context.Context, c ClientAttachment) {
	att := c.sanitized()
	s.emit(ctx, Event{Kind: KindClientAttached, Actor: spine.ActorSystem, Client: att.ID, Attachment: &att})
}

// ExtendBudget records a person raising the ceiling from old to next and adds
// the raise to the total. It refuses anything but a raise. The record is
// written with the human actor, so the caller must hold a person's decision.
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

// sanitized makes the peer-supplied text on a decoded default-path event
// inert. It touches only fields a peer can influence, so other event kinds
// decode unchanged.
func (e Event) sanitized() Event {
	if e.Attachment != nil {
		att := e.Attachment.sanitized()
		e.Attachment = &att
	}
	return e
}

// approvalRequestedEvent reads an approval.requested record. The preview is
// escaped and bounded again, so a record from any writer reaches a client as
// inert text.
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

// approvalDecisionEvent reads an approval.decision record. The gate writes its
// verdict with a verdict key and no request; the sink's Answer writes a
// request's end with an outcome key, which tells them apart. A resolution
// allows only when both outcome and reason say so; anything else is a denial.
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

// payloadText reads a string payload key and makes it inert. A missing key or
// a value of another type reads as "".
func payloadText(se spine.Event, key string) string {
	s, _ := se.Payload[key].(string)
	return inertext.Escape(s)
}
