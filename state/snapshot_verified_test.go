package state

import (
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/spine"
)

// noSnapLog hides a log's snapshots, so a Replay over it is forced to fold the whole
// stream: the ground-truth projection every snapshot restore is compared to.
type noSnapLog struct{ spine.Log }

func (noSnapLog) LatestSnapshot(context.Context, string, int64) (spine.Snapshot, bool, error) {
	return spine.Snapshot{}, false, nil
}

func stateTestSealer(t *testing.T) *chain.SnapshotSealer {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 0x5a
	}
	priv := ed25519.NewKeyFromSeed(seed)
	signer, err := chain.NewEd25519RootSigner("inst-1", priv)
	if err != nil {
		t.Fatal(err)
	}
	ring := chain.NewRootKeyring()
	if err := ring.Add("inst-1", priv.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
	sealer, err := chain.NewSnapshotSealer(signer, ring, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sealer
}

// coreDump is the observable projection two providers are compared by: the full read
// model, tombstones and slug index included, serialized deterministically.
func coreDump(t *testing.T, p Provider) string {
	t.Helper()
	mp := p.(*memProvider)
	mp.core.mu.Lock()
	snap := mp.core.snapshotLocked()
	mp.core.mu.Unlock()
	b, err := MarshalSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestVerifiedSnapshotReplayEquivalence is the equivalence gate: the projection restored
// from a verified snapshot plus the folded suffix equals the projection folded from the
// start of the stream. A snapshot is a cache, so both paths must agree.
func TestVerifiedSnapshotReplayEquivalence(t *testing.T) {
	ctx := context.Background()
	sealer := stateTestSealer(t)
	log := spine.NewMemoryLog()
	p := NewMemory(WithEventLog(log), WithSnapshotCodec(sealer)).(*memProvider)

	populateState(t, p)
	if err := p.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	// Mutations after the snapshot must be folded as the suffix on top of it.
	if _, err := p.Sessions().Create(ctx, Session{Title: "after"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Skills().Upsert(ctx, Skill{Slug: "deploy", Name: "Deploy v2", Body: "again"}); err != nil {
		t.Fatal(err)
	}

	fromSnap, err := Replay(ctx, log, WithSnapshotCodec(sealer))
	if err != nil {
		t.Fatal(err)
	}
	fromZero, err := Replay(ctx, noSnapLog{log}, WithSnapshotCodec(sealer))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := coreDump(t, fromSnap), coreDump(t, fromZero); got != want {
		t.Fatalf("snapshot replay differs from fold-from-zero:\n snap=%s\n zero=%s", got, want)
	}
}

// TestTamperedSnapshotFallsBackToFullFold asserts a snapshot the codec cannot verify is
// never restored: a bit-flipped sealed payload is rejected and the stream folds from the
// start, so the result is still correct.
func TestTamperedSnapshotFallsBackToFullFold(t *testing.T) {
	ctx := context.Background()
	sealer := stateTestSealer(t)
	log := spine.NewMemoryLog()
	p := NewMemory(WithEventLog(log), WithSnapshotCodec(sealer)).(*memProvider)

	populateState(t, p)
	if err := p.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}

	// Corrupt the stored sealed snapshot in place at the same (stream, seq).
	snap, found, err := log.LatestSnapshot(ctx, StateStream, 0)
	if err != nil || !found {
		t.Fatalf("no snapshot to tamper (found=%v err=%v)", found, err)
	}
	tampered := snap
	tampered.Payload = append([]byte(nil), snap.Payload...)
	tampered.Payload[len(tampered.Payload)/2] ^= 0x01
	if err := log.SaveSnapshot(ctx, tampered); err != nil {
		t.Fatal(err)
	}

	fromSnap, err := Replay(ctx, log, WithSnapshotCodec(sealer))
	if err != nil {
		t.Fatal(err)
	}
	fromZero, err := Replay(ctx, noSnapLog{log}, WithSnapshotCodec(sealer))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := coreDump(t, fromSnap), coreDump(t, fromZero); got != want {
		t.Fatalf("a tampered snapshot was restored instead of folding from the log:\n snap=%s\n zero=%s", got, want)
	}
}
