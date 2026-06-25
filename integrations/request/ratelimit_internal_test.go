package request

import (
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/clock"
)

// A disabled limiter (zero interval) never makes a caller wait.
func TestReserve_DisabledNeverWaits(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0).UTC())
	h := &hostLimiters{clk: clk, next: map[string]time.Time{}}
	for i := range 5 {
		if w := h.reserve("example.test"); w != 0 {
			t.Fatalf("reserve #%d = %v, want 0 when disabled", i, w)
		}
	}
}

// Property: with the clock held still, successive reservations for one host are
// spaced exactly interval apart (the first is immediate), so a burst queues into an
// evenly paced sequence rather than firing all at once. Reservations for different
// hosts are independent.
func TestProp_ReserveSpacesByInterval(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		intervalMs := rapid.IntRange(1, 1000).Draw(rt, "intervalMs")
		interval := time.Duration(intervalMs) * time.Millisecond
		n := rapid.IntRange(1, 6).Draw(rt, "n")

		clk := clock.NewManual(time.Unix(0, 0).UTC())
		h := &hostLimiters{clk: clk, interval: interval, next: map[string]time.Time{}}

		for i := range n {
			wantA := time.Duration(i) * interval
			if got := h.reserve("a.test"); got != wantA {
				rt.Fatalf("host a reserve #%d = %v, want %v", i, got, wantA)
			}
			// A second host keeps its own schedule, unaffected by host a.
			wantB := time.Duration(i) * interval
			if got := h.reserve("b.test"); got != wantB {
				rt.Fatalf("host b reserve #%d = %v, want %v", i, got, wantB)
			}
		}
	})
}

// As the clock advances past a host's reserved slot, the next reservation is again
// immediate: an idle host is not penalised for prior bursts.
func TestReserve_AdvancingClockClearsBacklog(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0).UTC())
	h := &hostLimiters{clk: clk, interval: time.Second, next: map[string]time.Time{}}

	if w := h.reserve("example.test"); w != 0 {
		t.Fatalf("first reserve = %v, want 0", w)
	}
	clk.Advance(5 * time.Second) // host has been idle well past its slot
	if w := h.reserve("example.test"); w != 0 {
		t.Fatalf("reserve after idle = %v, want 0", w)
	}
}
