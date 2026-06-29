package chain

import (
	"bytes"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/spine"
)

// drawEvent builds an arbitrary but well-formed event from rapid generators.
func drawEvent(rt *rapid.T) spine.Event {
	keys := rapid.SliceOfDistinct(rapid.StringMatching(`[a-z][a-z0-9_]{0,7}`),
		func(s string) string { return s }).Draw(rt, "payloadKeys")
	payload := map[string]any{}
	for _, k := range keys {
		payload[k] = rapid.Int64().Draw(rt, "v_"+k)
	}
	return spine.Event{
		Stream:        rapid.StringMatching(`[a-z][a-z0-9/]{0,15}`).Draw(rt, "stream"),
		Seq:           rapid.Int64Range(0, 1<<40).Draw(rt, "seq"),
		Time:          time.Unix(0, rapid.Int64Range(1, 1<<62).Draw(rt, "time")).UTC(),
		Type:          rapid.StringMatching(`[a-z][a-z.]{0,15}`).Draw(rt, "type"),
		Actor:         spine.ActorType(rapid.SampledFrom([]string{"agent", "human", "system"}).Draw(rt, "actor")),
		SchemaVersion: rapid.IntRange(1, 9).Draw(rt, "schemaVersion"),
		Payload:       payload,
	}
}

// TestPropCanonicalDeterministicAndVerifies asserts the core invariants over a wide
// space of events: encoding is deterministic, self-produced bytes are canonical, and
// a round trip preserves the load-bearing fields.
func TestPropCanonicalDeterministicAndVerifies(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		e := drawEvent(rt)
		a, err := CanonicalBytes(e)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		b, err := CanonicalBytes(e)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if !bytes.Equal(a, b) {
			t.Fatal("encoding is not deterministic")
		}
		if err := VerifyCanonical(a); err != nil {
			t.Fatalf("self-produced bytes not canonical: %v", err)
		}
		got, err := DecodeCanonical(a)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Seq != e.Seq || got.Stream != e.Stream || got.Type != e.Type || got.Actor != e.Actor {
			t.Fatalf("round trip lost a field: %+v", got)
		}
	})
}

// TestPropPayloadIntegersStayIntegers guards the float64 hazard: an integer-typed
// payload value must survive a round trip as an integer, not silently become a
// float. A drift here would change the canonical bytes and break verification.
func TestPropPayloadIntegersStayIntegers(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		want := rapid.Int64().Draw(rt, "n")
		e := sampleEvent()
		e.Payload = map[string]any{"n": want}
		b, err := CanonicalBytes(e)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeCanonical(b)
		if err != nil {
			t.Fatal(err)
		}
		var got64 int64
		switch n := got.Payload["n"].(type) {
		case int64:
			got64 = n
		case uint64:
			// A non-negative CBOR integer decodes to uint64 under any. Its canonical
			// bytes are identical to the int64 encoding of the same value, so this is
			// still an integer, not the float64 hazard the guard is about.
			got64 = int64(n)
		default:
			t.Fatalf("payload integer decoded as %T, not an integer type", got.Payload["n"])
		}
		if got64 != want {
			t.Fatalf("payload integer changed: got %d want %d", got64, want)
		}
	})
}

// Detecting that an event's CONTENT was tampered with (one valid event swapped for
// another valid event) is deliberately not tested here, because it is not a
// structural property. Canonical encoding is injective, so distinct canonical byte
// strings are distinct events; but proving a record matches the history it claims
// is the job of the tamper-evident spine's hash chain and signed roots, not of
// structural verification. That guarantee belongs to the chain keystone.
