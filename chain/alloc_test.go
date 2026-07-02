//go:build !race

// Allocation ceilings for the canonical codec, which every chained append and
// every verification pays. Ceilings are the measured cost plus ~30% headroom;
// lower them when the cost drops, never raise them to absorb a regression.
// Excluded under -race (instrumentation skews allocation counts), so dev/test
// skips them; dev/bench and the CI bench job run them.

package chain

import "testing"

func TestAllocCeilingCanonicalBytes(t *testing.T) {
	e := sampleEvent()
	allocs := testing.AllocsPerRun(100, func() {
		if _, err := CanonicalBytes(e); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 4 {
		t.Errorf("CanonicalBytes allocates %.0f/op, over the 4 ceiling: a codec regression (or lower the ceiling if the cost legitimately grew)", allocs)
	}
}

func TestAllocCeilingDecodeCanonical(t *testing.T) {
	b, err := CanonicalBytes(sampleEvent())
	if err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		if _, err := DecodeCanonical(b); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 18 {
		t.Errorf("DecodeCanonical allocates %.0f/op, over the 18 ceiling: a codec regression (or lower the ceiling if the cost legitimately grew)", allocs)
	}
}

// TestEventProofScalesLogarithmically is the two-point complexity gate on
// SealedRun.EventProof: the proof is assembled by node-map lookup from the
// seal-time snapshot, so its allocations grow with the audit-path length
// (log n), not the run length. Before the snapshot, EventProof rebuilt the
// whole Merkle tree per proof, so doubling the run doubled the cost (and
// proving every event was quadratic). A ratio near 2 means the rebuild is
// back.
func TestEventProofScalesLogarithmically(t *testing.T) {
	const n = 256
	small, _ := builtRun(t, n)
	large, _ := builtRun(t, 2*n)
	allocsSmall := testing.AllocsPerRun(50, func() {
		if _, err := small.EventProof(n / 2); err != nil {
			t.Fatal(err)
		}
	})
	allocsLarge := testing.AllocsPerRun(50, func() {
		if _, err := large.EventProof(n / 2); err != nil {
			t.Fatal(err)
		}
	})
	if allocsLarge > allocsSmall*1.5 {
		t.Errorf("EventProof allocations grew %.0f -> %.0f when the run doubled (ratio %.2f > 1.5): the per-proof tree rebuild is back",
			allocsSmall, allocsLarge, allocsLarge/allocsSmall)
	}
}
