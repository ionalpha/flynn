package sqlite

// The Merkle node store and the checkpoint reads, where absence is the ordinary answer
// and must not read as failure. A node never written, a slot past a tile's filled
// prefix, a stream with no signed head: each reports absent with no error, which is what
// lets a reopening log start a fresh tree instead of refusing to open. A hash of the
// wrong width is a different matter and is refused.

import (
	"bytes"
	"context"
	"testing"
)

// TestMerkleNodeAbsenceIsNotAnError gates proof assembly: a node the tiled store has not
// recorded reports absent rather than erroring, both for a tile that was never written and
// for a slot past the filled prefix of a resident tile. A tile is a fixed-width blob, so
// "the blob is shorter than this offset" must mean absent, not a slice out of range.
func TestMerkleNodeAbsenceIsNotAnError(t *testing.T) {
	s := newStore(t)
	ms := s.MerkleNodes("run/merkle")

	if _, ok, err := ms.Node(0, 0); err != nil || ok {
		t.Fatalf("Node on an empty store = (ok=%v, err=%v), want absent and no error", ok, err)
	}
	if err := ms.PutNode(0, 0, hash32(0xab)); err != nil {
		t.Fatal(err)
	}
	// Slot 0 of the resident tile is served from memory, verbatim.
	got, ok, err := ms.Node(0, 0)
	if err != nil || !ok || !bytes.Equal(got, hash32(0xab)) {
		t.Fatalf("Node(0,0) = (%x, ok=%v, err=%v), want the stored hash", got, ok, err)
	}
	// Slot 5 lives in the same resident tile but past its filled prefix: absent.
	if _, ok, err := ms.Node(0, 5); err != nil || ok {
		t.Fatalf("Node(0,5) past the filled prefix = (ok=%v, err=%v), want absent and no error", ok, err)
	}
}

// TestMerkleRejectsAWrongWidthHash guards the tile layout: tiles address nodes by fixed
// offset, so a hash of any other width would corrupt every neighbouring slot. It is
// refused at the write, not truncated or padded.
func TestMerkleRejectsAWrongWidthHash(t *testing.T) {
	s := newStore(t)
	ms := s.MerkleNodes("run/merkle")
	if err := ms.PutNode(0, 0, []byte{1, 2, 3}); err == nil {
		t.Fatal("PutNode accepted a 3-byte hash, want a rejection")
	}
}

// TestMerkleFailuresSurfaceOnABrokenStore proves the tiled node store never silently drops
// proof material: with the database closed underneath it, a read of an evicted tile, a
// write that must resume a persisted tile, the write that fills (and so persists) a tile,
// and Flush all report the failure. A swallowed error here would leave a log whose
// checkpoint commits to nodes that were never written.
func TestMerkleFailuresSurfaceOnABrokenStore(t *testing.T) {
	s := newStore(t)
	ms := s.MerkleNodes("run/broken")

	// Fill the tile up to (but not including) its last slot, so it stays resident and
	// the closing write below is the one that persists it.
	for i := range uint64(merkleTileWidth - 1) {
		if err := ms.PutNode(0, i, hash32(byte(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ms.Node(1, 0); err == nil {
		t.Error("Node of a non-resident tile succeeded on a closed store")
	}
	if err := ms.PutNode(1, 0, hash32(0x01)); err == nil {
		t.Error("PutNode that must resume a persisted tile succeeded on a closed store")
	}
	if err := ms.PutNode(0, merkleTileWidth-1, hash32(0xff)); err == nil {
		t.Error("PutNode that fills (and persists) a tile succeeded on a closed store")
	}
	if err := ms.Flush(); err == nil {
		t.Error("Flush succeeded on a closed store")
	}
}

// TestCheckpointLookupsReportAbsence pins the checkpoint reads' miss semantics: a stream
// with no signed head, and a size no head was stored at, both report ok=false with no
// error. A reopening log distinguishes "never checkpointed" from "the read failed", so it
// can start a fresh tree instead of refusing to open.
func TestCheckpointLookupsReportAbsence(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, _, ok, err := s.LatestCheckpoint(ctx, "run/none"); err != nil || ok {
		t.Fatalf("LatestCheckpoint of an unchecked stream = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	if err := s.SaveCheckpoint(ctx, "run/some", 4, []byte("cose-4")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.CheckpointAt(ctx, "run/some", 9); err != nil || ok {
		t.Fatalf("CheckpointAt a size with no head = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	cose, ok, err := s.CheckpointAt(ctx, "run/some", 4)
	if err != nil || !ok || string(cose) != "cose-4" {
		t.Fatalf("CheckpointAt(4) = (%q, ok=%v, err=%v), want the saved head", cose, ok, err)
	}
}
