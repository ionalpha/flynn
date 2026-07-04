//go:build !race

// Allocation ceilings for the per-turn transcript prune, in the style of
// chain/alloc_test.go: the ceiling is the measured cost, and it must hold as the
// transcript grows so the pass stays flat rather than adding O(history) allocation to
// every turn over a goal. Excluded under -race (instrumentation skews allocation
// counts); dev/bench and the CI bench job run it. The guarantee under test: the
// common turn, whose tool results are all small, prunes with zero allocation because
// pruneTranscript skips its map-building and hashing when nothing is prunable.

package mission

import (
	"fmt"
	"testing"

	"github.com/ionalpha/flynn/llm"
)

func TestPruneCommonCaseZeroAlloc(t *testing.T) {
	build := func(pairs int) []llm.Message {
		msgs := make([]llm.Message, 0, pairs*2)
		for i := range pairs {
			id := fmt.Sprintf("t%d", i)
			msgs = append(msgs, callMsg(id, "read"), resultMsg(id, "small result", false))
		}
		return msgs
	}
	for _, pairs := range []int{50, 100} {
		msgs := build(pairs)
		allocs := testing.AllocsPerRun(200, func() {
			_ = pruneTranscript(msgs, noSummarizer)
		})
		if allocs != 0 {
			t.Fatalf("pruneTranscript over %d small-result pairs allocated %.0f times; the no-large-result turn must be allocation-free and stay flat as history grows", pairs, allocs)
		}
	}
}
