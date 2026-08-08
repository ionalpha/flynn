package sqlite

// The tiers below the hot log: the content-addressed blob store and the warm store. A
// large payload is never silently downgraded to an inline write, and an unwritable warm
// store fails archival instead of losing the bodies it was moving. With no warm tier
// wired at all, the accessors are a quiet no-op, so a caller without one special-cases
// nothing.

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/spine"
)

// TestExternalizedAppendFailsWithNoBlobStore proves a large payload is never silently
// downgraded to an inline write: with externalization on and the content-addressed store
// gone, the append fails rather than committing an event whose body was dropped.
func TestExternalizedAppendFailsWithNoBlobStore(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, WithPayloadBlobThreshold(1))
	dropTables(t, s, "blobs")

	_, err := s.Log().Append(ctx, spine.AppendInput{
		Stream: "run/blob", Type: "tool.result", Actor: spine.ActorAgent, Time: testAt,
		Payload: map[string]any{"body": strings.Repeat("x", 8192)},
	})
	if err == nil {
		t.Fatal("externalized append succeeded with no blobs table, want a failure")
	}
}

// TestArchiveFailsWhenTheWarmStoreIsUnwritable is the copy-then-delete guarantee stated as
// a failure: a body is only deleted from hot once its warm copy is durable, so if the warm
// write fails the sweep must abort with the body still hot, never delete it into a gap.
func TestArchiveFailsWhenTheWarmStoreIsUnwritable(t *testing.T) {
	ctx := context.Background()
	const stream = "run/unwritable"
	s := newStore(t, WithPayloadBlobThreshold(1))
	if _, err := s.Log().Append(ctx, spine.AppendInput{
		Stream: stream, Type: "tool.result", Actor: spine.ActorAgent, Time: testAt,
		Payload: map[string]any{"body": strings.Repeat("x", 8192)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckpoint(ctx, stream, 1, []byte("cose")); err != nil {
		t.Fatal(err)
	}
	// Break only the warm half: the hot store stays fully usable, so a body deleted
	// from hot here would be a body lost.
	if err := s.warm.db.Close(); err != nil {
		t.Fatal(err)
	}

	if moved, err := s.ArchiveSealedBlobs(ctx); err == nil {
		t.Fatalf("archive moved %d bodies with an unwritable warm store, want a failure", moved)
	}
	// The body is still hot, and the stream still reads.
	if distinct, _, _, err := s.PayloadBlobStats(ctx); err != nil || distinct != 1 {
		t.Fatalf("hot bodies after the failed sweep = %d (err=%v), want 1 (nothing deleted)", distinct, err)
	}
	if evs, err := s.Log().Read(ctx, spine.Query{Stream: stream}); err != nil || len(evs) != 1 {
		t.Fatalf("stream read after the failed sweep = %d events (err=%v), want 1", len(evs), err)
	}
	// No retention event was recorded: a sweep that moved nothing records nothing.
	evs, err := s.Log().Read(ctx, spine.Query{Stream: "retention"})
	if err != nil || len(evs) != 0 {
		t.Fatalf("retention events after the failed sweep = %d (err=%v), want 0", len(evs), err)
	}
}

// TestNoWarmTierIsAQuietNoOp covers the degenerate configuration the warm accessors
// document: with no warm store wired, its accounting is zero and archival moves nothing,
// both without an error, so a caller with no warm tier is not forced to special-case it.
func TestNoWarmTierIsAQuietNoOp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, WithPayloadBlobThreshold(1))
	if err := s.warm.close(); err != nil {
		t.Fatal(err)
	}
	s.warm = nil

	count, raw, packed, err := s.WarmBlobStats(ctx)
	if err != nil || count != 0 || raw != 0 || packed != 0 {
		t.Fatalf("WarmBlobStats with no warm tier = (%d,%d,%d,%v), want all zero and no error", count, raw, packed, err)
	}
	moved, err := s.ArchiveSealedBlobs(ctx)
	if err != nil || moved != 0 {
		t.Fatalf("ArchiveSealedBlobs with no warm tier = (%d,%v), want (0,nil)", moved, err)
	}
}

// TestWarmGetAndHasReportAbsence pins the warm store's miss semantics: an id it does not
// hold is reported absent, not as an error, so a reader can tell "not archived here" from
// "the archive is broken" and name the id that is genuinely gone.
func TestWarmGetAndHasReportAbsence(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	present, err := s.warm.has(ctx, "deadbeef")
	if err != nil || present {
		t.Fatalf("has(unknown) = (%v,%v), want (false,nil)", present, err)
	}
	body, ok, err := s.warm.get(ctx, "deadbeef")
	if err != nil || ok || body != nil {
		t.Fatalf("get(unknown) = (%v,%v,%v), want (nil,false,nil)", body, ok, err)
	}
}
