package chain

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/spine"
)

// eventDomain is the domain-separation tag mixed into an event's hashed and signed
// preimage. It binds a hash over an event to this format and version so a signature
// over an event can never be confused with a signature over any other message.
const eventDomain = "provetrail/event/v1\n"

// Failure codes are stable, dotted identifiers. They are the codes a conforming
// verifier reports, so a rejection names which check failed rather than just
// "invalid". They match the published Provetrail conformance registry.
const (
	CodeNonCanonicalCBOR = "enc.non_canonical_cbor"
	CodeDuplicateMapKey  = "enc.duplicate_map_key"
	CodeIndefiniteLength = "enc.indefinite_length"
	CodeTrailingBytes    = "enc.trailing_bytes"
	CodeDecode           = "enc.decode"
	CodeEncode           = "enc.encode"
	CodeTimeRange        = "enc.time_out_of_range"
	CodeInvalidUTF8      = "enc.invalid_utf8"
)

// canonicalEnc and canonicalDec are the single, shared CBOR modes the format is
// defined by. canonicalEnc is RFC 8949 Core Deterministic Encoding (preferred
// serialization, shortest integers and floats, map keys sorted in bytewise
// lexicographic order). canonicalDec is strict: it rejects duplicate map keys and
// indefinite-length items, the two ambiguities that would let two byte strings
// claim to be the same event.
var (
	canonicalEnc cbor.EncMode
	canonicalDec cbor.DecMode
)

func init() {
	enc, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		// The options are static and valid, so this cannot fail in practice; a
		// panic here would be a programming error, not a runtime condition.
		panic("chain: build canonical CBOR encoder: " + err.Error())
	}
	canonicalEnc = enc

	dec, err := cbor.DecOptions{
		DupMapKey:   cbor.DupMapKeyEnforcedAPF,
		IndefLength: cbor.IndefLengthForbidden,
	}.DecMode()
	if err != nil {
		panic("chain: build canonical CBOR decoder: " + err.Error())
	}
	canonicalDec = dec
}

// canonicalEvent is the decode target for canonical bytes. Its cbor tags name the
// wire keys. It exists only to extract fields cleanly on decode; the encode path
// builds a map directly (below) so the encoder, not Go struct order, is the single
// authority on which keys are present and how they are ordered.
type canonicalEvent struct {
	Stream         string         `cbor:"stream"`
	Seq            int64          `cbor:"seq"`
	TimeUnixNano   int64          `cbor:"time"`
	Type           string         `cbor:"type"`
	Actor          string         `cbor:"actor"`
	SchemaVersion  int            `cbor:"schema_version"`
	Payload        map[string]any `cbor:"payload"`
	CausationID    string         `cbor:"causation_id"`
	OriginInstance string         `cbor:"origin_instance_id"`
	Principal      string         `cbor:"principal"`
	TraceID        string         `cbor:"trace_id"`
	SpanID         string         `cbor:"span_id"`
}

// canonicalMap builds the logical key/value map the encoder serializes. The seven
// load-bearing keys are always present; the optional string keys are present only
// when set, by a fixed rule, so the encoding of an event without them is stable.
// Time is normalized to UTC unix nanoseconds, which intentionally drops the
// monotonic reading and location so the canonical form is clock- and
// location-independent. A nil payload is normalized to an empty map so the payload
// key is always present with a stable value.
func canonicalMap(e spine.Event) (map[string]any, error) {
	if e.Time.IsZero() {
		return nil, fault.New(fault.Terminal, CodeTimeRange, "chain: event time is zero")
	}
	payload := e.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	// A CBOR text string must be valid UTF-8, and the strict decoder enforces that,
	// so the encoder fails closed on a non-UTF-8 string field rather than producing
	// bytes that will not round-trip. Binary data belongs in a byte string ([]byte),
	// which carries no UTF-8 constraint.
	for _, s := range []string{e.Stream, e.Type, string(e.Actor), e.CausationID, e.OriginInstanceID, e.Principal, e.TraceID, e.SpanID} {
		if !utf8.ValidString(s) {
			return nil, fault.New(fault.Terminal, CodeInvalidUTF8, "chain: event field is not valid UTF-8")
		}
	}
	if !validUTF8Value(payload) {
		return nil, fault.New(fault.Terminal, CodeInvalidUTF8, "chain: payload contains a non-UTF-8 string")
	}
	m := map[string]any{
		"stream":         e.Stream,
		"seq":            e.Seq,
		"time":           e.Time.UTC().UnixNano(),
		"type":           e.Type,
		"actor":          string(e.Actor),
		"schema_version": e.SchemaVersion,
		"payload":        payload,
	}
	if e.CausationID != "" {
		m["causation_id"] = e.CausationID
	}
	if e.OriginInstanceID != "" {
		m["origin_instance_id"] = e.OriginInstanceID
	}
	if e.Principal != "" {
		m["principal"] = e.Principal
	}
	if e.TraceID != "" {
		m["trace_id"] = e.TraceID
	}
	if e.SpanID != "" {
		m["span_id"] = e.SpanID
	}
	return m, nil
}

