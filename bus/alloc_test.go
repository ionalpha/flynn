//go:build !race

// Allocation ceiling for the Publish match path, in the style of jobs/alloc_test.go:
// the ceiling is the measured cost plus headroom, lowered when the cost drops,
// never raised to absorb a regression. Excluded under -race (instrumentation skews
// allocation counts); dev/bench and the CI bench job run it. The guarantee under
// test: a publish's match cost is one subject split regardless of subscriber count,
// because each subscription's pattern tokens are split once at Subscribe and the
// scan over them is alloc-free.

package bus

import (
	"strings"
	"testing"
)

// matchLoopAllocs models one publish: split the subject once, then match it against
// n subscriptions' pre-split pattern tokens (the shape memory.go's Publish loop uses).
func matchLoopAllocs(n int) float64 {
	const subject = "run.step.completed"
	patterns := make([][]string, n)
	for i := range patterns {
		patterns[i] = strings.Split("run.*.completed", ".")
	}
	return testing.AllocsPerRun(100, func() {
		st := strings.Split(subject, ".")
		for _, pt := range patterns {
			_ = matchTokens(pt, st)
		}
	})
}

func TestAllocCeilingMatchConstantInSubscribers(t *testing.T) {
	small := matchLoopAllocs(10)
	large := matchLoopAllocs(1000)
	if small != large {
		t.Errorf("publish match allocates %.0f/op over 10 subscribers but %.0f/op over 1000: match cost is scaling with the subscriber count", small, large)
	}
	if large > 1 {
		t.Errorf("publish match allocates %.0f/op, over the 1 ceiling (one subject split per publish): a match-path regression (or lower the ceiling if the cost legitimately grew)", large)
	}
}
