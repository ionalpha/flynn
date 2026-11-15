package approval

import (
	"errors"
	"strings"

	"github.com/ionalpha/flynn/internal/inertext"
)

// Outcome is whether the action an approval request asked about was allowed.
type Outcome string

const (
	// OutcomeAllow admits the action the request asked about.
	OutcomeAllow Outcome = "allow"
	// OutcomeDeny keeps the action refused.
	OutcomeDeny Outcome = "deny"
)

// Reason says how a request was resolved. It is a closed set so a reader of the
// record can branch on it and no free text a peer chose reaches code that
// decides on it; free text goes in a separate, labelled field.
//
// Two values are a person's answer and three are the system closing a request
// nobody answered, which decides who the record names as author; see
// [Reason.ByPerson].
type Reason string

const (
	// ReasonAllowed is a person approving the action.
	ReasonAllowed Reason = "allowed"
	// ReasonDenied is a person refusing the action.
	ReasonDenied Reason = "denied"
	// ReasonExpired is the grace period running out with no answer. The request is
	// declined, never admitted on a missing decision.
	ReasonExpired Reason = "expired"
	// ReasonCancelled is the run being cancelled while the request was open.
	ReasonCancelled Reason = "cancelled"
	// ReasonDisconnected is the asked client going away before it answered.
	ReasonDisconnected Reason = "disconnected"
)

// Reasons lists every value of [Reason], in the order a record documents them.
func Reasons() []Reason {
	return []Reason{ReasonAllowed, ReasonDenied, ReasonExpired, ReasonCancelled, ReasonDisconnected}
}

// Valid reports whether r is in the closed set, so a decoder refuses a value it
// does not know instead of passing on a string a later reader would trust.
func (r Reason) Valid() bool {
	for _, v := range Reasons() {
		if r == v {
			return true
		}
	}
	return false
}

// ByPerson reports whether a person produced this resolution. An expiry, a
// cancellation or a disconnect is the system closing the request, and naming a
// person as its author would credit them with a decision they never made.
func (r Reason) ByPerson() bool { return r == ReasonAllowed || r == ReasonDenied }

// Outcome is the verdict the reason implies. Only [ReasonAllowed] admits the
// action; every other reason, including an unknown one, declines it.
func (r Reason) Outcome() Outcome {
	if r == ReasonAllowed {
		return OutcomeAllow
	}
	return OutcomeDeny
}

// PreviewLimit is the most bytes a request preview carries, so a long scope
// cannot fill the screen the person is deciding on.
const PreviewLimit = 2048

// Preview builds the line a client shows beside an approval prompt from the
// action, scope and host only. Those are what the approval authorizes, so the
// person sees what they sign; a model's explanation, a command's arguments or
// a tool's summary have no way in.
//
// Each part is escaped first: a control character, an invisible format
// character or a non-UTF-8 byte appears as a visible escape such as \x1b, so
// the line holds nothing a terminal would act on and nothing hidden. The result
// is cut to [PreviewLimit] bytes.
func Preview(action, scope, host string) string {
	var b strings.Builder
	b.WriteString("allow ")
	b.WriteString(quote(action))
	if scope != "" {
		b.WriteString(" in scope ")
		b.WriteString(quote(scope))
	}
	if host != "" {
		b.WriteString(" on host ")
		b.WriteString(quote(host))
	}
	return inertext.Limit(b.String(), PreviewLimit)
}

// quote escapes s, including its quotes, and wraps it in double quotes, so a
// part cannot close the quotes early and pass the rest off as the next field.
func quote(s string) string {
	return `"` + strings.ReplaceAll(inertext.Escape(s), `"`, `\"`) + `"`
}

// ErrDisconnected is what a prompter returns when the asked client went away.
// The request's recorder reads it as [ReasonDisconnected]; the approval flow
// reads any prompter error as a decline, so the action stays refused either
// way.
var ErrDisconnected = errors.New("approval: the client that was asked disconnected")
