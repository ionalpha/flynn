package spine_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/spine"
)

// Materialize is the single place both logs build the stored Event from an
// AppendInput. The backends exercise it through spinetest.RunSuite; these tests
// pin the contract directly - defaulting, seq passthrough, the payload forms, and
// the payload-immutability clone.

func TestMaterializeRejectsBothPayloadForms(t *testing.T) {
	in := spine.AppendInput{
		Stream: "run", Type: "t",
		Payload:    map[string]any{"a": float64(1)},
		RawPayload: json.RawMessage(`{"a":1}`),
	}
	if _, _, err := in.Materialize(clock.NewManual(time.Unix(1, 0)), 1); !errors.Is(err, spine.ErrPayloadConflict) {
		t.Fatalf("want ErrPayloadConflict, got %v", err)
	}
}

func TestMaterializeDefaultsAndSeq(t *testing.T) {
	at := time.Unix(4242, 0).UTC()
	in := spine.AppendInput{Stream: "run", Type: "t", Actor: spine.ActorAgent}
	e, raw, err := in.Materialize(clock.NewManual(at), 7)
	if err != nil {
		t.Fatal(err)
	}
	if e.Seq != 7 {
		t.Errorf("seq = %d, want 7", e.Seq)
	}
	if !e.Time.Equal(at) {
		t.Errorf("time = %v, want clock now %v", e.Time, at)
	}
	if e.SchemaVersion != spine.DefaultSchemaVersion {
		t.Errorf("version = %d, want default %d", e.SchemaVersion, spine.DefaultSchemaVersion)
	}
	// No payload stores as JSON null and decodes back to a nil map, so an event
	// with no payload round-trips identically through either backend.
	if string(raw) != "null" {
		t.Errorf("raw = %q, want null", raw)
	}
	if e.Payload != nil {
		t.Errorf("payload = %v, want nil", e.Payload)
	}
}

func TestMaterializePreservesSetFields(t *testing.T) {
	at := time.Unix(100, 0).UTC()
	in := spine.AppendInput{
		Stream: "run", Type: "t", Actor: spine.ActorHuman,
		Time: at, SchemaVersion: 3,
		TraceID: "tr", SpanID: "sp", CausationID: "ca",
		OriginInstanceID: "inst", Principal: "who",
	}
	// A non-zero Time and SchemaVersion win over the clock/default.
	e, _, err := in.Materialize(clock.NewManual(time.Unix(0, 0)), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Time.Equal(at) || e.SchemaVersion != 3 || e.Actor != spine.ActorHuman ||
		e.TraceID != "tr" || e.SpanID != "sp" || e.CausationID != "ca" ||
		e.OriginInstanceID != "inst" || e.Principal != "who" {
		t.Fatalf("field not carried through: %+v", e)
	}
}

func TestMaterializeStoresRawPayloadVerbatim(t *testing.T) {
	raw := json.RawMessage(`{"k":"v","n":5}`)
	in := spine.AppendInput{Stream: "run", Type: "t", RawPayload: raw}
	e, gotRaw, err := in.Materialize(clock.NewManual(time.Unix(1, 0)), 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotRaw) != string(raw) {
		t.Errorf("raw = %q, want verbatim %q", gotRaw, raw)
	}
	if e.Payload["k"] != "v" {
		t.Errorf("decoded payload = %v, want k=v", e.Payload)
	}
}

func TestMaterializeClonesPayload(t *testing.T) {
	src := map[string]any{"k": "v"}
	in := spine.AppendInput{Stream: "run", Type: "t", Payload: src}
	e, _, err := in.Materialize(clock.NewManual(time.Unix(1, 0)), 1)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the returned event's payload must not touch the caller's map: an
	// event is immutable once built.
	e.Payload["k"] = "changed"
	if src["k"] != "v" {
		t.Errorf("input map mutated through returned event: %v", src)
	}
}
