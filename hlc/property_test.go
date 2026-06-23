package hlc_test

import (
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/hlc"
)

// TestProp_AlwaysMonotonic is the HLC's core contract: across ANY interleaving
// of Now/Observe and arbitrary physical-clock movement — including jumps
// backward (skew) — every timestamp the clock issues is strictly greater than
// the one before it. One property replaces an unbounded number of hand cases,
// and rapid shrinks any violation to a minimal reproducer.
func TestProp_AlwaysMonotonic(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		clk := clock.NewManual(time.UnixMilli(rapid.Int64Range(0, 1_000_000).Draw(rt, "start")))
		c := hlc.NewClock(hlc.WithPhysical(clk))

		var last hlc.Time
		advance := func(got hlc.Time) {
			if !last.Before(got) {
				rt.Fatalf("HLC not strictly monotonic: %v then %v", last, got)
			}
			last = got
		}

		for n := rapid.IntRange(1, 60).Draw(rt, "ops"); n > 0; n-- {
			switch rapid.IntRange(0, 3).Draw(rt, "op") {
			case 0:
				advance(c.Now())
			case 1:
				remote := hlc.Time{
					Wall:    rapid.Int64Range(0, 5_000_000).Draw(rt, "remoteWall"),
					Counter: rapid.Uint16().Draw(rt, "remoteCounter"),
				}
				advance(c.Observe(remote))
			case 2:
				clk.Advance(time.Duration(rapid.Int64Range(0, int64(time.Second)).Draw(rt, "adv")))
			case 3:
				clk.Set(time.UnixMilli(rapid.Int64Range(0, 5_000_000).Draw(rt, "set"))) // skew / rewind
			}
		}
	})
}
