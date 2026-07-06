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

// TestEncodeCheckpointDeltaAllocsFlat proves the per-turn checkpoint write does not
// re-serialize the whole history: with a warm cache (the prefix decoded from the
// prior step) only the turn just appended is marshaled, so encode allocation stays
// flat as the transcript grows instead of adding O(history) to every step. It asserts
// the count at a deep transcript is no worse than at a shallow one bar a small
// constant for the larger output buffer, which is what turns the per-goal write cost
// from quadratic into linear.
func TestEncodeCheckpointDeltaAllocsFlat(t *testing.T) {
	warmEncodeAllocs := func(pairs int) float64 {
		cp := checkpoint{Messages: benchTranscript(pairs, true)}
		raw, err := encodeCheckpoint(cp)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := decodeCheckpoint(raw) // warm cache over the whole prefix
		if err != nil {
			t.Fatal(err)
		}
		// Append one model turn (a tool call and its result): the hot per-step path.
		dec.Messages = append(dec.Messages, callMsg("new", "read"), resultMsg("new", big("z"), false))
		dec.Turns++
		return testing.AllocsPerRun(100, func() {
			if _, err := encodeCheckpoint(dec); err != nil {
				t.Fatal(err)
			}
		})
	}
	shallow := warmEncodeAllocs(16)
	deep := warmEncodeAllocs(256) // 16x the history
	// A full re-marshal would scale allocation with history; the delta write must not.
	// Allow a small additive margin for the larger output buffer's growth steps.
	if deep > shallow+4 {
		t.Fatalf("delta checkpoint encode allocated %.0f at depth 256 vs %.0f at depth 16; a 16x history must not scale per-turn encode allocation", deep, shallow)
	}
}

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