// CanonicalBytes returns the deterministic CBOR encoding of e. Two events with the
// same logical content always produce identical bytes, regardless of Go map
// iteration order, because the encoder sorts map keys. These are the bytes a hash
// chain commits to and a proof carries.
//
// Payload integers must be integer-typed (int, int64). A value that is a float64
// holding an integral number, which is what a payload round-tripped through a JSON
// decoder becomes, encodes as a float and changes the bytes. Producers must
// preserve integer types; the property tests guard this.
func CanonicalBytes(e spine.Event) ([]byte, error) {
	m, err := canonicalMap(e)
	if err != nil {
		return nil, err
	}
	b, err := canonicalEnc.Marshal(m)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return b, nil
}

// DecodeCanonical strictly decodes canonical bytes back to an Event. It rejects
// duplicate map keys, indefinite-length items, and trailing bytes, so a byte
// string that decodes here is well formed CBOR with no structural ambiguity. It
// does NOT by itself prove the bytes are in canonical form; use VerifyCanonical for
// that.
func DecodeCanonical(b []byte) (spine.Event, error) {
	var ce canonicalEvent
	if err := canonicalDec.Unmarshal(b, &ce); err != nil {
		return spine.Event{}, classifyDecodeError(err)
	}
	return fromCanonical(ce), nil
}

// VerifyCanonical reports whether b is the exact canonical encoding of the event it
// decodes to. It decodes b strictly, re-encodes the recovered event through the
// canonical encoder, and compares: any difference means b carries non-canonical or
// extra content and is rejected. This is the re-derivation check that lets a
// verifier confirm carried bytes agree with their logical content.
func VerifyCanonical(b []byte) error {
	e, err := DecodeCanonical(b)
	if err != nil {
		return err
	}
	reencoded, err := CanonicalBytes(e)
	if err != nil {
		return err
	}
	if !bytes.Equal(reencoded, b) {
		return fault.New(fault.Terminal, CodeNonCanonicalCBOR, "chain: bytes are not in canonical form")
	}
	return nil
}

// LeafInput returns the domain-separated preimage the hash chain commits to for one
// event: the domain tag, the length of the canonical bytes as a big-endian uint64,
// then the canonical bytes. The length prefix makes the encoding unambiguous so two
// different events can never share a preimage. The tamper-evident spine hashes this.
func LeafInput(canonical []byte) ([]byte, error) {
	if len(canonical) > math.MaxInt-len(eventDomain)-8 {
		return nil, fault.New(fault.Terminal, CodeEncode, "chain: canonical event too large to frame")
	}
	out := make([]byte, 0, len(eventDomain)+8+len(canonical))
	out = append(out, eventDomain...)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(canonical)))
	out = append(out, n[:]...)
	out = append(out, canonical...)
	return out, nil
}

func fromCanonical(ce canonicalEvent) spine.Event {
	return spine.Event{
		Stream:           ce.Stream,
		Seq:              ce.Seq,
		Time:             time.Unix(0, ce.TimeUnixNano).UTC(),
		Type:             ce.Type,
		Actor:            spine.ActorType(ce.Actor),
		Payload:          ce.Payload,
		SchemaVersion:    ce.SchemaVersion,
		CausationID:      ce.CausationID,
		OriginInstanceID: ce.OriginInstance,
		Principal:        ce.Principal,
		TraceID:          ce.TraceID,
		SpanID:           ce.SpanID,
	}
}

// validUTF8Value reports whether every string in v (and in any map keys, map
// values, or slice elements it contains) is valid UTF-8. Byte slices are not
// strings and are skipped, so binary payload data is allowed.
func validUTF8Value(v any) bool {
	switch x := v.(type) {
	case string:
		return utf8.ValidString(x)
	case map[string]any:
		for k, val := range x {
			if !utf8.ValidString(k) || !validUTF8Value(val) {
				return false
			}
		}
	case map[any]any:
		for k, val := range x {
			if ks, ok := k.(string); ok && !utf8.ValidString(ks) {
				return false
			}
			if !validUTF8Value(val) {
				return false
			}
		}
	case []any:
		for _, val := range x {
			if !validUTF8Value(val) {
				return false
			}
		}
	}
	return true
}

// classifyDecodeError maps a CBOR decode error to a stable failure code so a
// rejection names the specific structural fault.
func classifyDecodeError(err error) error {
	var dup *cbor.DupMapKeyError
	if errors.As(err, &dup) {
		return fault.Wrap(fault.Terminal, CodeDuplicateMapKey, err)
	}
	var extra *cbor.ExtraneousDataError
	if errors.As(err, &extra) {
		return fault.Wrap(fault.Terminal, CodeTrailingBytes, err)
	}
	if strings.Contains(err.Error(), "indefinite-length") {
		return fault.Wrap(fault.Terminal, CodeIndefiniteLength, err)
	}
	return fault.Wrap(fault.Terminal, CodeDecode, err)
}
