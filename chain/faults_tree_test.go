package chain

// Tree and node-store failure paths. The invariant: a Tree never reports a successful
// append or hands back a proof when the store underneath it failed, was reopened
// incomplete, or cannot snapshot itself. A proof assembled over a partially written
// store would attest a root the log does not have.

import (
	"bytes"
	"errors"
	"testing"
)

// TestTreeSurfacesStoreFailures is the storage-failure gate: a Tree never reports a
// successful append or a proof when the node store underneath it failed. A failed leaf
// write, a failed internal-node write, and a read failure or a missing node during
// proof assembly must all surface as errors, because a proof assembled from a partially
// written store would attest a root the log does not have.
func TestTreeSurfacesStoreFailures(t *testing.T) {
	cb, err := CanonicalBytes(sampleEvent())
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("store is down")

	t.Run("leaf write fails", func(t *testing.T) {
		st := newErrStore()
		st.putErr, st.putLevel = boom, 0
		tr := NewTreeWithStore(st)
		if err := tr.Append(cb); !errors.Is(err, boom) {
			t.Fatalf("append error = %v, want the store failure", err)
		}
		if tr.Size() != 0 {
			t.Fatalf("size = %d after a failed append, want 0", tr.Size())
		}
	})

	t.Run("internal node write fails", func(t *testing.T) {
		st := newErrStore()
		st.putErr, st.putLevel = boom, 1
		tr := NewTreeWithStore(st)
		// The first append completes no internal node; the second completes level 1.
		if err := tr.Append(cb); err != nil {
			t.Fatal(err)
		}
		second := sampleEvent()
		second.Seq = 8
		cb2, err := CanonicalBytes(second)
		if err != nil {
			t.Fatal(err)
		}
		if err := tr.Append(cb2); !errors.Is(err, boom) {
			t.Fatalf("append error = %v, want the store failure", err)
		}
	})

	t.Run("proof assembly read fails", func(t *testing.T) {
		st := newErrStore()
		tr := NewTreeWithStore(st)
		for i := range 4 {
			e := sampleEvent()
			e.Seq = int64(i + 1)
			b, err := CanonicalBytes(e)
			if err != nil {
				t.Fatal(err)
			}
			if err := tr.Append(b); err != nil {
				t.Fatal(err)
			}
		}
		st.getErr = boom
		if _, err := tr.InclusionProof(0); !errors.Is(err, boom) {
			t.Fatalf("inclusion proof error = %v, want the store failure", err)
		}
		st.getErr, st.missAll = nil, true
		_, err := tr.InclusionProof(0)
		if !hasCode(err, CodeMissingNode) {
			t.Fatalf("inclusion proof over a store missing nodes: err = %v, want %s", err, CodeMissingNode)
		}
	})
}

// TestTreeRejectsOutOfRangeProofs asserts a Tree refuses to produce a proof it cannot
// honestly make: an inclusion proof for a leaf past its size, and a consistency proof
// to a size larger than the tree. Emitting a proof there would be a proof about a log
// that does not exist.
func TestTreeRejectsOutOfRangeProofs(t *testing.T) {
	tr, _ := buildTree(t, 5)

	if _, err := tr.InclusionProof(5); !hasCode(err, CodeInclusionInvalid) {
		t.Fatalf("inclusion proof at index == size: err = %v, want %s", err, CodeInclusionInvalid)
	}
	if _, err := tr.InclusionProof(99); !hasCode(err, CodeInclusionInvalid) {
		t.Fatalf("inclusion proof past the end: err = %v, want %s", err, CodeInclusionInvalid)
	}
	if _, err := tr.ConsistencyProof(6); !hasCode(err, CodeConsistencyInvalid) {
		t.Fatalf("consistency proof to a larger size: err = %v, want %s", err, CodeConsistencyInvalid)
	}
}

// TestLoadTreeRejectsIncompleteStore is the restart gate: reopening a log at a signed
// size from a store that cannot produce the frontier must fail rather than resume on a
// silently truncated tree, which would let later appends build on a root nobody signed.
func TestLoadTreeRejectsIncompleteStore(t *testing.T) {
	boom := errors.New("tile read failed")

	st := newErrStore()
	st.missAll = true
	if _, err := LoadTree(st, 4); !hasCode(err, CodeMissingNode) {
		t.Fatalf("LoadTree over an empty store: err = %v, want %s", err, CodeMissingNode)
	}

	st2 := newErrStore()
	st2.getErr = boom
	if _, err := LoadTree(st2, 4); !errors.Is(err, boom) {
		t.Fatalf("LoadTree error = %v, want the store failure", err)
	}

	// Size 0 needs no frontier node, so an empty store still reopens an empty tree.
	tr, err := LoadTree(newErrStore(), 0)
	if err != nil {
		t.Fatalf("LoadTree at size 0: %v", err)
	}
	if tr.Size() != 0 {
		t.Fatalf("size = %d, want 0", tr.Size())
	}
}

// TestCloneStoreRefusesUnsnapshottableStore asserts a tree over a store that cannot
// snapshot itself refuses to seal, rather than handing a SealedRun a node store that
// later appends can mutate underneath it.
func TestCloneStoreRefusesUnsnapshottableStore(t *testing.T) {
	tr := NewTreeWithStore(bareStore{newMemNodeStore()})
	if _, err := tr.cloneStore(); !hasCode(err, CodeEncode) {
		t.Fatalf("cloneStore over a bare store: err = %v, want %s", err, CodeEncode)
	}
}

// TestTiledNodeStoreReportsAbsentNodes asserts the tiled layout distinguishes an
// unwritten slot from a zero hash: a node in a tile that was never created, and a node
// past the filled prefix of an existing tile, both read as absent rather than as a
// valid all-zero hash.
func TestTiledNodeStoreReportsAbsentNodes(t *testing.T) {
	st := newTiledNodeStore()

	if _, ok, err := st.Node(0, 0); err != nil || ok {
		t.Fatalf("Node on an empty store = (ok=%v, err=%v), want absent", ok, err)
	}
	if err := st.PutNode(0, 0, bytes.Repeat([]byte{0xab}, hashSize)); err != nil {
		t.Fatal(err)
	}
	// Index 1 sits in the same tile but past its filled prefix.
	if _, ok, err := st.Node(0, 1); err != nil || ok {
		t.Fatalf("Node past the filled prefix = (ok=%v, err=%v), want absent", ok, err)
	}
	h, ok, err := st.Node(0, 0)
	if err != nil || !ok {
		t.Fatalf("Node(0,0) = (ok=%v, err=%v), want present", ok, err)
	}
	if !bytes.Equal(h, bytes.Repeat([]byte{0xab}, hashSize)) {
		t.Fatal("stored hash does not round trip through the tile")
	}
}
