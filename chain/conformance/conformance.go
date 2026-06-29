// Package conformance defines the Provetrail conformance vector suite: a fixed set
// of artifacts a conforming verifier must accept, and malformed ones it must reject
// with a specific failure code. The vectors are the language-neutral canon other
// implementations test themselves against. They are built deterministically (no wall
// clock, no randomness), so the committed fixture files are reproducible byte for
// byte and a verifier in any language can be checked against the same evidence.
//
// This file covers the L1 structural tier: well-formedness, canonical form, and
// ordering. The cryptographic tiers (signed checkpoints, single-event inclusion
// proofs, full signed run records) are defined alongside it in crypto.go as L2 and
// L3.
package conformance

import (
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/spine"
)

// SuiteVersion identifies the vector set; it is bumped when the canon changes.
const SuiteVersion = "0.1.0-draft"

// Tier is the conformance tier this suite exercises.
const Tier = "L1"

// Verdict is the outcome a conforming verifier must reach for a vector.
type Verdict string

const (
	// Accept means a conforming verifier must accept every event in the vector.
	Accept Verdict = "accept"
	// Reject means a conforming verifier must reject the vector with FailureCode.
	Reject Verdict = "reject"
)

// Vector is one conformance case: a sequence of canonical event byte blobs forming
// a single stream, and the verdict a conforming verifier must reach over them.
type Vector struct {
	ID          string
	Expect      Verdict
	FailureCode string
	Flags       []string
	Description string
	Events      [][]byte
}

// fixedTime is the single timestamp every generated event uses, so the suite is
// reproducible byte for byte. It is an arbitrary fixed instant, never the wall clock.
var fixedTime = time.Unix(0, 1_700_000_000_000_000_000).UTC()

func baseEvent() spine.Event {
	return spine.Event{
		Stream:        "run/conformance",
		Seq:           1,
		Time:          fixedTime,
		Type:          "action.dispatched",
		Actor:         spine.ActorAgent,
		SchemaVersion: 1,
		Payload:       map[string]any{},
	}
}

func mustCanonical(e spine.Event) []byte {
	b, err := chain.CanonicalBytes(e)
	if err != nil {
		// The generator's events are static and valid, so this is a programming
		// error in the generator, not a runtime condition.
		panic("conformance: base event must encode: " + err.Error())
	}
	return b
}

// nonSortEnc encodes without sorting map keys, so a struct serializes in field
// declaration order. It exists only to produce a deliberately non-canonical
// encoding of an otherwise valid event for the corresponding reject vector.
var nonSortEnc = func() cbor.EncMode {
	em, err := cbor.EncOptions{Sort: cbor.SortNone}.EncMode()
	if err != nil {
		panic("conformance: build non-sorting encoder: " + err.Error())
	}
	return em
}()

// unsortedWire mirrors the canonical event keys but in a deliberately non-sorted
// field order, so encoding it without key sorting yields valid CBOR that decodes to
// a real event yet is not in canonical form.
type unsortedWire struct {
	Type          string         `cbor:"type"`
	Stream        string         `cbor:"stream"`
	Seq           int64          `cbor:"seq"`
	TimeUnixNano  int64          `cbor:"time"`
	Actor         string         `cbor:"actor"`
	SchemaVersion int            `cbor:"schema_version"`
	Payload       map[string]any `cbor:"payload"`
}

func nonCanonicalBytes() []byte {
	e := baseEvent()
	w := unsortedWire{
		Type:          e.Type,
		Stream:        e.Stream,
		Seq:           e.Seq,
		TimeUnixNano:  e.Time.UTC().UnixNano(),
		Actor:         string(e.Actor),
		SchemaVersion: e.SchemaVersion,
		Payload:       e.Payload,
	}
	b, err := nonSortEnc.Marshal(w)
	if err != nil {
		panic("conformance: encode non-canonical event: " + err.Error())
	}
	return b
}

// Hand-crafted malformed encodings. Each isolates exactly one structural defect.
func dupKeyBytes() []byte   { return []byte{0xA2, 0x63, 's', 'e', 'q', 0x01, 0x63, 's', 'e', 'q', 0x02} }
func indefMapBytes() []byte { return []byte{0xBF, 0x63, 's', 'e', 'q', 0x01, 0xFF} }
func invalidUTF8Bytes() []byte {
	// A definite-length map {"type": <one byte 0xFF as a text string>}; 0xFF is not
	// valid UTF-8, so a conforming decoder rejects it.
	return []byte{0xA1, 0x64, 't', 'y', 'p', 'e', 0x61, 0xFF}
}

