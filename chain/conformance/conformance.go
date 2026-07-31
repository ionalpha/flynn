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
//
// This package is the single source of truth for the vectors. The provetrail repo's
// vectors/ directory is a downstream copy of testdata/{vectors,crypto}; regenerate
// with `go test ./chain/conformance -update` and copy outward, never the reverse. See
// TESTING.md ("Conformance vectors") for the change ordering.
package conformance

import (
	"bytes"
	"math"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/spine"
)

// SuiteVersion identifies the vector set; it is bumped when the canon changes.
const SuiteVersion = "0.1.0"

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

// envelopeMap is the base event as a raw map, the starting point for the schema
// defect vectors: deleting, retyping, or adding a key produces exactly one deviation
// from the canonical envelope, encoded canonically so the deviation is the only fault.
func envelopeMap() map[string]any {
	e := baseEvent()
	return map[string]any{
		"stream":         e.Stream,
		"seq":            e.Seq,
		"time":           e.Time.UTC().UnixNano(),
		"type":           e.Type,
		"actor":          string(e.Actor),
		"schema_version": e.SchemaVersion,
		"payload":        map[string]any{},
	}
}

func mustEncodeMap(m map[string]any) []byte {
	b, err := cryptoEnc.Marshal(m)
	if err != nil {
		panic("conformance: encode envelope map: " + err.Error())
	}
	return b
}

// patchBytes replaces the first occurrence of old in a copy of b, panicking if old
// is absent: a surgical byte mutation that is auditable in the vector description.
func patchBytes(b, old, replacement []byte) []byte {
	i := bytes.Index(b, old)
	if i < 0 {
		panic("conformance: byte pattern to patch not found")
	}
	out := make([]byte, 0, len(b)-len(old)+len(replacement))
	out = append(out, b[:i]...)
	out = append(out, replacement...)
	out = append(out, b[i+len(old):]...)
	return out
}

// seqNonMinimal is the canonical minimal event with its seq value 1 re-encoded as a
// two-byte integer head (0x19 0x00 0x01): valid CBOR, same logical value, not the
// shortest encoding, so it is not in canonical form.
func seqNonMinimal() []byte {
	return patchBytes(mustCanonical(baseEvent()),
		[]byte{0x63, 's', 'e', 'q', 0x01},
		[]byte{0x63, 's', 'e', 'q', 0x19, 0x00, 0x01})
}

// dupKeyFull is the canonical minimal event grown by one duplicated "seq" entry, so
// the duplicate-key rejection is pinned on a full envelope, not only on a fragment.
func dupKeyFull() []byte {
	b := mustCanonical(baseEvent())
	out := append([]byte{}, b...)
	out[0] = 0xA8 // seven entries become eight
	return append(out, 0x63, 's', 'e', 'q', 0x02)
}

