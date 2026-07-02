//go:build !race

// Allocation ceilings for the write-path core: the number moved (the 2026-07
// write-path work) and these keep it from moving back. Ceilings are the measured
// cost plus ~30% headroom, not aspirations; lower them when the cost drops,
// never raise them to absorb a regression. They are excluded under -race
// (instrumentation skews allocation counts), so dev/test skips them; dev/bench
// and the CI bench job run them.

package resource_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/resource"
)

func assertAllocCeiling(t *testing.T, name string, ceiling float64, fn func()) {
	t.Helper()
	allocs := testing.AllocsPerRun(100, fn)
	if allocs > ceiling {
		t.Errorf("%s allocates %.0f/op, over the %.0f ceiling: a write-path regression (or lower the ceiling if the cost legitimately grew)", name, allocs, ceiling)
	}
}

func TestAllocCeilingHash(t *testing.T) {
	r := benchResource("alloc")
	assertAllocCeiling(t, "Hash", 95, func() {
		if _, err := resource.Hash(r); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAllocCeilingSpecHash(t *testing.T) {
	r := benchResource("alloc")
	assertAllocCeiling(t, "SpecHash", 40, func() {
		if _, err := resource.SpecHash(r); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAllocCeilingHashes(t *testing.T) {
	r := benchResource("alloc")
	assertAllocCeiling(t, "Hashes", 120, func() {
		if _, _, err := resource.Hashes(r); err != nil {
			t.Fatal(err)
		}
	})
}

// TestAllocCeilingMemoryPut pins the full in-memory command path: admission,
// stamping (both hashes), the single payload Marshal, the log append, and the
// raw-payload projection.
func TestAllocCeilingMemoryPut(t *testing.T) {
	ctx := context.Background()
	store := resource.NewMemory(benchRegistry())
	defer func() { _ = store.Close() }()
	r := benchResource("alloc")
	assertAllocCeiling(t, "memory Store.Put", 340, func() {
		if _, err := store.Put(ctx, r); err != nil {
			t.Fatal(err)
		}
	})
}
