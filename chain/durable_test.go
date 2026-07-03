package chain

import (
	"bytes"
	"context"
	"testing"

	"github.com/ionalpha/flynn/spine"
)

// durableFlushStore is an in-memory FlushNodeStore for tests: the tiled layout with a
// no-op Flush (nothing to page out in memory). Sharing one instance across two recorders
// simulates the tiles surviving a restart.
type durableFlushStore struct{ *tiledNodeStore }

func (durableFlushStore) Flush() error { return nil }

// fakeCheckpointStore keeps the latest checkpoint per stream and records every saved size
// so a test can assert the checkpoint cadence.
type fakeCheckpointStore struct {
	latest map[string]struct {
		size uint64
		cose []byte
	}
	saved map[string][]uint64
}

func newFakeCheckpointStore() *fakeCheckpointStore {
	return &fakeCheckpointStore{
		latest: map[string]struct {
			size uint64
			cose []byte
		}{},
		saved: map[string][]uint64{},
	}
}

func (f *fakeCheckpointStore) SaveCheckpoint(_ context.Context, stream string, size uint64, cose []byte) error {
	cur := f.latest[stream]
	if size >= cur.size {
		f.latest[stream] = struct {
			size uint64
			cose []byte
		}{size, append([]byte(nil), cose...)}
	}
	f.saved[stream] = append(f.saved[stream], size)
	return nil
}

func (f *fakeCheckpointStore) LatestCheckpoint(_ context.Context, stream string) (uint64, []byte, bool, error) {
	cur, ok := f.latest[stream]
	if !ok {
		return 0, nil, false, nil
	}
	return cur.size, cur.cose, true, nil
}

// referenceRoot folds a stream's events straight from the log into a fresh in-memory
// tree, the independent answer a durable recorder's head must match.
func referenceRoot(t *testing.T, log spine.Log, stream string) []byte {
	t.Helper()
	events, err := log.Read(context.Background(), spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	tr := NewTree()
	for _, e := range events {
		cb, cerr := CanonicalBytes(e)
		if cerr != nil {
			t.Fatal(cerr)
		}
		if aerr := tr.Append(cb); aerr != nil {
			t.Fatal(aerr)
		}
	}
	root, err := tr.Root()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func appendDurable(t *testing.T, rec *DurableRecorder, stream string, n int) {
	t.Helper()
	for range n {
		if _, err := rec.Append(context.Background(), spine.AppendInput{
			Stream: stream,
			Type:   "test.event",
			Actor:  spine.ActorSystem,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDurableRecorderEvictionReloads asserts a recorder capped below the number of
// streams it touches stays correct: evicted streams reload from their checkpoint plus the
// log, so every stream's final head still matches an independent fold and residency never
// exceeds the cap.
func TestDurableRecorderEvictionReloads(t *testing.T) {
	ctx := context.Background()
	priv, pub := testKey(0x51)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	ring := NewRootKeyring()
	if err := ring.Add("inst", pub); err != nil {
		t.Fatal(err)
	}

	log := spine.NewMemoryLog()
	stores := map[string]*durableFlushStore{}
	nodes := func(s string) FlushNodeStore {
		st, ok := stores[s]
		if !ok {
			st = &durableFlushStore{newTiledNodeStore()}
			stores[s] = st
		}
		return st
	}
	ckpts := newFakeCheckpointStore()
	const capN = 4
	rec := NewDurableRecorder(log, nodes, ckpts, signer, nil, 10).WithMaxResident(capN)

	streams := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	// Two interleaved passes so streams are revisited after being evicted.
	for range 2 {
		for _, s := range streams {
			appendDurable(t, rec, s, 15)
			if resident := len(rec.streams); resident > capN {
				t.Fatalf("resident streams = %d, want <= %d", resident, capN)
			}
		}
	}
	for _, s := range streams {
		if err := rec.Checkpoint(ctx, s); err != nil {
			t.Fatalf("checkpoint %s: %v", s, err)
		}
		_, cose, ok, err := ckpts.LatestCheckpoint(ctx, s)
		if err != nil || !ok {
			t.Fatalf("no checkpoint for %s (err=%v)", s, err)
		}
		cp, err := VerifyCheckpoint(cose, ring)
		if err != nil {
			t.Fatalf("checkpoint %s does not verify: %v", s, err)
		}
		if want := referenceRoot(t, log, s); !bytes.Equal(cp.RootHash, want) {
			t.Fatalf("stream %s head after eviction/reload does not match the log fold", s)
		}
	}
}

// TestDurableRecorderCadenceAndResume is the wiring gate: a durable recorder signs a
// checkpoint every K events and its head matches an independent fold of the log; after a
// restart it resumes from the latest checkpoint, catches the tree up from the log's
// uncheckpointed tail, keeps checkpointing, and a final Checkpoint seals the tip. Every
// signed checkpoint verifies, and the final head equals the reference fold over the whole
// stream.
func TestDurableRecorderCadenceAndResume(t *testing.T) {
	ctx := context.Background()
	priv, pub := testKey(0x33)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	ring := NewRootKeyring()
	if err := ring.Add("inst", pub); err != nil {
		t.Fatal(err)
	}

	const stream = "server/spine"
	log := spine.NewMemoryLog()
	stores := map[string]*durableFlushStore{}
	nodes := func(s string) FlushNodeStore {
		st, ok := stores[s]
		if !ok {
			st = &durableFlushStore{newTiledNodeStore()}
			stores[s] = st
		}
		return st
	}
	ckpts := newFakeCheckpointStore()

	// First process: append 230 events with a checkpoint every 50, so the last 30 are
	// past the final checkpoint (at size 200) and never sealed by this recorder.
	rec1 := NewDurableRecorder(log, nodes, ckpts, signer, nil, 50)
	appendDurable(t, rec1, stream, 230)

	wantSaved := []uint64{50, 100, 150, 200}
	if got := ckpts.saved[stream]; len(got) != len(wantSaved) {
		t.Fatalf("checkpoint sizes = %v, want %v", got, wantSaved)
	}
	for i, w := range wantSaved {
		if ckpts.saved[stream][i] != w {
			t.Fatalf("checkpoint sizes = %v, want %v", ckpts.saved[stream], wantSaved)
		}
	}

	// Restart: a new recorder over the same log, tiles, and checkpoints. Its first
	// append must resume from checkpoint 200 and catch up events 201..230 from the log.
	rec2 := NewDurableRecorder(log, nodes, ckpts, signer, nil, 50)
	appendDurable(t, rec2, stream, 90) // events 231..320; a checkpoint lands at 250 and 300
	if err := rec2.Checkpoint(ctx, stream); err != nil {
		t.Fatal(err)
	}

	size, cose, ok, err := ckpts.LatestCheckpoint(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || size != 320 {
		t.Fatalf("latest checkpoint size = %d (ok=%v), want 320", size, ok)
	}
	cp, err := VerifyCheckpoint(cose, ring)
	if err != nil {
		t.Fatalf("final checkpoint does not verify: %v", err)
	}
	if want := referenceRoot(t, log, stream); !bytes.Equal(cp.RootHash, want) {
		t.Fatal("durable recorder head does not match the independent fold of the log")
	}
	if cp.Size != 320 {
		t.Fatalf("signed size = %d, want 320", cp.Size)
	}
}
