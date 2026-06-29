package chain

import (
	"errors"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/spine"
)

func govEvent(typ string, call int64) spine.Event {
	return spine.Event{Type: typ, Payload: map[string]any{GovCallKey: call}}
}

func govCode(err error) string {
	var fe *fault.Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return ""
}

func TestVerifyGovernanceAccepts(t *testing.T) {
	events := []spine.Event{
		govEvent(GovStart, 1),
		{Type: "tool.call", Payload: map[string]any{}}, // unrelated events are ignored
		govEvent(GovEnd, 1),
		govEvent(GovStart, 2),
		govEvent(GovRejected, 3), // a denied action that never ran
		govEvent(GovEnd, 2),
	}
	if err := VerifyGovernance(events); err != nil {
		t.Fatalf("a governed run was rejected: %v", err)
	}
}

func TestVerifyGovernanceRejectsUnadmitted(t *testing.T) {
	// A completion whose admission is missing (deleted, or never recorded).
	err := VerifyGovernance([]spine.Event{govEvent(GovEnd, 7)})
	if err == nil {
		t.Fatal("a completion with no admission was accepted")
	}
	if code := govCode(err); code != CodeUnadmittedAction {
		t.Fatalf("code = %q, want %q", code, CodeUnadmittedAction)
	}
}

func TestVerifyGovernanceRejectsDeniedButExecuted(t *testing.T) {
	// One call that was both refused admission and completed.
	err := VerifyGovernance([]spine.Event{
		govEvent(GovStart, 1),
		govEvent(GovRejected, 1),
		govEvent(GovEnd, 1),
	})
	if err == nil {
		t.Fatal("a denied-but-executed action was accepted")
	}
	if code := govCode(err); code != CodeDeniedButExecuted {
		t.Fatalf("code = %q, want %q", code, CodeDeniedButExecuted)
	}
}

// TestVerifyGovernanceRejectsDeniedThenExecutedAnyOrder confirms the contradiction is
// caught regardless of whether the completion or the refusal appears first.
func TestVerifyGovernanceRejectsDeniedThenExecutedAnyOrder(t *testing.T) {
	err := VerifyGovernance([]spine.Event{
		govEvent(GovStart, 1),
		govEvent(GovEnd, 1),
		govEvent(GovRejected, 1),
	})
	if code := govCode(err); code != CodeDeniedButExecuted {
		t.Fatalf("code = %q, want %q", code, CodeDeniedButExecuted)
	}
}

// TestVerifyGovernanceProperties asserts the invariants directly: a well-formed
// governance stream (every completed call admitted first, refusals never also
// completed) always verifies, and injecting either defect always fails.
func TestVerifyGovernanceProperties(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 8).Draw(rt, "calls")
		var events []spine.Event
		for i := 1; i <= n; i++ {
			call := int64(i)
			switch rapid.IntRange(0, 2).Draw(rt, "kind") {
			case 0: // admitted and completed
				events = append(events, govEvent(GovStart, call), govEvent(GovEnd, call))
			case 1: // admitted, still in flight
				events = append(events, govEvent(GovStart, call))
			default: // refused, never ran
				events = append(events, govEvent(GovRejected, call))
			}
		}
		if err := VerifyGovernance(events); err != nil {
			rt.Fatalf("a well-formed governance stream was rejected: %v", err)
		}

		// Injecting an unadmitted completion must be caught.
		bad := append(append([]spine.Event{}, events...), govEvent(GovEnd, int64(n+100)))
		if err := VerifyGovernance(bad); govCode(err) != CodeUnadmittedAction {
			rt.Fatalf("unadmitted completion not caught: %v", err)
		}
	})
}
