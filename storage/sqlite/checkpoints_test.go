package sqlite_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"path/filepath"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// testSigner returns a deterministic Ed25519 checkpoint signer and a keyring holding
// its public key, so a test can sign tree heads and verify them without external state.
func testSigner(t *testing.T, seed byte) (chain.RootSigner, *chain.RootKeyring) {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	signer, err := chain.NewEd25519RootSigner("test-inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	ring := chain.NewRootKeyring()
	if err := ring.Add("test-inst", priv.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
	return signer, ring
}

// signHead signs the tree's current head as a checkpoint under origin.
func signHead(t *testing.T, signer chain.RootSigner, origin string, tree *chain.Tree) []byte {
	t.Helper()
	root, err := tree.Root()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := signer.SignCheckpoint(chain.Checkpoint{Origin: origin, Size: tree.Size(), RootHash: root})
	if err != nil {
		t.Fatal(err)
	}
	return sc.COSE
}

// TestDurableCheckpointConsistencyAcrossReopen is the Layer 3 gate: a durable log signs
// tree heads as it grows and persists each; after a close and reopen, the latest
// checkpoint recovers the log's authenticated size and rebuilds the tree from tiles,
// and a consistency proof anchored at an earlier persisted checkpoint proves the log
// only appended between the two heads. This is the append-only property proven across
// persisted checkpoints on a reopened, tile-backed log, not held in memory.
func TestDurableCheckpointConsistencyAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "store.db")
	const stream = "run/checkpointed"
	const origin = "flynn://run/checkpointed"
	const step = 200
	const steps = 5 // checkpoints at 200, 400, 600, 800, 1000
	signer, ring := testSigner(t, 0x42)

	leaves := canonicalEvents(t, stream, step*steps)

	s1, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	ms := s1.MerkleNodes(stream)
	tree := chain.NewTreeWithStore(ms)
	for i, cb := range leaves {
		if err := tree.Append(cb); err != nil {
			t.Fatal(err)
		}
		if (i+1)%step == 0 {
			// Flush the tiles, then sign and persist the head, so the checkpoint's
			// size is always fully backed by durable tiles.
			if err := ms.Flush(); err != nil {
				t.Fatal(err)
			}
			if err := s1.SaveCheckpoint(ctx, stream, tree.Size(), signHead(t, signer, origin, tree)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the latest checkpoint gives the authenticated size, and the tree rebuilds
	// from tiles to exactly that head.
	s2, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()

	size, latestCOSE, ok, err := s2.LatestCheckpoint(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("no checkpoint found after reopen")
	}
	if size != uint64(step*steps) {
		t.Fatalf("latest checkpoint size = %d, want %d", size, step*steps)
	}
	latest, err := chain.VerifyCheckpoint(latestCOSE, ring)
	if err != nil {
		t.Fatalf("latest checkpoint does not verify: %v", err)
	}

	reloaded, err := chain.LoadTree(s2.MerkleNodes(stream), size)
	if err != nil {
		t.Fatalf("LoadTree at the checkpoint size: %v", err)
	}
	root, err := reloaded.Root()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(root, latest.RootHash) {
		t.Fatal("reopened tree head does not match the signed checkpoint root")
	}

	// A consistency proof anchored at an earlier persisted checkpoint proves the log
	// only appended in between.
	const earlier = step * 2 // 400
	earlierCOSE, ok, err := s2.CheckpointAt(ctx, stream, earlier)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("no checkpoint stored at size %d", earlier)
	}
	path, err := reloaded.ConsistencyProof(earlier)
	if err != nil {
		t.Fatalf("consistency proof from tiles: %v", err)
	}
	raw, err := (&chain.ConsistencyProof{Before: earlierCOSE, After: latestCOSE, Proof: path}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	before, after, err := chain.VerifyConsistencyProof(raw, ring)
	if err != nil {
		t.Fatalf("consistency proof between persisted checkpoints rejected: %v", err)
	}
	if before.Size != earlier || after.Size != size {
		t.Fatalf("consistency proof connects sizes %d and %d, want %d and %d", before.Size, after.Size, earlier, size)
	}

	// A forged path between the two real checkpoints must not verify.
	if len(path) > 0 {
		bad := make([][]byte, len(path))
		copy(bad, path)
		bad[0] = append([]byte(nil), bad[0]...)
		bad[0][0] ^= 0xff
		badRaw, err := (&chain.ConsistencyProof{Before: earlierCOSE, After: latestCOSE, Proof: bad}).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := chain.VerifyConsistencyProof(badRaw, ring); err == nil {
			t.Fatal("a forged consistency path between persisted checkpoints verified")
		}
	}
}
