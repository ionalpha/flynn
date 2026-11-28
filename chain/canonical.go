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

// eventDomain separates event preimages from every other signed message.
const eventDomain = "provetrail/event/v1\n"

// Failure codes are stable identifiers matching the published Provetrail
// conformance registry.
const (
	CodeNonCanonicalCBOR = "enc.non_canonical_cbor"
	CodeDuplicateMapKey  = "enc.duplicate_map_key"
	CodeIndefiniteLength = "enc.indefinite_length"
	CodeTrailingBytes    = "enc.trailing_bytes"
	CodeDecode           = "enc.decode"
	CodeEncode           = "enc.encode"
	CodeTimeRange        = "enc.time_out_of_range"
	CodeInvalidUTF8      = "enc.invalid_utf8"
	CodeInvalidActor     = "enc.invalid_actor"
)

// canonicalEnc is RFC 8949 Core Deterministic Encoding. canonicalDec rejects
// duplicate map keys and indefinite-length items, the two ambiguities that
// would let two byte strings claim to be the same event.
var (
	canonicalEnc cbor.EncMode
	canonicalDec cbor.DecMode
)

func init() {
	enc, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		// The options are static, so an error is a programming bug.
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

// canonicalEvent is the decode target and encode source. The seven critical keys
// are always present; optional keys are omitted when empty. The encoder sorts
// struct keys as it sorts map keys, so field order does not affect the wire; the
// conformance vectors pin the bytes.
type canonicalEvent struct {
	Stream         string         `cbor:"stream"`
	Seq            int64          `cbor:"seq"`
	TimeUnixNano   int64          `cbor:"time"`
	Type           string         `cbor:"type"`
	Actor          string         `cbor:"actor"`
	SchemaVersion  int            `cbor:"schema_version"`
	Payload        map[string]any `cbor:"payload"`
	CausationID    string         `cbor:"causation_id,omitempty"`
	OriginInstance string         `cbor:"origin_instance_id,omitempty"`
	Principal      string         `cbor:"principal,omitempty"`
	TraceID        string         `cbor:"trace_id,omitempty"`
	SpanID         string         `cbor:"span_id,omitempty"`
}

// toCanonical normalizes time to UTC unix nanoseconds, dropping the monotonic
// reading and location, and a nil payload to an empty map so the payload key is
// always present.
func toCanonical(e spine.Event) (canonicalEvent, error) {
	if e.Time.IsZero() {
		return canonicalEvent{}, fault.New(fault.Terminal, CodeTimeRange, "chain: event time is zero")
	}
	// Actor is a closed category (SPEC 2.1). Checking here stops a producer
	// emitting outside it, and verifyCanonical's re-derivation rejects carried
	// bytes that claim one.
	switch e.Actor {
	case spine.ActorAgent, spine.ActorHuman, spine.ActorSystem:
	default:
		return canonicalEvent{}, fault.New(fault.Terminal, CodeInvalidActor, "chain: actor is not agent, human, or system")
	}
	payload := e.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	// The strict decoder requires UTF-8 text, so fail closed here rather than
	// produce bytes that will not round-trip. Binary data belongs in a []byte.
	for _, s := range [...]string{e.Stream, e.Type, string(e.Actor), e.CausationID, e.OriginInstanceID, e.Principal, e.TraceID, e.SpanID} {
		if !utf8.ValidString(s) {
			return canonicalEvent{}, fault.New(fault.Terminal, CodeInvalidUTF8, "chain: event field is not valid UTF-8")
		}
	}
	if !validUTF8Value(payload) {
		return canonicalEvent{}, fault.New(fault.Terminal, CodeInvalidUTF8, "chain: payload contains a non-UTF-8 string")
	}
	return canonicalEvent{
		Stream:         e.Stream,
		Seq:            e.Seq,
		TimeUnixNano:   e.Time.UTC().UnixNano(),
		Type:           e.Type,
		Actor:          string(e.Actor),
		SchemaVersion:  e.SchemaVersion,
		Payload:        payload,
		CausationID:    e.CausationID,
		OriginInstance: e.OriginInstanceID,
		Principal:      e.Principal,
		TraceID:        e.TraceID,
		SpanID:         e.SpanID,
	}, nil
}

// CanonicalBytes returns the deterministic CBOR encoding of e, the bytes a hash
// chain commits to and a proof carries. Equal content gives equal bytes whatever
// the map iteration order.
//
// Payload integers must be integer-typed (int, int64). An integral float64, as a
// JSON round trip produces, encodes as a float and changes the bytes. Producers
// must preserve integer types.
func CanonicalBytes(e spine.Event) ([]byte, error) {
	ce, err := toCanonical(e)
	if err != nil {
		return nil, err
	}
	b, err := canonicalEnc.Marshal(ce)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return b, nil
}

// DecodeCanonical rejects duplicate map keys, indefinite-length items and
// trailing bytes. It does not prove canonical form; use [VerifyCanonical].
func DecodeCanonical(b []byte) (spine.Event, error) {
	var ce canonicalEvent
	if err := canonicalDec.Unmarshal(b, &ce); err != nil {
		return spine.Event{}, classifyDecodeError(err)
	}
	return fromCanonical(ce), nil
}

// VerifyCanonical decodes b strictly, re-encodes the event and compares, so
// non-canonical or extra content is rejected.
func VerifyCanonical(b []byte) error {
	_, err := verifyCanonical(b)
	return err
}

// verifyCanonical also returns the decoded event, to avoid a second decode.
func verifyCanonical(b []byte) (spine.Event, error) {
	e, err := DecodeCanonical(b)
	if err != nil {
		return spine.Event{}, err
	}
	reencoded, err := CanonicalBytes(e)
	if err != nil {
		return spine.Event{}, err
	}
	if !bytes.Equal(reencoded, b) {
		return spine.Event{}, fault.New(fault.Terminal, CodeNonCanonicalCBOR, "chain: bytes are not in canonical form")
	}
	return e, nil
}

// LeafInput returns the preimage the hash chain commits to: the domain tag, the
// big-endian uint64 length, then the canonical bytes. The length prefix keeps two
// events from sharing a preimage.
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

// validUTF8Value checks strings, map keys and slice elements; byte slices are
// skipped.
func validUTF8Value(v any) bool {
	switch x := v.(type) {
	case string:
		return utf8.ValidString(x)
	case map[string]any:
		//ordered:ok every bad entry gives the same false
		for k, val := range x {
			if !utf8.ValidString(k) || !validUTF8Value(val) {
				return false
			}
		}
	case map[any]any:
		//ordered:ok every bad entry gives the same false
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

func classifyDecodeError(err error) error {
	var dup *cbor.DupMapKeyError
	if errors.As(err, &dup) {
		return fault.Wrap(fault.Terminal, CodeDuplicateMapKey, err)
	}
	var extra *cbor.ExtraneousDataError
	if errors.As(err, &extra) {
		return fault.Wrap(fault.Terminal, CodeTrailingBytes, err)
	}
	if strings.Contains(err.Error(), "indefinite") {
		return fault.Wrap(fault.Terminal, CodeIndefiniteLength, err)
	}
	if strings.Contains(err.Error(), "invalid UTF-8") {
		return fault.Wrap(fault.Terminal, CodeInvalidUTF8, err)
	}
	return fault.Wrap(fault.Terminal, CodeDecode, err)
}
