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
	"encoding/json"
	"fmt"
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
	assertAllocCeiling(t, "Hash", 80, func() {
		if _, err := resource.Hash(r); err != nil {
			t.Fatal(err)
		}
	})
}

// statusOfDepth builds a status blob shaped like a goal's inline transcript at the
// given message depth: the dominant, transcript-sized part of a goal's status.
func statusOfDepth(depth int) json.RawMessage {
	msgs := make([]map[string]string, depth)
	for i := range msgs {
		msgs[i] = map[string]string{"role": "assistant", "content": fmt.Sprintf("turn %d: a representative message body of some length", i)}
	}
	b, err := json.Marshal(map[string]any{"phase": "Running", "steps": depth, "messages": msgs})
	if err != nil {
		panic(err)
	}
	return b
}

func hashAllocsAtDepth(t *testing.T, depth int) float64 {
	t.Helper()
	r := benchResource("alloc")
	r.Status = statusOfDepth(depth)
	return testing.AllocsPerRun(50, func() {
		if _, err := resource.Hash(r); err != nil {
			t.Fatal(err)
		}
	})
}

// TestAllocCeilingHashFlatInStatusSize pins the content-hash guarantee that matters
// most for a long-running goal: the goal embeds its whole transcript in status, and
// the content hash runs on every write (2-3 per step), so the hash must fold status
// in as a fixed digest and cost the same at transcript depth n as at 2n. The
// generic-tree canonicalization this replaced allocated with the transcript, making
// a run O(history^2).
func TestAllocCeilingHashFlatInStatusSize(t *testing.T) {
	n := hashAllocsAtDepth(t, 200)
	n2 := hashAllocsAtDepth(t, 400)
	if n != n2 {
		t.Errorf("Hash allocates %.0f/op at transcript depth 200 but %.0f/op at 400: the content hash is scaling with the status size", n, n2)
	}
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
	assertAllocCeiling(t, "Hashes", 105, func() {
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

// listAllocs runs List over a store with a fixed live set and the given
// tombstone backlog, reporting allocations per call.
func listAllocs(t *testing.T, tombstones int) float64 {
	t.Helper()
	ctx := context.Background()
	store := tombstoneStore(t, 100, tombstones)
	defer func() { _ = store.Close() }()
	return testing.AllocsPerRun(50, func() {
		out, err := store.List(ctx, "Gadget", resource.Scope{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 100 {
			t.Fatalf("got %d live resources", len(out))
		}
	})
}

// TestAllocCeilingListFlatInTombstoneBacklog pins the read-side guarantee: a
// tombstone leaves the live index when it projects, so List over a fixed live
// set costs the same with 100 tombstones retained as with 10,000.
func TestAllocCeilingListFlatInTombstoneBacklog(t *testing.T) {
	small := listAllocs(t, 100)
	large := listAllocs(t, 10_000)
	if small != large {
		t.Errorf("List allocates %.0f/op with 100 tombstones but %.0f/op with 10000: list cost is scaling with the tombstone backlog", small, large)
	}
	if large > 8 {
		t.Errorf("List allocates %.0f/op, over the 8 ceiling: a read-path regression (or lower the ceiling if the cost legitimately dropped)", large)
	}
}
