package envelope

import (
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/spine"
)

func at(wall int64) hlc.Time { return hlc.Time{Wall: wall} }

func TestStampCreate(t *testing.T) {
	var e Envelope
	StampCreate(&e, "w1", at(10))
	if e.SyncVersion != 1 || e.OriginInstanceID != "w1" || e.LastWriterID != "w1" || e.Deleted {
		t.Fatalf("create stamp wrong: %+v", e)
	}

	// A synced create keeps the origin it arrived with.
	e = Envelope{OriginInstanceID: "elsewhere", Deleted: true}
	StampCreate(&e, "w1", at(11))
	if e.OriginInstanceID != "elsewhere" {
		t.Fatal("create must preserve a caller-attributed origin")
	}
	if e.Deleted {
		t.Fatal("a create is live, whatever the caller left in Deleted")
	}
}

func TestStampUpdateCarriesOriginAndBumpsFromStored(t *testing.T) {
	prev := Envelope{SyncVersion: 7, OriginInstanceID: "origin", LastWriterID: "old", UpdatedHLC: at(1)}
	// The caller sends a stale-looking envelope; identity comes from prev, never
	// from what was sent.
	e := Envelope{SyncVersion: 3, OriginInstanceID: "forged"}
	StampUpdate(&e, prev, "w2", at(2))
	if e.SyncVersion != 8 {
		t.Fatalf("SyncVersion = %d, want stored+1 = 8", e.SyncVersion)
	}
	if e.OriginInstanceID != "origin" || e.LastWriterID != "w2" || e.UpdatedHLC != at(2) {
		t.Fatalf("update stamp wrong: %+v", e)
	}
}

func TestStampTombstoneBumpsAndMarks(t *testing.T) {
	e := Envelope{SyncVersion: 2, OriginInstanceID: "o", LastWriterID: "old"}
	StampTombstone(&e, "w3", at(5))
	if !e.Deleted || e.SyncVersion != 3 || e.LastWriterID != "w3" || e.UpdatedHLC != at(5) {
		t.Fatalf("tombstone stamp wrong: %+v", e)
	}
	if e.OriginInstanceID != "o" {
		t.Fatal("tombstone must keep the origin")
	}
}

func TestCAS(t *testing.T) {
	stored := &Envelope{SyncVersion: 4}
	cases := []struct {
		name     string
		expected int64
		stored   *Envelope
		want     bool
	}{
		{"unconditional over existing", 0, stored, true},
		{"matching version", 4, stored, true},
		{"stale version", 3, stored, false},
		{"unconditional create", 0, nil, true},
		{"expected version but nothing stored", 4, nil, false},
	}
	for _, tc := range cases {
		if got := CAS(tc.expected, tc.stored); got != tc.want {
			t.Errorf("%s: CAS(%d) = %v, want %v", tc.name, tc.expected, got, tc.want)
		}
	}
}

// TestWireFormat pins the JSON keys of the shared envelope: records embed this
// struct with no tags, event logs replay these exact bytes, and fleet sync
// compares them across versions, so a renamed or tagged field here is a
// replay/sync break, not a refactor.
func TestWireFormat(t *testing.T) {
	e := Envelope{SyncVersion: 1, OriginInstanceID: "o", LastWriterID: "w", UpdatedHLC: at(9), Deleted: true}
	b, err := json.Marshal(struct{ Envelope }{e})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"SyncVersion", "OriginInstanceID", "UpdatedHLC", "LastWriterID", "Deleted"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("embedded envelope must serialize flat with key %q; got %s", key, b)
		}
	}
	if _, ok := m["Envelope"]; ok {
		t.Fatalf("embedding must inline, not nest: %s", b)
	}
}

func TestEventInput(t *testing.T) {
	in := EventInput("s", "t", spine.ActorAgent, "me", json.RawMessage(`{"a":1}`))
	if in.Stream != "s" || in.Type != "t" || in.Actor != spine.ActorAgent || in.OriginInstanceID != "me" || string(in.RawPayload) != `{"a":1}` {
		t.Fatalf("EventInput wrong: %+v", in)
	}
}
