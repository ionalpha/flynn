package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/spine"
)

// archivedOneBody opens an externalizing store, appends a single big-payload event to
// stream, seals it, and relocates its body into the warm store. It returns the store and
// the body's content id, so a test can then corrupt or drop the warm copy and assert the
// read fails loudly rather than yielding an empty payload.
func archivedOneBody(t *testing.T, stream string) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()
	s, err := Open(ctx, ":memory:", WithClock(clock.NewManual(at)), WithPayloadBlobThreshold(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Log().Append(ctx, spine.AppendInput{
		Stream:  stream,
		Type:    "tool.result",
		Actor:   spine.ActorAgent,
		Time:    at,
		Payload: map[string]any{"body": strings.Repeat("x", 8192)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckpoint(ctx, stream, 1, []byte("cose")); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := s.reads().QueryRowContext(ctx, `SELECT content_id FROM blobs LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if moved, err := s.ArchiveSealedBlobs(ctx); err != nil || moved != 1 {
		t.Fatalf("archive moved %d (err=%v), want 1", moved, err)
	}
	return s, id
}

// TestWarmMissingBodyFailsLoudly proves a body that is in neither tier is a named
// verification failure, never a silent gap: after the body is archived and then removed
// from the warm store, reading the stream errors and the error names the absent content
// id, so a caller learns exactly which body is unavailable.
func TestWarmMissingBodyFailsLoudly(t *testing.T) {
	ctx := context.Background()
	const stream = "run/missing"
	s, id := archivedOneBody(t, stream)
	defer func() { _ = s.Close() }()

	if _, err := s.warm.db.ExecContext(ctx, `DELETE FROM warm_blobs WHERE content_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	_, err := s.Log().Read(ctx, spine.Query{Stream: stream})
	if err == nil {
		t.Fatal("read of a stream whose body is in neither tier succeeded, want a named failure")
	}
	if !strings.Contains(err.Error(), id) {
		t.Fatalf("error %q does not name the missing content id %q", err, id)
	}
}

// TestWarmCorruptBodyFailsLoudly proves a corrupt warm record is surfaced as an error, not
// decoded into wrong bytes: overwriting an archived body with non-zstd garbage makes the
// read fail at decompression rather than returning a truncated or wrong payload.
func TestWarmCorruptBodyFailsLoudly(t *testing.T) {
	ctx := context.Background()
	const stream = "run/corrupt"
	s, id := archivedOneBody(t, stream)
	defer func() { _ = s.Close() }()

	if _, err := s.warm.db.ExecContext(ctx,
		`UPDATE warm_blobs SET body = ? WHERE content_id = ?`, []byte("not zstd at all"), id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Log().Read(ctx, spine.Query{Stream: stream}); err == nil {
		t.Fatal("read of a corrupt warm body succeeded, want a decompress failure")
	}
}

// TestCompressRoundTrip is the codec contract: any bytes compressed by compressBody
// decompress back to the identical bytes, and the empty body round-trips too.
func TestCompressRoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		[]byte("small"),
		[]byte(strings.Repeat("repetitive json ", 4096)),
	}
	for i, raw := range cases {
		got, err := decompressBody(compressBody(raw))
		if err != nil {
			t.Fatalf("case %d: decompress: %v", i, err)
		}
		if string(got) != string(raw) {
			t.Fatalf("case %d: round-trip changed the bytes", i)
		}
	}
}
