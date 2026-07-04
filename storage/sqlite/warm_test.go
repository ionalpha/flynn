package sqlite_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// sealedStore opens an externalizing store, appends n distinct big-payload events to
// stream, and returns the store plus the events read back before any archival. Every
// payload is over the blob threshold, so each externalizes into its own hot blob.
func sealedStore(t *testing.T, n int, stream string) (*sqlite.Store, []spine.Event) {
	t.Helper()
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()
	s, err := sqlite.Open(ctx, ":memory:",
		sqlite.WithClock(clock.NewManual(at)), sqlite.WithPayloadBlobThreshold(1))
	if err != nil {
		t.Fatal(err)
	}
	ins := make([]spine.AppendInput, n)
	for i := range ins {
		ins[i] = spine.AppendInput{
			Stream:  stream,
			Type:    "tool.result",
			Actor:   spine.ActorAgent,
			Time:    at,
			Payload: bigPayload(fmt.Sprintf("event-%d", i)),
		}
	}
	appendAll(t, s, ins)
	before, err := s.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	return s, before
}

// canonicalEqual asserts two event slices fold to byte-identical canonical bytes event by
// event: the record a Merkle tree commits to is unchanged.
func canonicalEqual(t *testing.T, want, got []spine.Event) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("event count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		wantCB, err := chain.CanonicalBytes(want[i])
		if err != nil {
			t.Fatal(err)
		}
		gotCB, err := chain.CanonicalBytes(got[i])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(wantCB, gotCB) {
			t.Fatalf("event %d rehydrated to different canonical bytes across tiers", i)
		}
	}
}

// TestArchiveSealedBlobsRehydratesIdentically is the core warm-tier invariant: relocating
// a sealed body from the hot blob table into the compressed warm store changes only where
// the body is held, never what the log reads back. After every body has moved out of hot,
// the same stream reads back byte-identical events - the warm store decompresses each to
// the exact bytes the chain already committed to.
func TestArchiveSealedBlobsRehydratesIdentically(t *testing.T) {
	ctx := context.Background()
	const stream = "run/warm"
	s, before := sealedStore(t, 6, stream)
	defer func() { _ = s.Close() }()

	// Seal the whole stream: a checkpoint at size 6 makes every event sealed history.
	if err := s.SaveCheckpoint(ctx, stream, 6, []byte("cose")); err != nil {
		t.Fatal(err)
	}

	// All six bodies are hot before archival.
	if distinct, _, _, err := s.PayloadBlobStats(ctx); err != nil || distinct != 6 {
		t.Fatalf("hot blobs before archive = %d (err=%v), want 6", distinct, err)
	}

	moved, err := s.ArchiveSealedBlobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 6 {
		t.Fatalf("archived %d bodies, want 6", moved)
	}

	// Hot is now empty; warm holds all six, compressed smaller than raw.
	if distinct, _, _, err := s.PayloadBlobStats(ctx); err != nil || distinct != 0 {
		t.Fatalf("hot blobs after archive = %d (err=%v), want 0", distinct, err)
	}
	count, rawBytes, packedBytes, err := s.WarmBlobStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 6 {
		t.Fatalf("warm bodies = %d, want 6", count)
	}
	if packedBytes == 0 || packedBytes >= rawBytes {
		t.Fatalf("warm compression: packed=%d raw=%d, want 0 < packed < raw", packedBytes, rawBytes)
	}

	// The stream reads back identically with every body served from the warm tier.
	after, err := s.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	canonicalEqual(t, before, after)
}

// TestArchiveLeavesUnsealedBlobsHot proves the tier only moves provably closed history: a
// body is eligible only when every event referencing it is sealed under a checkpoint. With
// the stream sealed to size 3 of 6, exactly the first three bodies relocate and the rest
// stay hot, yet the whole stream still reads back identically across the split.
func TestArchiveLeavesUnsealedBlobsHot(t *testing.T) {
	ctx := context.Background()
	const stream = "run/split"
	s, before := sealedStore(t, 6, stream)
	defer func() { _ = s.Close() }()

	// Seal only the first three events (seq 1..3); events 4..6 stay unsealed.
	if err := s.SaveCheckpoint(ctx, stream, 3, []byte("cose")); err != nil {
		t.Fatal(err)
	}

	moved, err := s.ArchiveSealedBlobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 3 {
		t.Fatalf("archived %d bodies, want 3 (only the sealed ones)", moved)
	}
	if distinct, _, _, err := s.PayloadBlobStats(ctx); err != nil || distinct != 3 {
		t.Fatalf("hot blobs after archive = %d (err=%v), want 3 unsealed still hot", distinct, err)
	}
	if count, _, _, err := s.WarmBlobStats(ctx); err != nil || count != 3 {
		t.Fatalf("warm bodies = %d (err=%v), want 3", count, err)
	}

	after, err := s.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	canonicalEqual(t, before, after)
}

// TestArchiveWithoutCheckpointMovesNothing confirms the conservative default: a stream with
// no checkpoint has sealed size 0, so none of its bodies are eligible and archival is a
// no-op that leaves every body hot and readable.
func TestArchiveWithoutCheckpointMovesNothing(t *testing.T) {
	ctx := context.Background()
	const stream = "run/unsealed"
	s, before := sealedStore(t, 4, stream)
	defer func() { _ = s.Close() }()

	moved, err := s.ArchiveSealedBlobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 0 {
		t.Fatalf("archived %d bodies with no checkpoint, want 0", moved)
	}
	if distinct, _, _, err := s.PayloadBlobStats(ctx); err != nil || distinct != 4 {
		t.Fatalf("hot blobs = %d (err=%v), want 4 (nothing sealed)", distinct, err)
	}
	after, err := s.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	canonicalEqual(t, before, after)
}
