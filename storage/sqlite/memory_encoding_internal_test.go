package sqlite

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ionalpha/flynn/state"
)

// Provenance is stored as JSON in one column, so the encode/decode pair is the
// only thing standing between a list of sources and a corrupted read. The round
// trip has to hold for the shapes the store actually sees, and the empty case has
// to stay distinguishable from a list holding one empty string.
func TestMemorySourcesRoundTrip(t *testing.T) {
	for _, sources := range [][]string{
		nil,
		{},
		{"chat"},
		{"user:operator", "tool:web-fetch", "agent:run-7"},
		{`a "quoted" source`, "a,comma"}, // the characters a naive join would break on
	} {
		raw := encodeSources(sources)
		back, err := decodeSources(raw)
		if err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		if len(sources) == 0 {
			if raw != "" {
				t.Errorf("empty provenance encoded to %q, want the empty column value", raw)
			}
			if len(back) != 0 {
				t.Errorf("empty provenance decoded to %v, want none", back)
			}
			continue
		}
		if !slices.Equal(back, sources) {
			t.Errorf("round trip of %v = %v", sources, back)
		}
	}
}

// A sources column that is not JSON is a corrupted row, and the read has to say so
// rather than hand back an item whose provenance silently vanished.
func TestDecodeSourcesReportsCorruptJSON(t *testing.T) {
	got, err := decodeSources(`{"not":"a list"}`)
	if err == nil {
		t.Fatalf("decode of a non-list returned %v and no error", got)
	}
	if got != nil {
		t.Fatalf("decode returned items %v alongside its error, want none", got)
	}
}

// Expiry is stored as unix nanoseconds with 0 reserved for "never", so the mapping
// has to be exact in both directions and must not turn a real expiry into never.
func TestExpiryNanosRoundTrip(t *testing.T) {
	if got := expiryNanos(time.Time{}); got != 0 {
		t.Errorf("the zero time encoded to %d, want 0 (never)", got)
	}
	if got := expiryTime(0); !got.IsZero() {
		t.Errorf("0 decoded to %v, want the zero time", got)
	}
	at := time.Date(2026, 3, 1, 12, 0, 0, 123456789, time.UTC)
	back := expiryTime(expiryNanos(at))
	if !back.Equal(at) {
		t.Errorf("round trip of %v = %v (nanosecond precision must survive)", at, back)
	}
	// An expiry in the past is an ordinary value, not a sentinel: an already-dead
	// item is stored and read back as dead, not as never-expiring.
	past := time.Date(1970, 1, 1, 0, 0, 0, 1, time.UTC)
	if n := expiryNanos(past); n == 0 {
		t.Error("an expiry one nanosecond after the epoch collided with the never sentinel")
	}
}

// A sources column that is not a JSON list is a corrupted row. Recall has to
// surface that as an error: handing back an item whose provenance silently
// vanished would let a purge that works by source miss it, which is exactly the
// failure provenance exists to prevent.
func TestRecallReportsACorruptSourcesColumn(t *testing.T) {
	ctx := context.Background()
	p, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	mem := p.Memory()
	it, err := mem.Write(ctx, state.MemoryItem{
		Kind: "fact", Content: "corruptible", Sources: []string{"chat"},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := p.db.ExecContext(ctx,
		`UPDATE memory_items SET sources = ? WHERE id = ?`, `{"not":"a list"}`, it.ID); err != nil {
		t.Fatalf("corrupt the row: %v", err)
	}

	// Both recall shapes read the column, so both have to report it.
	if _, err := mem.Recall(ctx, state.RecallQuery{Scope: state.Scope{}, Query: "corruptible"}); err == nil {
		t.Error("full-text recall over a corrupt sources column returned no error")
	}
	if _, err := mem.Recall(ctx, state.RecallQuery{Scope: state.Scope{Project: "p"}}); err != nil {
		t.Errorf("recall of an unaffected scope must still work: %v", err)
	}
}