func trailingBytes() []byte {
	b := mustCanonical(baseEvent())
	return append(append([]byte{}, b...), 0x00)
}

// Generate returns the full structural conformance vector set, deterministically.
func Generate() []Vector {
	minimal := mustCanonical(baseEvent())

	full := baseEvent()
	full.Payload = map[string]any{
		"tool": "exec",
		"exit": int64(0),
		"ok":   true,
		"args": []any{"a", "b"},
		"meta": map[string]any{"k": "v"},
	}
	full.CausationID = "evt-0"
	full.Principal = "agent-1"
	full.OriginInstanceID = "inst-1"
	full.TraceID = "trace-1"
	full.SpanID = "span-1"
	withOptional := mustCanonical(full)

	seqEvent := func(seq int64, stream string) []byte {
		e := baseEvent()
		e.Seq = seq
		if stream != "" {
			e.Stream = stream
		}
		return mustCanonical(e)
	}

	return []Vector{
		{
			ID: "valid.minimal.01", Expect: Accept, Flags: []string{"Structural"},
			Description: "A single minimal event with the required fields and an empty payload.",
			Events:      [][]byte{minimal},
		},
		{
			ID: "valid.optional_fields.01", Expect: Accept, Flags: []string{"Structural"},
			Description: "A single event carrying every optional field and a payload of mixed value types.",
			Events:      [][]byte{withOptional},
		},
		{
			ID: "valid.chain.01", Expect: Accept, Flags: []string{"Structural", "Ordering"},
			Description: "Three events on one stream with strictly increasing Seq.",
			Events:      [][]byte{seqEvent(1, ""), seqEvent(2, ""), seqEvent(3, "")},
		},
		{
			ID: "invalid.enc.trailing_bytes.01", Expect: Reject, FailureCode: chain.CodeTrailingBytes,
			Flags: []string{"Encoding"}, Description: "A canonical event followed by one extra trailing byte.",
			Events: [][]byte{trailingBytes()},
		},
		{
			ID: "invalid.enc.duplicate_map_key.01", Expect: Reject, FailureCode: chain.CodeDuplicateMapKey,
			Flags: []string{"Encoding"}, Description: "A CBOR map containing a duplicated key.",
			Events: [][]byte{dupKeyBytes()},
		},
		{
			ID: "invalid.enc.indefinite_length.01", Expect: Reject, FailureCode: chain.CodeIndefiniteLength,
			Flags: []string{"Encoding"}, Description: "An indefinite-length CBOR map.",
			Events: [][]byte{indefMapBytes()},
		},
		{
			ID: "invalid.enc.invalid_utf8.01", Expect: Reject, FailureCode: chain.CodeInvalidUTF8,
			Flags: []string{"Encoding"}, Description: "A CBOR text string containing invalid UTF-8.",
			Events: [][]byte{invalidUTF8Bytes()},
		},
		{
			ID: "invalid.enc.non_canonical_cbor.01", Expect: Reject, FailureCode: chain.CodeNonCanonicalCBOR,
			Flags: []string{"Encoding"}, Description: "A valid event whose map keys are not in canonical sorted order.",
			Events: [][]byte{nonCanonicalBytes()},
		},
		{
			ID: "invalid.chain.non_monotonic_seq.01", Expect: Reject, FailureCode: chain.CodeNonMonotonicSeq,
			Flags: []string{"Ordering"}, Description: "Two events on one stream with a repeated Seq.",
			Events: [][]byte{seqEvent(2, ""), seqEvent(2, "")},
		},
		{
			ID: "invalid.chain.stream_mismatch.01", Expect: Reject, FailureCode: chain.CodeStreamMismatch,
			Flags: []string{"Ordering"}, Description: "Two events that belong to different streams.",
			Events: [][]byte{seqEvent(1, ""), seqEvent(2, "run/other")},
		},
		{
			ID: "invalid.chain.empty.01", Expect: Reject, FailureCode: chain.CodeEmptyStream,
			Flags: []string{"Structural"}, Description: "An empty event stream.",
			Events: [][]byte{},
		},
	}
}
