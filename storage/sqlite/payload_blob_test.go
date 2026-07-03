package sqlite_test

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// bigPayload builds a payload whose JSON encoding is comfortably over any small blob
// threshold, so it externalizes. The tag varies the body so callers can make distinct or
// identical bodies on demand.
func bigPayload(tag string) map[string]any {
	return map[string]any{"tag": tag, "body": strings.Repeat("x", 8192)}
}

// appendAll appends each input to the store's log in order and returns the stored events.
func appendAll(t *testing.T, s *sqlite.Store, ins []spine.AppendInput) []spine.Event {
	t.Helper()
	ctx := context.Background()
	out := make([]spine.Event, len(ins))
	for i, in := range ins {
		e, err := s.Log().Append(ctx, in)
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		out[i] = e
	}
	return out
}

// TestPayloadBlobRehydratesIdentically is the core Layer 5 invariant: externalizing a
// large payload into the content-addressed blob store changes only where the body is
// held, never what the log records. Two stores fed identical inputs under a fixed clock -
// one keeping every payload inline, one externalizing them - must read back byte-equal
// events and, folded into a Merkle tree, prove the same root. If externalization moved
// the root, it would have changed the signed record; this asserts it does not.
func TestPayloadBlobRehydratesIdentically(t *testing.T) {
	ctx := context.Background()
	const stream = "run/blobs"
	at := time.Unix(1_700_000_000, 0).UTC()

	ins := make([]spine.AppendInput, 8)
	for i := range ins {
		ins[i] = spine.AppendInput{
			Stream:  stream,
			Type:    "tool.result",
			Actor:   spine.ActorAgent,
			Time:    at,
			Payload: bigPayload(fmt.Sprintf("event-%d", i)),
		}
	}

	// Inline store: threshold 0 keeps every payload in the events table.
	inlineStore, err := sqlite.Open(ctx, ":memory:",
		sqlite.WithClock(clock.NewManual(at)), sqlite.WithPayloadBlobThreshold(0))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inlineStore.Close() }()

	// Externalized store: threshold 1 forces every non-empty payload into blobs.
	extStore, err := sqlite.Open(ctx, ":memory:",
		sqlite.WithClock(clock.NewManual(at)), sqlite.WithPayloadBlobThreshold(1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = extStore.Close() }()

	appendAll(t, inlineStore, ins)
	appendAll(t, extStore, ins)

	// Read back through the log and compare the rehydrated payloads event by event.
	inlineRead, err := inlineStore.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	extRead, err := extStore.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	if len(extRead) != len(ins) {
		t.Fatalf("read back %d events, want %d", len(extRead), len(ins))
	}
	for i := range extRead {
		wantCB, err := chain.CanonicalBytes(inlineRead[i])
		if err != nil {
			t.Fatal(err)
		}
		gotCB, err := chain.CanonicalBytes(extRead[i])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(wantCB, gotCB) {
			t.Fatalf("event %d: externalized payload rehydrated to different canonical bytes", i)
		}
	}

	// Fold both streams into a tree and compare the roots: the externalized log proves
	// the same commitment as the inline one.
	inlineRoot := rootOf(t, inlineStore, stream, inlineRead)
	extRoot := rootOf(t, extStore, stream, extRead)
	if !bytes.Equal(inlineRoot, extRoot) {
		t.Fatal("externalized log root differs from the inline log root")
	}

	// The externalized store actually externalized (and the inline one did not), so the
	// two roots agreeing is a real cross-storage result, not both taking the same path.
	distinct, extBytes, refs, err := extStore.PayloadBlobStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if distinct != int64(len(ins)) || refs != int64(len(ins)) || extBytes == 0 {
		t.Fatalf("externalized store stats = (distinct=%d, bytes=%d, refs=%d), want %d distinct bodies referenced once each", distinct, extBytes, refs, len(ins))
	}
	if d, _, _, err := inlineStore.PayloadBlobStats(ctx); err != nil || d != 0 {
		t.Fatalf("inline store held %d blobs (err=%v), want 0", d, err)
	}
}

