package chain

import (
	"bytes"
	"testing"
	"time"

	"github.com/ionalpha/flynn/spine"
)

func sampleEvent() spine.Event {
	return spine.Event{
		Stream:        "run/abc",
		Seq:           7,
		Time:          time.Date(2026, 6, 29, 12, 0, 0, 123, time.UTC),
		Type:          "action.dispatched",
		Actor:         spine.ActorAgent,
		SchemaVersion: 1,
		Payload:       map[string]any{"tool": "exec", "exit": int64(0), "n": int64(42)},
		CausationID:   "evt-6",
		Principal:     "agent-1",
	}
}

func TestCanonicalDeterministic(t *testing.T) {
	e := sampleEvent()
	a, err := CanonicalBytes(e)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	b, err := CanonicalBytes(e)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("canonical encoding is not stable across calls")
	}
}

func TestCanonicalPayloadKeyOrderIndependent(t *testing.T) {
	// Build the same logical payload in two different key-insertion orders. Each key
	// maps to a value that does not depend on insertion order, so the two events are
	// logically identical and must encode to identical bytes: the encoder sorts keys
	// rather than following Go map iteration order.
	mk := func(order []string) []byte {
		e := sampleEvent()
		e.Payload = map[string]any{}
		for _, k := range order {
			e.Payload[k] = k
		}
		b, err := CanonicalBytes(e)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	forward := mk([]string{"a", "b", "c", "d"})
	reverse := mk([]string{"d", "c", "b", "a"})
	if !bytes.Equal(forward, reverse) {
		t.Fatal("canonical bytes depend on payload key insertion order")
	}
}

func TestRoundTrip(t *testing.T) {
	e := sampleEvent()
	b, err := CanonicalBytes(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeCanonical(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stream != e.Stream || got.Seq != e.Seq || got.Type != e.Type || got.Actor != e.Actor {
		t.Fatalf("round-trip field mismatch: %+v", got)
	}
	if !got.Time.Equal(e.Time) {
		t.Fatalf("round-trip time mismatch: %v vs %v", got.Time, e.Time)
	}
	if got.CausationID != e.CausationID || got.Principal != e.Principal {
		t.Fatalf("round-trip optional-field mismatch: %+v", got)
	}
}

func TestEncodeDecodeEncodeIdempotent(t *testing.T) {
	b, err := CanonicalBytes(sampleEvent())
	if err != nil {
		t.Fatal(err)
	}
	e, err := DecodeCanonical(b)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := CanonicalBytes(e)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, b2) {
		t.Fatal("encode-decode-encode is not idempotent")
	}
}

func TestVerifyCanonicalAccepts(t *testing.T) {
	b, err := CanonicalBytes(sampleEvent())
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyCanonical(b); err != nil {
		t.Fatalf("canonical bytes rejected: %v", err)
	}
}

func TestVerifyCanonicalRejectsTrailingBytes(t *testing.T) {
	b, _ := CanonicalBytes(sampleEvent())
	bad := append(append([]byte{}, b...), 0x00)
	if err := VerifyCanonical(bad); err == nil {
		t.Fatal("trailing bytes were accepted")
	}
}

func TestVerifyCanonicalRejectsGarbage(t *testing.T) {
	for _, g := range [][]byte{nil, {}, {0xff}, {0xa1, 0x00}, []byte("not cbor")} {
		if err := VerifyCanonical(g); err == nil {
			t.Fatalf("garbage accepted as canonical: %x", g)
		}
	}
}

func TestZeroTimeRejected(t *testing.T) {
	e := sampleEvent()
	e.Time = time.Time{}
	if _, err := CanonicalBytes(e); err == nil {
		t.Fatal("zero time was accepted")
	}
}

func TestInvalidUTF8Rejected(t *testing.T) {
	e := sampleEvent()
	e.Type = "bad\xff\xfe"
	if _, err := CanonicalBytes(e); err == nil {
		t.Fatal("invalid UTF-8 in a field was accepted")
	}
	e = sampleEvent()
	e.Payload = map[string]any{"k": "v\xff"}
	if _, err := CanonicalBytes(e); err == nil {
		t.Fatal("invalid UTF-8 in payload was accepted")
	}
}

func TestLeafInputDomainSeparated(t *testing.T) {
	b, _ := CanonicalBytes(sampleEvent())
	in, err := LeafInput(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(in, []byte(eventDomain)) {
		t.Fatal("leaf input missing domain tag")
	}
	if len(in) != len(eventDomain)+8+len(b) {
		t.Fatalf("leaf input length wrong: %d", len(in))
	}
}

func TestVerifyStreamMonotonic(t *testing.T) {
	v := NewVerifier()
	mk := func(seq int64) []byte {
		e := sampleEvent()
		e.Seq = seq
		b, _ := CanonicalBytes(e)
		return b
	}
	if _, err := v.VerifyStream([][]byte{mk(1), mk(2), mk(3)}); err != nil {
		t.Fatalf("valid stream rejected: %v", err)
	}
	if _, err := v.VerifyStream([][]byte{mk(1), mk(1)}); err == nil {
		t.Fatal("repeated Seq accepted")
	}
	if _, err := v.VerifyStream([][]byte{mk(3), mk(2)}); err == nil {
		t.Fatal("decreasing Seq accepted")
	}
	if _, err := v.VerifyStream(nil); err == nil {
		t.Fatal("empty stream accepted")
	}
}

func TestVerifyStreamRejectsStreamMismatch(t *testing.T) {
	v := NewVerifier()
	a := sampleEvent()
	a.Seq = 1
	ab, _ := CanonicalBytes(a)
	b := sampleEvent()
	b.Seq = 2
	b.Stream = "run/other"
	bb, _ := CanonicalBytes(b)
	if _, err := v.VerifyStream([][]byte{ab, bb}); err == nil {
		t.Fatal("mixed streams accepted")
	}
}
