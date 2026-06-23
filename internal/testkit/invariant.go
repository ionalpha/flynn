package testkit

import "github.com/ionalpha/flynn/dispatch"

// TB is the subset of testing.TB the testkit helpers use. Both *testing.T and
// *rapid.T satisfy it, so the same helpers work in ordinary unit tests and
// inside rapid property checks.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// RequireLifecycle asserts a coherent event lifecycle on a recorded stream:
// every dispatched action is either a start paired with an end, or a single
// rejection. An imbalance means an action was lost or double-recorded.
func RequireLifecycle(t TB, events []dispatch.Event) {
	t.Helper()
	var starts, ends, rejected int
	for _, e := range events {
		switch e.Type {
		case dispatch.EventStart:
			starts++
		case dispatch.EventEnd:
			ends++
		case dispatch.EventRejected:
			rejected++
		default:
			t.Fatalf("unknown dispatch event type %q", e.Type)
		}
	}
	if starts != ends {
		t.Fatalf("lifecycle imbalance: %d start vs %d end events (%d rejected)", starts, ends, rejected)
	}
}

// CountByType tallies events by their Type, for assertions on what was emitted.
func CountByType(events []dispatch.Event) map[string]int {
	counts := make(map[string]int, 3)
	for _, e := range events {
		counts[e.Type]++
	}
	return counts
}
