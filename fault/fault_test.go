package fault_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ionalpha/flynn/fault"
)

func TestErrorStringAndUnwrap(t *testing.T) {
	cause := errors.New("boom")
	e := fault.Wrap(fault.Transient, "net_timeout", cause)
	if !errors.Is(e, cause) {
		t.Fatal("Wrap must keep the cause reachable via errors.Is")
	}
	// The cause supplies the message; the code is not duplicated.
	if got := e.Error(); got != "net_timeout: boom" {
		t.Fatalf("Error() = %q, want %q", got, "net_timeout: boom")
	}

	// New (no wrapped cause) reads code: message.
	n := fault.New(fault.Terminal, "bad_input", "missing field")
	if got := n.Error(); got != "bad_input: missing field" {
		t.Fatalf("Error() = %q, want %q", got, "bad_input: missing field")
	}
}

func TestClassify(t *testing.T) {
	if got := fault.Classify(nil); got != "" {
		t.Fatalf("Classify(nil) = %q, want empty", got)
	}

	e := fault.New(fault.BudgetExceeded, "over_budget", "stop")
	if got := fault.Classify(e); got != fault.BudgetExceeded {
		t.Fatalf("Classify(direct) = %q, want budget_exceeded", got)
	}

	wrapped := fmt.Errorf("dispatching action: %w", e)
	if got := fault.Classify(wrapped); got != fault.BudgetExceeded {
		t.Fatalf("Classify(wrapped) = %q, want budget_exceeded", got)
	}

	if got := fault.Classify(context.Canceled); got != fault.Cancelled {
		t.Fatalf("Classify(context.Canceled) = %q, want cancelled", got)
	}

	if got := fault.Classify(errors.New("mystery")); got != fault.Terminal {
		t.Fatalf("Classify(unknown) = %q, want terminal (no blind retry)", got)
	}
}
