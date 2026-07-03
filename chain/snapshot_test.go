package chain

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/spine"
)

// snapshotFixture builds a log with n events on one stream and a sealer whose
// keyring holds the signing key, the setup every snapshot test starts from.
func snapshotFixture(t *testing.T, n int) (spine.Log, *SnapshotSealer, spine.Snapshot) {
	t.Helper()
	ctx := context.Background()
	log := spine.NewMemoryLog()
	var lastSeq int64
	for i := range n {
		e, err := log.Append(ctx, spine.AppendInput{
			Stream:  "run-1",
			Type:    "step.completed",
			Actor:   spine.ActorAgent,
			Payload: map[string]any{"i": int64(i)},
		})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		lastSeq = e.Seq
	}
	priv, pub := testKey(0x21)
	signer, err := NewEd25519RootSigner("inst-1", priv)
	if err != nil {
		t.Fatal(err)
	}
	ring := NewRootKeyring()
	if err := ring.Add("inst-1", pub); err != nil {
		t.Fatal(err)
	}
	sealer, err := NewSnapshotSealer(signer, ring, nil)
	if err != nil {
		t.Fatal(err)
	}
	return log, sealer, spine.Snapshot{Stream: "run-1", Seq: lastSeq, Payload: []byte(`{"state":"folded"}`)}
}

func TestSnapshotSealOpenRoundTrip(t *testing.T) {
	ctx := context.Background()
	log, sealer, snap := snapshotFixture(t, 7)

	sealed, err := sealer.Seal(ctx, log, snap)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if string(sealed.Payload) == string(snap.Payload) {
		t.Fatal("sealed payload should not be the raw state")
	}
	opened, err := sealer.Open(ctx, sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(opened.Payload) != string(snap.Payload) {
		t.Fatalf("opened state = %q, want %q", opened.Payload, snap.Payload)
	}
	if opened.Stream != snap.Stream || opened.Seq != snap.Seq {
		t.Fatalf("opened binding = (%s, %d), want (%s, %d)", opened.Stream, opened.Seq, snap.Stream, snap.Seq)
	}
}

// TestSnapshotClaimMatchesIndependentTree proves the sealed claim's checkpoint is
// the stream's real Merkle head: a verifier folding the prefix independently
// reaches the same root, which is what makes a snapshot auditable in depth.
func TestSnapshotClaimMatchesIndependentTree(t *testing.T) {
	ctx := context.Background()
	log, sealer, snap := snapshotFixture(t, 5)

	sealed, err := sealer.Seal(ctx, log, snap)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	var wire snapshotWire
	if err := canonicalDec.Unmarshal(sealed.Payload, &wire); err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	claim, err := VerifySnapshotClaim(wire.COSE, sealer.ring)
	if err != nil {
		t.Fatalf("verify claim: %v", err)
	}

	events, err := log.Read(ctx, spine.Query{Stream: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	tree := NewTree()
	for _, e := range events {
		canonical, err := CanonicalBytes(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := tree.Append(canonical); err != nil {
			t.Fatal(err)
		}
	}
	root, err := tree.Root()
	if err != nil {
		t.Fatal(err)
	}
	if claim.Checkpoint.Size != tree.Size() {
		t.Fatalf("claim size = %d, want %d", claim.Checkpoint.Size, tree.Size())
	}
	if string(claim.Checkpoint.RootHash) != string(root) {
		t.Fatal("claim root does not match an independently folded tree")
	}
}

// TestSnapshotTamperRejected is the tamper gate: any bit flipped anywhere in the
// sealed artifact must fail Open, so a rebuild falls back to a full fold rather
// than restoring corrupted or forged state.
func TestSnapshotTamperRejected(t *testing.T) {
	ctx := context.Background()
	log, sealer, snap := snapshotFixture(t, 7)
	sealed, err := sealer.Seal(ctx, log, snap)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	for i := range sealed.Payload {
		flipped := spine.Snapshot{Stream: sealed.Stream, Seq: sealed.Seq, Payload: append([]byte(nil), sealed.Payload...)}
		flipped.Payload[i] ^= 0x01
		if _, err := sealer.Open(ctx, flipped); err == nil {
			t.Fatalf("bit flip at byte %d was not rejected", i)
		}
	}
}

func TestSnapshotBindingMismatchRejected(t *testing.T) {
	ctx := context.Background()
	log, sealer, snap := snapshotFixture(t, 7)
	sealed, err := sealer.Seal(ctx, log, snap)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	wrongStream := sealed
	wrongStream.Stream = "run-2"
	if _, err := sealer.Open(ctx, wrongStream); err == nil {
		t.Fatal("a claim replayed onto another stream was not rejected")
	}
	wrongSeq := sealed
	wrongSeq.Seq = sealed.Seq - 1
	if _, err := sealer.Open(ctx, wrongSeq); err == nil {
		t.Fatal("a claim replayed at another seq was not rejected")
	}
}

func TestSnapshotUnknownKeyRejected(t *testing.T) {
	ctx := context.Background()
	log, sealer, snap := snapshotFixture(t, 3)
	sealed, err := sealer.Seal(ctx, log, snap)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	_, otherPub := testKey(0x42)
	otherRing := NewRootKeyring()
	if err := otherRing.Add("inst-1", otherPub); err != nil {
		t.Fatal(err)
	}
	verifier, err := NewSnapshotSealer(nil, otherRing, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Open(ctx, sealed); err == nil {
		t.Fatal("a snapshot signed by a key the ring does not hold was not rejected")
	}
}

// TestSnapshotCheckpointSignatureNotInterchangeable proves the content-type
// separation: a signed checkpoint cannot be presented as a snapshot claim, and a
// snapshot claim cannot be presented as a checkpoint, even under the same key.
func TestSnapshotCheckpointSignatureNotInterchangeable(t *testing.T) {
	priv, pub := testKey(0x33)
	signer, err := NewEd25519RootSigner("inst-1", priv)
	if err != nil {
		t.Fatal(err)
	}
	ring := NewRootKeyring()
	if err := ring.Add("inst-1", pub); err != nil {
		t.Fatal(err)
	}
	sc, err := signer.SignCheckpoint(sampleCheckpoint(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySnapshotClaim(sc.COSE, ring); err == nil {
		t.Fatal("a checkpoint signature was accepted as a snapshot claim")
	}
	claimSig, err := signer.SignSnapshotClaim(SnapshotClaim{Stream: "s", Seq: 1, StateHash: make([]byte, 32), Checkpoint: sampleCheckpoint(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyCheckpoint(claimSig, ring); err == nil {
		t.Fatal("a snapshot claim signature was accepted as a checkpoint")
	}
}

func TestSnapshotSealRefusals(t *testing.T) {
	ctx := context.Background()
	log, sealer, snap := snapshotFixture(t, 3)

	short := snap
	short.Seq = snap.Seq + 10
	if _, err := sealer.Seal(ctx, log, short); err == nil {
		t.Fatal("sealing past the stream head was not refused")
	}

	verifyOnly, err := NewSnapshotSealer(nil, sealer.ring, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyOnly.Seal(ctx, log, snap); err == nil {
		t.Fatal("a verify-only sealer accepted a Seal")
	}
	if _, err := NewSnapshotSealer(nil, nil, nil); err == nil {
		t.Fatal("a sealer with no keyring was not refused")
	}
}
