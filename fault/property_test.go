package fault_test

import (
	"errors"
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/fault"
)

var allClasses = []fault.Class{
	fault.Transient, fault.Terminal, fault.NeedsApproval, fault.BudgetExceeded, fault.Cancelled,
}

// Property: New and Wrap preserve their Class through Classify, and a *Error
// stays classifiable after being wrapped by an outer fmt.Errorf (errors.As).
func TestProp_ClassifyPreservesClass(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		class := rapid.SampledFrom(allClasses).Draw(rt, "class")
		code := rapid.StringMatching(`[a-z_]{1,12}`).Draw(rt, "code")

		if got := fault.Classify(fault.New(class, code, "msg")); got != class {
			rt.Fatalf("Classify(New) = %q, want %q", got, class)
		}
		if got := fault.Classify(fault.Wrap(class, code, errors.New("cause"))); got != class {
			rt.Fatalf("Classify(Wrap) = %q, want %q", got, class)
		}
		// Survives an outer wrap.
		nested := fmt.Errorf("dispatching: %w", fault.New(class, code, "msg"))
		if got := fault.Classify(nested); got != class {
			rt.Fatalf("Classify(nested) = %q, want %q", got, class)
		}
	})
}

// Error() formats all four shapes: code+message+cause, code+cause, code+message,
// and code alone.
func TestErrorStringAllShapes(t *testing.T) {
	cause := errors.New("boom")

	// code + message + cause: the Message field is set on a wrapped error.
	withAll := fault.Wrap(fault.Transient, "io", cause)
	withAll.Message = "while reading"

	cases := []struct {
		name string
		err  *fault.Error
		want string
	}{
		{"code only", &fault.Error{Code: "bare", Class: fault.Terminal}, "bare"},
		{"code+message", fault.New(fault.Terminal, "bad_input", "missing field"), "bad_input: missing field"},
		{"code+cause", fault.Wrap(fault.Transient, "net_timeout", cause), "net_timeout: boom"},
		{"code+message+cause", withAll, "io: while reading: boom"},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Fatalf("%s: Error() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
