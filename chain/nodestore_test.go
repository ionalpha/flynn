package chain

import (
	"bytes"
	"testing"

	"github.com/transparency-dev/merkle/proof"
	"pgregory.net/rapid"
)

// buildTreeStore builds a size-n log backed by store and returns the tree and the
// leaf hashes, so a test can drive one tree shape through different node layouts.
func buildTreeStore(t *testing.T, store nodeStore, n int) (*Tree, [][]byte) {
	t.Helper()
	tr := newTreeWithStore(store)
	leaves := make([][]byte, n)
	for i := range n {
		e := sampleEvent()
		e.Seq = int64(i + 1)
		cb, err := CanonicalBytes(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := tr.Append(cb); err != nil {
			t.Fatal(err)
		}
		lh, err := LeafHash(cb)
		if err != nil {
			t.Fatal(err)
		}
		leaves[i] = lh
	}
	return tr, leaves
}

func equalProof(a, b [][]byte) bool {
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

// TestTiledStoreMatchesMemStore asserts the tiled node layout produces byte-identical
// roots, inclusion proofs, and consistency proofs to the in-memory map, including at
// sizes that straddle the tile boundary and at the two-point n / 2n scale. Paging
// nodes into fixed-width tiles is a storage change, so it must never change what the
// log proves.
func TestTiledStoreMatchesMemStore(t *testing.T) {
	// tileWidth-1, tileWidth, tileWidth+1 exercise the boundary; 512/1024 are the
	// n / 2n pair that spans multiple tiles per level.
	for _, n := range []int{1, tileWidth - 1, tileWidth, tileWidth + 1, 512, 1024} {
		memTree, _ := buildTreeStore(t, newMemNodeStore(), n)
		tiledTree, leaves := buildTreeStore(t, newTiledNodeStore(), n)

		memRoot, err := memTree.Root()
		if err != nil {
			t.Fatal(err)
		}
		tiledRoot, err := tiledTree.Root()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(memRoot, tiledRoot) {
			t.Fatalf("n=%d: tiled root differs from in-memory root", n)
		}

		for _, idx := range []int{0, n / 2, n - 1} {
			mp, err := memTree.InclusionProof(uint64(idx))
			if err != nil {
				t.Fatal(err)
			}
			tp, err := tiledTree.InclusionProof(uint64(idx))
			if err != nil {
				t.Fatal(err)
			}
			if !equalProof(mp, tp) {
				t.Fatalf("n=%d idx=%d: tiled inclusion proof differs from in-memory", n, idx)
			}
			if err := VerifyInclusion(uint64(idx), uint64(n), leaves[idx], tiledRoot, tp); err != nil {
				t.Fatalf("n=%d idx=%d: tiled inclusion proof rejected: %v", n, idx, err)
			}
		}

		if n > 1 {
			half := n / 2
			mc, err := memTree.ConsistencyProof(uint64(half))
			if err != nil {
				t.Fatal(err)
			}
			tc, err := tiledTree.ConsistencyProof(uint64(half))
			if err != nil {
				t.Fatal(err)
			}
			if !equalProof(mc, tc) {
				t.Fatalf("n=%d: tiled consistency proof differs from in-memory", n)
			}
		}
	}
}

// TestTiledStorePagesNodes asserts the level-0 leaves are held in ceil(n/tileWidth)
// tiles rather than one entry per node, so a growing log's proof material is paged
// into a bounded number of fixed-width blobs instead of an unbounded flat map.
func TestTiledStorePagesNodes(t *testing.T) {
	const n = 1000
	store := newTiledNodeStore()
	buildTreeStore(t, store, n)

	wantLeafTiles := (n + tileWidth - 1) / tileWidth
	leafTiles := 0
	for id := range store.tiles {
		if id.level == 0 {
			leafTiles++
		}
	}
	if leafTiles != wantLeafTiles {
		t.Fatalf("level-0 tiles = %d, want %d for %d leaves", leafTiles, wantLeafTiles, n)
	}
}

// TestTiledStoreClonedSnapshotIsStable asserts a cloned tiled store keeps serving the
// proofs it held at clone time even as the source tree keeps appending, which is the
// property a sealed run depends on.
func TestTiledStoreClonedSnapshotIsStable(t *testing.T) {
	store := newTiledNodeStore()
	tree, leaves := buildTreeStore(t, store, 300)
	sealed := store.clone()
	rootAt300, err := tree.Root()
	if err != nil {
		t.Fatal(err)
	}

	// Keep appending to the live tree after the snapshot.
	for i := 300; i < 400; i++ {
		e := sampleEvent()
		e.Seq = int64(i + 1)
		cb, err := CanonicalBytes(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := tree.Append(cb); err != nil {
			t.Fatal(err)
		}
	}

	// The snapshot still assembles the size-300 inclusion proof for an early leaf.
	nodes, err := proof.Inclusion(5, 300)
	if err != nil {
		t.Fatal(err)
	}
	pf, err := assembleProof(sealed, nodes)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyInclusion(5, 300, leaves[5], rootAt300, pf); err != nil {
		t.Fatalf("cloned snapshot no longer proves the size-300 tree: %v", err)
	}
}

// TestPropTiledStoreInclusion is the property form: across many sizes and indices, a
// tiled-store inclusion proof verifies and never differs from the in-memory proof.
func TestPropTiledStoreInclusion(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 600).Draw(rt, "n")
		memTree, _ := buildTreeStore(t, newMemNodeStore(), n)
		tiledTree, leaves := buildTreeStore(t, newTiledNodeStore(), n)
		root, err := tiledTree.Root()
		if err != nil {
			t.Fatal(err)
		}
		idx := rapid.IntRange(0, n-1).Draw(rt, "idx")
		mp, err := memTree.InclusionProof(uint64(idx))
		if err != nil {
			t.Fatal(err)
		}
		tp, err := tiledTree.InclusionProof(uint64(idx))
		if err != nil {
			t.Fatal(err)
		}
		if !equalProof(mp, tp) {
			rt.Fatalf("n=%d idx=%d: tiled proof differs from in-memory", n, idx)
		}
		if err := VerifyInclusion(uint64(idx), uint64(n), leaves[idx], root, tp); err != nil {
			rt.Fatalf("n=%d idx=%d: tiled inclusion rejected: %v", n, idx, err)
		}
	})
}
