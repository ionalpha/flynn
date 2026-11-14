package approval

import (
	"errors"
	"strings"

	"github.com/ionalpha/flynn/internal/inertext"
)

// Outcome is what an approval request came to: the action was allowed or it was not.
type Outcome string

const (
	// OutcomeAllow admits the action the request asked about.
	OutcomeAllow Outcome = "allow"
	// OutcomeDeny keeps the action refused.
	OutcomeDeny Outcome = "deny"
)

// Reason says how a request was resolved. It is a closed set so that a reader of the
// record can branch on it, and so that no free text a peer chose can reach a place where
// code decides something from it: free text lives in a separate, labelled field.
//
// Two of the values are a person's answer and three are the system closing a request
// nobody answered. The split decides who the record names as the author of the
// resolution; see [Reason.ByPerson].
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
	// ReasonDisconnected is the client that was asked going away before it answered.
	ReasonDisconnected Reason = "disconnected"
)

// Reasons lists every value of [Reason], in the order a record documents them.
func Reasons() []Reason {
	return []Reason{ReasonAllowed, ReasonDenied, ReasonExpired, ReasonCancelled, ReasonDisconnected}
}

// Valid reports whether r is one of the closed set. A decoder uses it to refuse a value
// it does not know, rather than passing a string on that a later reader would trust.
func (r Reason) Valid() bool {
	for _, v := range Reasons() {
		if r == v {
			return true
		}
	}
	return false
}

// ByPerson reports whether a person produced this resolution. Only an answer at the
// prompt is a person's; an expiry, a cancellation and a disconnect are the system closing
// a request, and a record that named a person as the author of one would credit them with
// a decision they never made.
func (r Reason) ByPerson() bool { return r == ReasonAllowed || r == ReasonDenied }

// Outcome is the verdict the reason implies. Only [ReasonAllowed] admits the action; every
// other reason, including one this package does not know, declines it.
func (r Reason) Outcome() Outcome {
	if r == ReasonAllowed {
		return OutcomeAllow
	}
	return OutcomeDeny
}

// PreviewLimit is the most bytes a request preview carries. A preview is a line beside a
// prompt, and a bound keeps a long scope from filling the screen the person is deciding
// on.
const PreviewLimit = 2048

// Preview builds the line a client shows beside an approval prompt. It takes the action,
// the scope and the host and nothing else, on purpose: those are what the approval will
// authorize, so the person is shown what they are signing and not a description someone
// wrote about it. A model's explanation, a command's arguments or a tool's own summary
// have no way in.
//
// Each part is escaped before it is joined: a control character, an invisible format
// character or a byte that is not UTF-8 appears as a visible escape such as \x1b, so the
// line holds nothing a terminal would act on and a part that tried to hide something shows
// it. The result is cut to [PreviewLimit] bytes.
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

// quote shows a part between double quotes after escaping it, and escapes the quote
// itself, so a part cannot close the quotes early and pass the rest off as the next field.
func quote(s string) string {
	return `"` + strings.ReplaceAll(inertext.Escape(s), `"`, `\"`) + `"`
}

// ErrDisconnected is what a prompter returns when the client it asked went away before
// answering. The recorder of the request reads it as [ReasonDisconnected]; the approval
// flow reads any prompter error as a decline, so the action stays refused either way.
var ErrDisconnected = errors.New("approval: the client that was asked disconnected")
