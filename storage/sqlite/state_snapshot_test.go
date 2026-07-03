package sqlite

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/state"
)

func stateSealer(t *testing.T) *chain.SnapshotSealer {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 0x3c
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

// populateSQLiteState drives the store through sessions with turns, a skill upserted then
// deleted alongside a live one, and memory written then deleted, so the projection has
// tombstones and a non-trivial shape.
func populateSQLiteState(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	for i := range 3 {
		s, err := store.Sessions().Create(ctx, state.Session{Title: fmt.Sprintf("s%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Sessions().AppendTurn(ctx, state.Turn{SessionID: s.ID, Role: "user", Content: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	d, err := store.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Skills().Upsert(ctx, state.Skill{Slug: "keep", Name: "Keep", Body: "y"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Skills().Delete(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	m, err := store.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: "green"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Memory().Delete(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
}

// dumpState serializes the full state projection (every table row) deterministically, the
// value a rebuild must reproduce.
func dumpState(t *testing.T, store *Store) string {
	t.Helper()
	snap, err := store.readStateSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := state.MarshalSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestStateRebuildFromSnapshotEquivalence is the equivalence gate: a Rebuild that resumes
// from a verified state snapshot lands the exact tables a full fold would, and the
// snapshot is genuinely consulted (the fold starts after it, not from seq 0).
func TestStateRebuildFromSnapshotEquivalence(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, ":memory:", WithSnapshotCodec(stateSealer(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	populateSQLiteState(t, store)
	if err := store.SnapshotState(ctx); err != nil {
		t.Fatal(err)
	}
	// Suffix mutations after the snapshot must be folded on top of it.
	if _, err := store.Sessions().Create(ctx, state.Session{Title: "after"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy v2", Body: "z"}); err != nil {
		t.Fatal(err)
	}
	live := dumpState(t, store)

	if _, afterSeq := store.stateSnapshotForRebuild(ctx); afterSeq == 0 {
		t.Fatal("the snapshot was not consulted: Rebuild would fold from seq 0")
	}
	if err := store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if got := dumpState(t, store); got != live {
		t.Fatalf("rebuild from snapshot differs from the live projection:\n live=%s\n got=%s", live, got)
	}
}

// TestStateRebuildTamperedSnapshotFallsBack asserts a state snapshot the codec cannot
// verify is skipped and the stream folds from the start, so the rebuilt tables are still
// correct.
func TestStateRebuildTamperedSnapshotFallsBack(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, ":memory:", WithSnapshotCodec(stateSealer(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	populateSQLiteState(t, store)
	if err := store.SnapshotState(ctx); err != nil {
		t.Fatal(err)
	}
	live := dumpState(t, store)

	// Corrupt the stored sealed snapshot in place.
	snap, found, err := store.Log().LatestSnapshot(ctx, state.StateStream, 0)
	if err != nil || !found {
		t.Fatalf("no snapshot to tamper (found=%v err=%v)", found, err)
	}
	snap.Payload = append([]byte(nil), snap.Payload...)
	snap.Payload[len(snap.Payload)/2] ^= 0x01
	if err := store.Log().SaveSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}

	if _, afterSeq := store.stateSnapshotForRebuild(ctx); afterSeq != 0 {
		t.Fatal("a tampered snapshot was accepted instead of falling back to a full fold")
	}
	if err := store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if got := dumpState(t, store); got != live {
		t.Fatalf("rebuild after tamper differs from the live projection:\n live=%s\n got=%s", live, got)
	}
}

// BenchmarkStateRebuild is the two-point flatness gate for state snapshots: history grows
// (the same skill is revised n times) while the live projection stays a single record, and
// a snapshot sits at the head. A rebuild then restores the one record and folds an empty
// suffix, so its cost stays flat as history grows. Run at history sizes n and 2n; the
// per-op time should not roughly double the way a fold from seq 0 would.
func BenchmarkStateRebuild(b *testing.B) {
	for _, n := range []int{200, 400} {
		b.Run(fmt.Sprintf("history=%d", n), func(b *testing.B) {
			ctx := context.Background()
			store, err := Open(ctx, ":memory:", WithSnapshotCodec(benchSealer(b)))
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			for i := range n {
				if _, err := store.Skills().Upsert(ctx, state.Skill{Slug: "same", Name: fmt.Sprintf("v%d", i), Body: "x"}); err != nil {
					b.Fatal(err)
				}
			}
			if err := store.SnapshotState(ctx); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for range b.N {
				if err := store.Rebuild(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchSealer builds a snapshot sealer for benchmarks, mirroring stateSealer without a
// *testing.T.
func benchSealer(b *testing.B) *chain.SnapshotSealer {
	b.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 0x3c
	}
	priv := ed25519.NewKeyFromSeed(seed)
	signer, err := chain.NewEd25519RootSigner("inst-1", priv)
	if err != nil {
		b.Fatal(err)
	}
	ring := chain.NewRootKeyring()
	if err := ring.Add("inst-1", priv.Public().(ed25519.PublicKey)); err != nil {
		b.Fatal(err)
	}
	sealer, err := chain.NewSnapshotSealer(signer, ring, nil)
	if err != nil {
		b.Fatal(err)
	}
	return sealer
}
