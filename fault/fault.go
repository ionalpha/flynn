// Package fault is the agent's canonical error model: typed, wrapped errors
// classified by how a caller should react. The router, governor, and reconciler
// branch on a failure's Class, never on string matching, so behaviour stays
// consistent across processes (fleet/P2P) and through the event log and replay.
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
	// BudgetExceeded means the governor's token/cost ceiling was hit.
	BudgetExceeded Class = "budget_exceeded"
	// Cancelled means the context was cancelled or its deadline passed.
	Cancelled Class = "cancelled"
)

// Error is a classified, wrapped error. Code is a short stable identifier for
// the error kind (meaningful across processes and in the event log); Class is
// the reaction; the wrapped error, if any, is reachable via errors.Unwrap.
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

// Unwrap returns the wrapped error, if any.
func (e *Error) Unwrap() error { return e.wrapped }

// New builds a classified Error with no wrapped cause.
func New(class Class, code, message string) *Error {
	return &Error{Code: code, Class: class, Message: message}
}

// Wrap builds a classified Error around an existing cause. The cause supplies
// the message; use New when there is no underlying error to wrap.
func Wrap(class Class, code string, err error) *Error {
	return &Error{Code: code, Class: class, wrapped: err}
}

// Classify returns the Class a caller should act on. It honours an explicit
// *Error in the chain, maps context cancellation/deadline, and otherwise treats
// the failure as Terminal — unknown errors are not retried by default; retry is
// opted into by classifying explicitly.
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

// Compile-time check that Error satisfies the error interface.
var _ error = (*Error)(nil)
