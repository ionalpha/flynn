// Package fault is the agent's error model: wrapped errors classified by how a
// caller should react. The router, governor and reconciler branch on Class, so
// behaviour is the same across processes and through the event log and replay.
package fault

import (
	"context"
	"errors"
	"fmt"
)

// Class is how a caller should react to a failure.
type Class string

const (
	// Transient failures are worth retrying (network blips, rate limits, 5xx).
	Transient Class = "transient"
	// Terminal failures must not be retried (bad input, 4xx, programmer error).
	Terminal Class = "terminal"
	// NeedsApproval means a governance gate paused the action for a human.
	NeedsApproval Class = "needs_approval"
	// BudgetExceeded means the governor's token or cost ceiling was hit.
	BudgetExceeded Class = "budget_exceeded"
	// Forbidden means a gate denied the action because it is outside the run's
	// grant; it must not be retried or escalated.
	Forbidden Class = "forbidden"
	// Cancelled means the context was cancelled or its deadline passed.
	Cancelled Class = "cancelled"
)

// Error is a classified, wrapped error. Code is a short identifier for the kind,
// stable across processes and in the event log; Class is the reaction.
type Error struct {
	Code    string
	Class   Class
	Message string
	wrapped error
}

// Error implements error.
func (e *Error) Error() string {
	switch {
	case e.Message != "" && e.wrapped != nil:
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.wrapped)
	case e.wrapped != nil:
		return fmt.Sprintf("%s: %v", e.Code, e.wrapped)
	case e.Message != "":
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	default:
		return e.Code
	}
}

// Unwrap returns the wrapped cause, if any.
func (e *Error) Unwrap() error { return e.wrapped }

// New builds a classified Error with no cause.
func New(class Class, code, message string) *Error {
	return &Error{Code: code, Class: class, Message: message}
}

// Wrap builds a classified Error whose message is the cause's.
func Wrap(class Class, code string, err error) *Error {
	return &Error{Code: code, Class: class, wrapped: err}
}

// Classify returns the Class a caller should act on: an *Error in the chain wins,
// context cancellation and deadline map to Cancelled, and anything else is
// Terminal, so an error is retried only when it was classified for it.
func Classify(err error) Class {
	if err == nil {
		return ""
	}
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Class
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return Cancelled
	default:
		return Terminal
	}
}

// CodeOf returns the kind's identifier, or "" when the chain has no classified
// error. A recorded refusal needs it beside the class: "forbidden" says a rule
// refused, "capability_denied" says which, so a reader can tell one rule hit
// repeatedly from several.
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return ""
}

var _ error = (*Error)(nil)
