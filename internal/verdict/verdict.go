// Package verdict is the result of a check that may not have been able to run.
//
// A bool has two answers and a check has three. A check that ran and failed says
// the thing checked is wrong; a check that could not run says nothing about it and
// reports on the host, the network or the credentials instead. Collapsing the
// third answer into false has made an unrunnable check count as non-convergence, a
// partial read count as a complete one, and an unreadable file count as an absent
// one. [Verdict] carries the third answer, and [Outcome] is an enum so the
// exhaustive linter makes every switch over it handle [Unknown].
//
// The zero value is [Unknown], so a verdict nobody set never reads as a pass or a
// fail. A gate test fails a switch over [Outcome] that has a default clause, since
// the linter counts a default as handling every value and a default is where
// unknown gets folded back into one of the other two.
package verdict

import (
	"errors"
	"fmt"
)

// Outcome is which of the three answers a check gave.
type Outcome int

const (
	// Unknown means the check could not be evaluated. It is the zero value.
	Unknown Outcome = iota
	// Passed means the check ran and the thing checked is right.
	Passed
	// Failed means the check ran and the thing checked is wrong.
	Failed
)

// String names the outcome for logs and messages.
func (o Outcome) String() string {
	switch o {
	case Unknown:
		return "unknown"
	case Passed:
		return "passed"
	case Failed:
		return "failed"
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}

// Verdict is one check's answer, with the reason for a failure and the cause of an
// unknown.
type Verdict struct {
	outcome Outcome
	reason  string
	cause   error
}

// errNoCause stands in for the cause of an [Unevaluated] verdict built from a nil
// error, so [Verdict.Err] is non-nil exactly when the outcome is [Unknown].
var errNoCause = errors.New("verdict: the check could not be evaluated and gave no cause")

// Pass is a check that ran and passed.
func Pass() Verdict { return Verdict{outcome: Passed} }

// Fail is a check that ran and failed for reason.
func Fail(reason string) Verdict { return Verdict{outcome: Failed, reason: reason} }

// Unevaluated is a check that could not run because of cause.
func Unevaluated(cause error) Verdict {
	if cause == nil {
		cause = errNoCause
	}
	return Verdict{outcome: Unknown, cause: cause}
}

// Outcome is which answer the check gave.
func (v Verdict) Outcome() Outcome { return v.outcome }

// Reason says why: the failure's reason, the cause's message for an unknown, and ""
// for a pass.
func (v Verdict) Reason() string {
	switch v.outcome {
	case Passed:
		return ""
	case Failed:
		return v.reason
	case Unknown:
		return v.Err().Error()
	}
	return ""
}

// Err is the cause of an [Unknown] verdict, for wrapping with %w, and nil for one
// that ran. A zero Verdict reports [errNoCause].
func (v Verdict) Err() error {
	if v.outcome != Unknown {
		return nil
	}
	if v.cause == nil {
		return errNoCause
	}
	return v.cause
}

// String renders the verdict for a log line.
func (v Verdict) String() string {
	if r := v.Reason(); r != "" {
		return v.outcome.String() + ": " + r
	}
	return v.outcome.String()
}
