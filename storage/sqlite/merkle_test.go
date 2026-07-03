package sqlite_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// canonicalEvents builds n canonical event encodings on one stream, the leaf bytes a
// verifiable log is appended from.
func canonicalEvents(t *testing.T, stream string, n int) [][]byte {
	t.Helper()
	out := make([][]byte, n)
	for i := range n {
		e := spine.Event{
			Stream:        stream,
			Seq:           int64(i + 1),
			Time:          time.Unix(0, int64(i+1)).UTC(),
			Type:          "test.event",
			Actor:         spine.ActorSystem,
			SchemaVersion: 1,
			Payload:       map[string]any{"i": i},
		}
		cb, err := chain.CanonicalBytes(e)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = cb
	}
	return out
}

func equalPath(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// memTreeOf builds an in-memory reference tree over the given leaves.
func memTreeOf(t *testing.T, leaves [][]byte) *chain.Tree {
	t.Helper()
	tr := chain.NewTree()
	for _, cb := range leaves {
		if err := tr.Append(cb); err != nil {
			t.Fatal(err)
		}
	}
	return tr
}

// TestDurableMerkleStoreReopen is the Layer 2 gate: a log appended through the durable
// tiled store proves the same roots and paths as the in-memory tree, those proofs
// survive a close and reopen (loaded back from tiles with chain.LoadTree), and the log
// keeps growing after the reopen. Tiling is a storage change, so it must never change
// what the log proves, and it must not require folding the whole history back to serve
// a proof.
func TestDurableMerkleStoreReopen(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "store.db")
	const stream = "run/durable"
	const n = 1000

	leaves := canonicalEvents(t, stream, n)
	ref := memTreeOf(t, leaves)
	refRoot, err := ref.Root()
	if err != nil {
		t.Fatal(err)
	}

	// Append through the durable store, then flush and close.
	s1, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	ms := s1.MerkleNodes(stream)
	tree := chain.NewTreeWithStore(ms)
	for _, cb := range leaves {
		if err := tree.Append(cb); err != nil {
			t.Fatal(err)
		}
	}
	root, err := tree.Root()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(root, refRoot) {
		t.Fatal("durable-store root differs from the in-memory tree")
	}
	if err := ms.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen and rebuild the tree from the persisted tiles alone.
	s2, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	ms2 := s2.MerkleNodes(stream)
	reloaded, err := chain.LoadTree(ms2, n)
	if err != nil {
		t.Fatalf("LoadTree from persisted tiles: %v", err)
	}
	reRoot, err := reloaded.Root()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reRoot, refRoot) {
		t.Fatal("reopened root differs from the in-memory tree")
	}

	// Inclusion and consistency proofs from the reopened, tile-backed log match the
	// in-memory reference and verify.
	for _, idx := range []int{0, 1, n / 2, n - 1} {
		want, err := ref.InclusionProof(uint64(idx))
		if err != nil {
			t.Fatal(err)
		}
		got, err := reloaded.InclusionProof(uint64(idx))
		if err != nil {
			t.Fatalf("reopened inclusion proof idx=%d: %v", idx, err)
		}
		if !equalPath(want, got) {
			t.Fatalf("idx=%d: reopened inclusion proof differs from in-memory", idx)
		}
		leaf, err := chain.LeafHash(leaves[idx])
		if err != nil {
			t.Fatal(err)
		}
		if err := chain.VerifyInclusion(uint64(idx), uint64(n), leaf, reRoot, got); err != nil {
			t.Fatalf("idx=%d: reopened inclusion proof rejected: %v", idx, err)
		}
	}
	half := n / 2
	wantC, err := ref.ConsistencyProof(uint64(half))
	if err != nil {
		t.Fatal(err)
	}
	gotC, err := reloaded.ConsistencyProof(uint64(half))
	if err != nil {
		t.Fatal(err)
	}
	if !equalPath(wantC, gotC) {
		t.Fatal("reopened consistency proof differs from in-memory")
	}

	// The reopened log keeps growing: appending more leaves tracks a fresh in-memory
	// tree over the full sequence, so the frontier was rebuilt, not reset.
	more := canonicalEvents(t, stream, n+50)[n:]
	for _, cb := range more {
		if err := reloaded.Append(cb); err != nil {
			t.Fatal(err)
		}
	}
	grown, err := reloaded.Root()
	if err != nil {
		t.Fatal(err)
	}
	full := memTreeOf(t, canonicalEvents(t, stream, n+50))
	fullRoot, err := full.Root()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(grown, fullRoot) {
		t.Fatal("root after post-reopen appends differs from a full in-memory tree")
	}
}

// TestDurableMerkleStoreResidencyBounded asserts the resident tile set is bounded by
// the log's height, not its length: after appending many leaves, only the rightmost
// partial tile per level stays in memory because full tiles are persisted and evicted.
func TestDurableMerkleStoreResidencyBounded(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "store.db")
	const stream = "run/resident"
	const n = 5000

	s, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ms := s.MerkleNodes(stream)
	tree := chain.NewTreeWithStore(ms)
	for _, cb := range canonicalEvents(t, stream, n) {
		if err := tree.Append(cb); err != nil {
			t.Fatal(err)
		}
	}
	// A 5000-leaf log is ~13 levels; the resident set is one partial tile per level at
	// most, far below the ~5000 nodes a flat in-memory map would hold. The bound guards
	// against a regression that stops evicting full tiles.
	if got := ms.Resident(); got > 32 {
		t.Fatalf("resident tiles = %d, want the O(log n) frontier (<= 32) for %d leaves", got, n)
	}
}