// indefFull is the canonical minimal event re-framed as an indefinite-length map.
func indefFull() []byte {
	b := mustCanonical(baseEvent())
	out := append([]byte{0xBF}, b[1:]...)
	return append(out, 0xFF)
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
		{
			ID: "valid.seq_gap.01", Expect: Accept, Flags: []string{"Structural", "Ordering"},
			Description: "Three events with seq 1, 5, 900: a record may carry a window of a longer stream, so gaps are permitted; only repeats and decreases are rejected.",
			Events:      [][]byte{seqEvent(1, ""), seqEvent(5, ""), seqEvent(900, "")},
		},
		{
			ID: "valid.large_ints.01", Expect: Accept, Flags: []string{"Structural", "Encoding"},
			Description: "Events whose seq and time exceed 2^53 (2^53+1 and the int64 maximum): a verifier that coerces int64 to a 53-bit float changes these values and fails.",
			Events:      [][]byte{largeIntEvent(1<<53+1, 1<<53+1), largeIntEvent(1<<53+3, math.MaxInt64)},
		},
		{
			ID: "valid.unicode_payload.01", Expect: Accept, Flags: []string{"Structural", "Encoding"},
			Description: "A payload whose keys span one to four UTF-8 bytes, pinning bytewise-encoded key sort order and non-ASCII content.",
			Events:      [][]byte{unicodePayloadEvent()},
		},
		{
			ID: "valid.payload_bytes.01", Expect: Accept, Flags: []string{"Structural", "Encoding"},
			Description: "A payload carrying a CBOR byte string value: binary data is a bstr, exempt from the UTF-8 rule that governs text strings.",
			Events:      [][]byte{bytesPayloadEvent()},
		},
		{
			ID: "valid.empty_strings.01", Expect: Accept, Flags: []string{"Structural", "Encoding"},
			Description: "An event whose required string fields are empty: required fields are encoded even when empty (SPEC 2.1/8.2), as zero-length text strings.",
			Events:      [][]byte{emptyStringsEvent()},
		},
		{
			ID: "invalid.schema.missing_field.01", Expect: Reject, FailureCode: chain.CodeNonCanonicalCBOR,
			Flags: []string{"Schema"}, Description: "A canonically encoded envelope missing the required time field: the canonical re-encoding of what it decodes to differs, so it is not the canonical form of any event.",
			Events: [][]byte{mustEncodeMap(deleteKey(envelopeMap(), "time"))},
		},
		{
			ID: "invalid.schema.unknown_field.01", Expect: Reject, FailureCode: chain.CodeNonCanonicalCBOR,
			Flags: []string{"Schema"}, Description: "A canonically encoded envelope carrying an unknown extra field, which no canonical event encoding contains.",
			Events: [][]byte{mustEncodeMap(withKey(envelopeMap(), "extra", int64(1)))},
		},
		{
			ID: "invalid.schema.wrong_type.01", Expect: Reject, FailureCode: chain.CodeDecode,
			Flags: []string{"Schema"}, Description: "An envelope whose seq is a text string rather than an integer.",
			Events: [][]byte{mustEncodeMap(withKey(envelopeMap(), "seq", "1"))},
		},
		{
			ID: "invalid.schema.bad_actor.01", Expect: Reject, FailureCode: chain.CodeInvalidActor,
			Flags: []string{"Schema"}, Description: "An envelope whose actor is not one of the closed category agent, human, or system.",
			Events: [][]byte{mustEncodeMap(withKey(envelopeMap(), "actor", "robot"))},
		},
		{
			ID: "invalid.enc.non_minimal_int.01", Expect: Reject, FailureCode: chain.CodeNonCanonicalCBOR,
			Flags: []string{"Encoding"}, Description: "A full envelope whose seq value 1 is encoded in a two-byte integer head: same value, not the shortest encoding.",
			Events: [][]byte{seqNonMinimal()},
		},
		{
			ID: "invalid.enc.duplicate_map_key_full.01", Expect: Reject, FailureCode: chain.CodeDuplicateMapKey,
			Flags: []string{"Encoding"}, Description: "A full envelope grown by a duplicated seq entry, so duplicate-key rejection is pinned on a complete event, not only a fragment.",
			Events: [][]byte{dupKeyFull()},
		},
		{
			ID: "invalid.enc.indefinite_length_full.01", Expect: Reject, FailureCode: chain.CodeIndefiniteLength,
			Flags: []string{"Encoding"}, Description: "A full envelope framed as an indefinite-length map.",
			Events: [][]byte{indefFull()},
		},
	}
}

// largeIntEvent returns an event whose seq and time exceed float64's 53-bit integer
// range, so any verifier that routes int64 through a Number representation corrupts
// them and fails the re-derivation check.
func largeIntEvent(seq, timeNanos int64) []byte {
	e := baseEvent()
	e.Seq = seq
	e.Time = time.Unix(0, timeNanos).UTC()
	return mustCanonical(e)
}

func unicodePayloadEvent() []byte {
	e := baseEvent()
	e.Payload = map[string]any{
		"Z": "latin",
		"é": "e-acute",
		"☃": "snowman",
		"🎯": "target",
	}
	return mustCanonical(e)
}

func bytesPayloadEvent() []byte {
	e := baseEvent()
	e.Payload = map[string]any{"data": []byte{0x00, 0x10, 0xFF}}
	return mustCanonical(e)
}

// emptyStringsEvent returns an event whose required string fields are all empty:
// well-formed per the encode-even-when-empty rule, since the actor category and
// the non-string fields still carry values.
func emptyStringsEvent() []byte {
	e := baseEvent()
	e.Stream = ""
	e.Type = ""
	return mustCanonical(e)
}

func deleteKey(m map[string]any, k string) map[string]any {
	delete(m, k)
	return m
}

func withKey(m map[string]any, k string, v any) map[string]any {
	m[k] = v
	return m
}