// rootOf folds a stream's events into a fresh Merkle tree backed by the store's node
// store and returns the root, the commitment a checkpoint signs.
func rootOf(t *testing.T, s *sqlite.Store, stream string, events []spine.Event) []byte {
	t.Helper()
	tree := chain.NewTreeWithStore(s.MerkleNodes(stream))
	for _, e := range events {
		cb, err := chain.CanonicalBytes(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := tree.Append(cb); err != nil {
			t.Fatal(err)
		}
	}
	root, err := tree.Root()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestPayloadBlobDedup proves the content-addressed store holds one row per distinct
// body: two events carrying an identical large payload share a single blob referenced
// twice, and a third event with a different body adds a second row. This is the property
// that bounds hot growth to distinct bodies rather than their repetition.
func TestPayloadBlobDedup(t *testing.T) {
	ctx := context.Background()
	const stream = "run/dedup"
	s, err := sqlite.Open(ctx, ":memory:", sqlite.WithPayloadBlobThreshold(1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	same := bigPayload("repeated")
	appendAll(t, s, []spine.AppendInput{
		{Stream: stream, Type: "tool.result", Actor: spine.ActorAgent, Payload: same},
		{Stream: stream, Type: "tool.result", Actor: spine.ActorAgent, Payload: same},
	})
	distinct, _, refs, err := s.PayloadBlobStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if distinct != 1 || refs != 2 {
		t.Fatalf("after two identical bodies: distinct=%d refs=%d, want distinct=1 refs=2", distinct, refs)
	}

	appendAll(t, s, []spine.AppendInput{
		{Stream: stream, Type: "tool.result", Actor: spine.ActorAgent, Payload: bigPayload("different")},
	})
	distinct, _, refs, err = s.PayloadBlobStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if distinct != 2 || refs != 3 {
		t.Fatalf("after a distinct body: distinct=%d refs=%d, want distinct=2 refs=3", distinct, refs)
	}

	// All three events still read back with their exact payloads.
	got, err := s.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d events, want 3", len(got))
	}
	if got[0].Payload["tag"] != "repeated" || got[2].Payload["tag"] != "different" {
		t.Fatalf("payloads rehydrated wrong: %v / %v", got[0].Payload["tag"], got[2].Payload["tag"])
	}
}

// TestPayloadBlobInlineBelowThreshold confirms a payload under the threshold stays inline:
// no blob row is written and the event still reads back intact. Small control-plane events
// must not pay the externalization round trip.
func TestPayloadBlobInlineBelowThreshold(t *testing.T) {
	ctx := context.Background()
	const stream = "run/inline"
	s, err := sqlite.Open(ctx, ":memory:", sqlite.WithPayloadBlobThreshold(4096))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	appendAll(t, s, []spine.AppendInput{
		{Stream: stream, Type: "heartbeat", Actor: spine.ActorSystem, Payload: map[string]any{"ok": true}},
	})
	distinct, _, _, err := s.PayloadBlobStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if distinct != 0 {
		t.Fatalf("small payload externalized %d blobs, want 0", distinct)
	}
	got, err := s.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Payload["ok"] != true {
		t.Fatalf("inline payload read back wrong: %+v", got)
	}
}

// TestPayloadBlobPersistsAcrossReopen closes a store holding an externalized payload and
// reopens it from the same file: the body survives in the blob table and rehydrates to
// the exact payload on read. The durable path, not just the in-process one, must keep the
// separated body.
func TestPayloadBlobPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "store.db")
	const stream = "run/reopen"
	want := bigPayload("survives-reopen")

	s1, err := sqlite.Open(ctx, dsn, sqlite.WithPayloadBlobThreshold(1))
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, s1, []spine.AppendInput{
		{Stream: stream, Type: "tool.result", Actor: spine.ActorAgent, Payload: want},
	})
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	got, err := s2.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Payload["tag"] != "survives-reopen" ||
		got[0].Payload["body"] != want["body"] {
		t.Fatalf("payload did not survive reopen: %+v", got)
	}
}
