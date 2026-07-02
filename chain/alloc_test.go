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
	if allocs > 24 {
		t.Errorf("CanonicalBytes allocates %.0f/op, over the 24 ceiling: a codec regression (or lower the ceiling if the cost legitimately grew)", allocs)
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
	if allocs > 20 {
		t.Errorf("DecodeCanonical allocates %.0f/op, over the 20 ceiling: a codec regression (or lower the ceiling if the cost legitimately grew)", allocs)
	}
}
