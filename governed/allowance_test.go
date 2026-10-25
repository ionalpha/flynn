package governed

import "testing"

// TestOutsideGateIsAbsentWhenNothingIsMarked: a run with no marked action carries no gate
// rather than one that is installed and marks nothing. Both admit everything; only one of
// them claims a gate is there, and the posture reports the claim.
func TestOutsideGateIsAbsentWhenNothingIsMarked(t *testing.T) {
	if got := outsideGate(nil); got != nil {
		t.Errorf("gate = %v, want none when nothing is marked", got)
	}
	if got := outsideGate([]string{" "}); got != nil {
		t.Errorf("gate = %v, want none when only blanks are marked", got)
	}
	if outsideGate([]string{"deploy"}) == nil {
		t.Error("no gate was built for a marked action")
	}
}

// TestAllowanceOptionsInstallTheGateOnlyWhenMarked: the mission option that installs the
// gate exists exactly when an action is marked.
func TestAllowanceOptionsInstallTheGateOnlyWhenMarked(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outside []string
		want    int
	}{
		{"nothing marked", nil, 0},
		{"only blanks", []string{" ", ""}, 0},
		{"one action", []string{"deploy"}, 1},
		{"a duplicate collapses", []string{"deploy", "deploy"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(allowanceOptions(tc.outside)); got != tc.want {
				t.Errorf("options = %d, want %d", got, tc.want)
			}
		})
	}
}
