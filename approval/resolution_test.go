package approval_test

import (
	"testing"

	"github.com/ionalpha/flynn/approval"
)

func TestReasonTable(t *testing.T) {
	tests := []struct {
		reason   approval.Reason
		valid    bool
		byPerson bool
		outcome  approval.Outcome
	}{
		{approval.ReasonAllowed, true, true, approval.OutcomeAllow},
		{approval.ReasonDenied, true, true, approval.OutcomeDeny},
		{approval.ReasonExpired, true, false, approval.OutcomeDeny},
		{approval.ReasonCancelled, true, false, approval.OutcomeDeny},
		{approval.ReasonDisconnected, true, false, approval.OutcomeDeny},
		{"", false, false, approval.OutcomeDeny},
		{"Allowed", false, false, approval.OutcomeDeny},
		{"timeout", false, false, approval.OutcomeDeny},
		{"allowed ", false, false, approval.OutcomeDeny},
		{"approved", false, false, approval.OutcomeDeny},
	}
	for _, tc := range tests {
		t.Run(string(tc.reason), func(t *testing.T) {
			if got := tc.reason.Valid(); got != tc.valid {
				t.Errorf("Valid = %v, want %v", got, tc.valid)
			}
			if got := tc.reason.ByPerson(); got != tc.byPerson {
				t.Errorf("ByPerson = %v, want %v", got, tc.byPerson)
			}
			if got := tc.reason.Outcome(); got != tc.outcome {
				t.Errorf("Outcome = %q, want %q", got, tc.outcome)
			}
		})
	}
}

// TestReasonsIsTheClosedSet pins the set to the five values the record documents, so
// adding a sixth is a deliberate edit to this test and not a side effect.
func TestReasonsIsTheClosedSet(t *testing.T) {
	want := []string{"allowed", "denied", "expired", "cancelled", "disconnected"}
	got := approval.Reasons()
	if len(got) != len(want) {
		t.Fatalf("Reasons = %v, want %v", got, want)
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Fatalf("Reasons[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
