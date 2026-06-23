package testkit

import (
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ionalpha/flynn/dispatch"
)

// DiffEvents fails the test with a readable diff if two dispatch-event streams
// differ. Because the dispatcher stamps time from an injectable clock, two runs
// of the same scenario under a clock.Manual produce byte-identical streams —
// this is the basis of golden/replay assertions and determinism checks. A nil
// stream and an empty one are treated as equal (both mean "no events").
func DiffEvents(t TB, want, got []dispatch.Event) {
	t.Helper()
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("event streams differ (-want +got):\n%s", diff)
	}
}
