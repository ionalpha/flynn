package state_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ionalpha/flynn/state"
)

// Expiry's boundary is exact, and the conformance suite cannot reach it: that
// suite runs against whatever clock a backend holds, so it can only assert an
// item an hour dead and an item an hour alive. The instant itself, which is what
// "half-open" actually means, is only testable against the predicate directly.
func TestMemoryItemExpiredAtBoundary(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	item := state.MemoryItem{ExpiresAt: at}

	for _, c := range []struct {
		what string
		now  time.Time
		want bool
	}{
		{"a nanosecond before expiry", at.Add(-time.Nanosecond), false},
		{"the expiry instant itself, which is inclusive", at, true},
		{"a nanosecond after expiry", at.Add(time.Nanosecond), true},
	} {
		if got := item.ExpiredAt(c.now); got != c.want {
			t.Errorf("ExpiredAt %s = %v, want %v", c.what, got, c.want)
		}
	}

	// The zero ExpiresAt never expires, however far the clock has run. This is the
	// default every existing writer gets, so an item that never opted into expiry
	// must not acquire one.
	never := state.MemoryItem{}
	if never.ExpiredAt(at) || never.ExpiredAt(at.AddDate(100, 0, 0)) {
		t.Error("an item with no ExpiresAt must never expire")
	}
}

// Selects is where a backend filtering in Go gets expiry for free. It has to
// reject an expired item regardless of what else the query asked for, so a query
// that happens to match on kind and window cannot pull a dead item through.
func TestRecallQuerySelectsRejectsExpired(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	live := state.MemoryItem{Kind: "fact", CreatedAt: now.Add(-time.Hour)}
	dead := live
	dead.ExpiresAt = now.Add(-time.Minute)

	q := state.RecallQuery{Kinds: []string{"fact"}, Since: now.Add(-2 * time.Hour)}
	if !q.Selects(live, now) {
		t.Fatal("a live item matching the kind filter and window must be selected")
	}
	if q.Selects(dead, now) {
		t.Fatal("an expired item must be rejected even when it matches every other selector")
	}
	// And it is expiry doing the rejecting, not the window: the same item is
	// selected again once the clock is rolled back before its expiry.
	if !q.Selects(dead, now.Add(-time.Hour)) {
		t.Fatal("the item was rejected for something other than expiry")
	}
}

// A memory item written before provenance became a list is still on the spine, and
// a rebuild replays it. Decoding it into an item with no provenance would rewrite
// the history the replay exists to reproduce.
func TestMemoryItemUnmarshalAcceptsLegacySource(t *testing.T) {
	var legacy state.MemoryItem
	if err := json.Unmarshal([]byte(`{"ID":"m1","Content":"c","Source":"chat"}`), &legacy); err != nil {
		t.Fatalf("decode legacy payload: %v", err)
	}
	if len(legacy.Sources) != 1 || legacy.Sources[0] != "chat" {
		t.Fatalf("legacy Source decoded to %v, want [chat]", legacy.Sources)
	}
	if legacy.ID != "m1" || legacy.Content != "c" {
		t.Fatalf("legacy payload lost fields: %+v", legacy)
	}

	// A payload carrying both is a current writer's, so the list wins and the old
	// field is not appended to it.
	var both state.MemoryItem
	if err := json.Unmarshal([]byte(`{"Sources":["a","b"],"Source":"chat"}`), &both); err != nil {
		t.Fatalf("decode mixed payload: %v", err)
	}
	if len(both.Sources) != 2 || both.Sources[0] != "a" || both.Sources[1] != "b" {
		t.Fatalf("mixed payload decoded to %v, want [a b]", both.Sources)
	}

	// A current payload round-trips, and encoding no longer emits the old field, so
	// nothing downstream can start depending on it again.
	enc, err := json.Marshal(state.MemoryItem{Sources: []string{"user:op"}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back state.MemoryItem
	if err := json.Unmarshal(enc, &back); err != nil {
		t.Fatalf("decode round trip: %v", err)
	}
	if len(back.Sources) != 1 || back.Sources[0] != "user:op" {
		t.Fatalf("round trip = %v, want [user:op]", back.Sources)
	}

	// Malformed JSON is reported, not swallowed into a zero item.
	if err := json.Unmarshal([]byte(`{"Sources":"not-a-list"}`), &back); err == nil {
		t.Fatal("decode of a malformed payload returned nil error")
	}
}
