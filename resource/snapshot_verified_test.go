package resource_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// noSnapLog hides a log's snapshots, so a Replay over it is forced to fold the
// whole stream: the ground-truth state every snapshot restore is compared to.
type noSnapLog struct{ spine.Log }

func (l noSnapLog) LatestSnapshot(context.Context, string, int64) (spine.Snapshot, bool, error) {
	return spine.Snapshot{}, false, nil
}

func testSealer(t testing.TB) *chain.SnapshotSealer {
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

// stateDump is the observable projection two stores are compared by: every live
// record of the kind, fully serialized.
func stateDump(t testing.TB, s resource.Store) string {
	t.Helper()
	all, err := s.ListAll(context.Background(), "Thing", nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	b, err := json.Marshal(all)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestVerifiedSnapshotReplayEquivalence is the equivalence gate: for randomized
// mutation streams, the state restored from a verified snapshot plus the folded
// suffix must equal the state folded from the start of the stream. A snapshot is
// a cache, never a second source of truth.
func TestVerifiedSnapshotReplayEquivalence(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		log := spine.NewMemoryLog()
		reg := resource.NewRegistry()
		if err := reg.Register(resource.Kind{APIVersion: "test/v1", Name: "Thing"}); err != nil {
			rt.Fatalf("register kind: %v", err)
		}
		sealer := testSealer(t)
		store := resource.NewMemory(
			reg,
			resource.WithEventLog(log),
			resource.WithSnapshotCodec(sealer),
		)

		names := []string{"alpha", "beta", "gamma", "delta"}
		steps := rapid.IntRange(1, 40).Draw(rt, "steps")
		snapshotAt := rapid.IntRange(0, steps-1).Draw(rt, "snapshotAt")
		for i := range steps {
			name := rapid.SampledFrom(names).Draw(rt, fmt.Sprintf("name%d", i))
			if rapid.Bool().Draw(rt, fmt.Sprintf("del%d", i)) {
				err := store.Delete(ctx, "Thing", resource.Scope{}, name)
				if err != nil && !errors.Is(err, resource.ErrNotFound) {
					rt.Fatalf("delete: %v", err)
				}
			} else {
				if _, err := store.Put(ctx, resource.Resource{
					APIVersion: "test/v1", Kind: "Thing", Name: name,
					Spec: json.RawMessage(fmt.Sprintf(`{"step":%d}`, i)),
				}); err != nil {
					rt.Fatalf("put: %v", err)
				}
			}
			if i == snapshotAt {
				if err := store.Snapshot(ctx); err != nil {
					rt.Fatalf("snapshot: %v", err)
				}
			}
		}

		fromSnapshot, err := resource.Replay(ctx, log, reg, resource.WithSnapshotCodec(sealer))
		if err != nil {
			rt.Fatalf("replay from snapshot: %v", err)
		}
		fullFold, err := resource.Replay(ctx, noSnapLog{log}, reg)
		if err != nil {
			rt.Fatalf("replay full fold: %v", err)
		}
		if got, want := stateDump(t, fromSnapshot), stateDump(t, fullFold); got != want {
			rt.Fatalf("snapshot restore diverged from full fold:\n got:  %s\n want: %s", got, want)
		}
	})
}

// TestTamperedSnapshotFallsBackToFullFold is the tamper gate: a bit-flipped
// stored snapshot must be rejected by the codec and the replay must fall back to
// folding the whole stream, ending in the exact same state.
func TestTamperedSnapshotFallsBackToFullFold(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	reg := resource.NewRegistry()
	if err := reg.Register(resource.Kind{APIVersion: "test/v1", Name: "Thing"}); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	sealer := testSealer(t)
	store := resource.NewMemory(
		reg,
		resource.WithEventLog(log),
		resource.WithSnapshotCodec(sealer),
	)
	for i := range 10 {
		if _, err := store.Put(ctx, resource.Resource{
			APIVersion: "test/v1", Kind: "Thing", Name: fmt.Sprintf("thing-%d", i%3),
			Spec: json.RawMessage(fmt.Sprintf(`{"step":%d}`, i)),
		}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	if err := store.Snapshot(ctx); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	snap, found, err := log.LatestSnapshot(ctx, resource.ResourceStream, 0)
	if err != nil || !found {
		t.Fatalf("latest snapshot: found=%v err=%v", found, err)
	}
	tampered := snap
	tampered.Payload = append([]byte(nil), snap.Payload...)
	tampered.Payload[len(tampered.Payload)/2] ^= 0x01
	if err := log.SaveSnapshot(ctx, tampered); err != nil {
		t.Fatalf("save tampered: %v", err)
	}

	replayed, err := resource.Replay(ctx, log, reg, resource.WithSnapshotCodec(sealer))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	fullFold, err := resource.Replay(ctx, noSnapLog{log}, reg)
	if err != nil {
		t.Fatalf("full fold: %v", err)
	}
	if got, want := stateDump(t, replayed), stateDump(t, fullFold); got != want {
		t.Fatalf("tampered snapshot changed the replayed state:\n got:  %s\n want: %s", got, want)
	}
}

// TestUnsignedSnapshotRejectedWhenCodecSet closes the downgrade hole: with a
// codec configured, a plain (unsigned) snapshot must not be restored - the
// replay folds from the start instead of trusting an unverified blob.
func TestUnsignedSnapshotRejectedWhenCodecSet(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	reg := resource.NewRegistry()
	if err := reg.Register(resource.Kind{APIVersion: "test/v1", Name: "Thing"}); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	// A store with no codec writes a plain snapshot.
	plain := resource.NewMemory(reg, resource.WithEventLog(log))
	if _, err := plain.Put(ctx, resource.Resource{
		APIVersion: "test/v1", Kind: "Thing", Name: "thing",
		Spec: json.RawMessage(`{"step":1}`),
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := plain.Snapshot(ctx); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	sealer := testSealer(t)
	replayed, err := resource.Replay(ctx, log, reg, resource.WithSnapshotCodec(sealer))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	fullFold, err := resource.Replay(ctx, noSnapLog{log}, reg)
	if err != nil {
		t.Fatalf("full fold: %v", err)
	}
	if got, want := stateDump(t, replayed), stateDump(t, fullFold); got != want {
		t.Fatalf("an unsigned snapshot leaked into a verified replay:\n got:  %s\n want: %s", got, want)
	}
}

// TestAutomaticSnapshotCadence proves WithSnapshotEvery writes checkpoints by
// itself: after more mutations than the cadence, a snapshot exists on the log and
// a replay through it matches the full fold.
func TestAutomaticSnapshotCadence(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	reg := resource.NewRegistry()
	if err := reg.Register(resource.Kind{APIVersion: "test/v1", Name: "Thing"}); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	sealer := testSealer(t)
	store := resource.NewMemory(
		reg,
		resource.WithEventLog(log),
		resource.WithSnapshotCodec(sealer),
		resource.WithSnapshotEvery(5),
	)
	for i := range 12 {
		if _, err := store.Put(ctx, resource.Resource{
			APIVersion: "test/v1", Kind: "Thing", Name: fmt.Sprintf("thing-%d", i),
			Spec: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	snap, found, err := log.LatestSnapshot(ctx, resource.ResourceStream, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("no snapshot was written by the automatic cadence")
	}
	if snap.Seq < 5 {
		t.Fatalf("snapshot seq = %d, want >= 5", snap.Seq)
	}
	fromSnapshot, err := resource.Replay(ctx, log, reg, resource.WithSnapshotCodec(sealer))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	fullFold, err := resource.Replay(ctx, noSnapLog{log}, reg)
	if err != nil {
		t.Fatalf("full fold: %v", err)
	}
	if got, want := stateDump(t, fromSnapshot), stateDump(t, fullFold); got != want {
		t.Fatalf("cadence snapshot diverged from full fold:\n got:  %s\n want: %s", got, want)
	}
}
