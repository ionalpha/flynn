package chain

// The canonical encoder and the payload field readers. The encoder must refuse anything
// it cannot canonicalise, including invalid UTF-8 nested inside a payload and an
// indefinite-length item, and a field must read back the same value whatever integer or
// float encoding it arrived in. Two encodings of one value that disagree here would give
// two roots for one event.

import "testing"

// TestValidUTF8ValueWalksNestedPayloads asserts the canonical encoder refuses an event
// whose payload hides an invalid UTF-8 string anywhere in it: in a nested map's key or
// value, in a slice element, or in a map keyed by any. A CBOR text string must be valid
// UTF-8, so encoding one would produce bytes that do not round trip.
func TestValidUTF8ValueWalksNestedPayloads(t *testing.T) {
	const bad = "\xff\xfe"

	tests := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{"clean nested", map[string]any{
			"m": map[string]any{"k": "v"},
			"l": []any{"a", int64(1), []byte{0xff}},
		}, true},
		{"bytes are not text", map[string]any{"blob": []byte{0xff, 0xfe}}, true},
		{"bad nested map value", map[string]any{"m": map[string]any{"k": bad}}, false},
		{"bad nested map key", map[string]any{"m": map[string]any{bad: "v"}}, false},
		{"bad slice element", map[string]any{"l": []any{"ok", bad}}, false},
		{"bad element deep in a slice of maps", map[string]any{
			"l": []any{map[string]any{"k": bad}},
		}, false},
		{"bad key in a map keyed by any", map[string]any{
			"m": map[any]any{bad: "v"},
		}, false},
		{"bad value in a map keyed by any", map[string]any{
			"m": map[any]any{"k": bad},
		}, false},
		{"non-string key in a map keyed by any is skipped", map[string]any{
			"m": map[any]any{int64(1): "v"},
		}, true},
		{"top-level bad string", map[string]any{"s": bad}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validUTF8Value(tt.payload); got != tt.want {
				t.Fatalf("validUTF8Value = %v, want %v", got, tt.want)
			}
			e := sampleEvent()
			e.Payload = tt.payload
			_, err := CanonicalBytes(e)
			if tt.want && err != nil {
				t.Fatalf("CanonicalBytes rejected a valid payload: %v", err)
			}
			if !tt.want && !hasCode(err, CodeInvalidUTF8) {
				t.Fatalf("CanonicalBytes err = %v, want %s", err, CodeInvalidUTF8)
			}
		})
	}
}

// TestDecodeCanonicalRejectsIndefiniteLength asserts the strict decoder refuses
// indefinite-length CBOR items and names the fault. An indefinite-length encoding is
// one of the two ambiguities that would let two different byte strings claim to be the
// same event, so it must never decode.
func TestDecodeCanonicalRejectsIndefiniteLength(t *testing.T) {
	// 0xbf starts an indefinite-length map, 0xff breaks it: an empty map, encoded the
	// one way the canonical form forbids.
	indefinite := []byte{0xbf, 0xff}
	_, err := DecodeCanonical(indefinite)
	if !hasCode(err, CodeIndefiniteLength) {
		t.Fatalf("DecodeCanonical of an indefinite-length map: err = %v, want %s", err, CodeIndefiniteLength)
	}
	if err := VerifyCanonical(indefinite); !hasCode(err, CodeIndefiniteLength) {
		t.Fatalf("VerifyCanonical of an indefinite-length map: err = %v, want %s", err, CodeIndefiniteLength)
	}
}

// TestIntFieldAcceptsEveryIntegerEncoding asserts a payload integer reads back the same
// whether the event came straight from canonical CBOR (uint64), back through a JSON
// store (float64), or from memory (int/int64), and that a value that cannot be an int64
// is refused rather than wrapped to a negative.
func TestIntFieldAcceptsEveryIntegerEncoding(t *testing.T) {
	const key = "call"
	tests := []struct {
		name  string
		value any
		want  int64
		ok    bool
	}{
		{"int64", int64(7), 7, true},
		{"int", 7, 7, true},
		{"uint64", uint64(7), 7, true},
		{"float64 from a JSON round trip", float64(7), 7, true},
		{"uint64 past int64", uint64(1) << 63, 0, false},
		{"string", "7", 0, false},
		{"bool", true, 0, false},
		{"nil value", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := intField(map[string]any{key: tt.value}, key)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("intField = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
	if _, ok := intField(map[string]any{}, key); ok {
		t.Fatal("intField over an absent key must report not-ok")
	}
}

// TestStringFieldIsTypeSafe asserts a payload string field reads as empty when it is
// absent or carries some other type, so a mistyped record never reads as a valid name.
func TestStringFieldIsTypeSafe(t *testing.T) {
	if got := stringField(map[string]any{"a": "x"}, "a"); got != "x" {
		t.Fatalf("stringField = %q, want x", got)
	}
	if got := stringField(map[string]any{"a": int64(1)}, "a"); got != "" {
		t.Fatalf("stringField over a non-string = %q, want empty", got)
	}
	if got := stringField(map[string]any{}, "a"); got != "" {
		t.Fatalf("stringField over an absent key = %q, want empty", got)
	}
}

// TestPayloadNumbersAcrossEncodings asserts a provenance declaration reads back the same
// counts and rates from every encoding it round trips through (canonical CBOR, a JSON
// store, memory), and that an out-of-range count is clamped rather than wrapped
// negative, which would read as fewer attested events than none.
func TestPayloadNumbersAcrossEncodings(t *testing.T) {
	intTests := []struct {
		name  string
		value any
		want  int
	}{
		{"int", 5, 5},
		{"int64", int64(5), 5},
		{"uint64", uint64(5), 5},
		{"float64", float64(5), 5},
		{"string", "5", 0},
		{"absent", nil, 0},
	}
	for _, tt := range intTests {
		t.Run("int/"+tt.name, func(t *testing.T) {
			if got := payloadInt(tt.value); got != tt.want {
				t.Fatalf("payloadInt = %d, want %d", got, tt.want)
			}
		})
	}
	if got := payloadInt(uint64(1) << 63); got <= 0 {
		t.Fatalf("payloadInt of an oversized count = %d, want it clamped to a positive maximum", got)
	}

	floatTests := []struct {
		name  string
		value any
		want  float64
	}{
		{"float64", 0.5, 0.5},
		{"int", 1, 1},
		{"string", "1", 0},
	}
	for _, tt := range floatTests {
		t.Run("float/"+tt.name, func(t *testing.T) {
			if got := payloadFloat(tt.value); got != tt.want {
				t.Fatalf("payloadFloat = %v, want %v", got, tt.want)
			}
		})
	}

	countTests := []struct {
		name  string
		value any
		want  map[string]int
	}{
		{"map[string]any from JSON", map[string]any{"a": float64(2)}, map[string]int{"a": 2}},
		{"map[any]any from CBOR", map[any]any{"a": uint64(2)}, map[string]int{"a": 2}},
		{"non-string keys are dropped", map[any]any{int64(1): uint64(2)}, map[string]int{}},
		{"empty map[string]any", map[string]any{}, nil},
		{"empty map[any]any", map[any]any{}, nil},
		{"wrong type", "a", nil},
	}
	for _, tt := range countTests {
		t.Run("counts/"+tt.name, func(t *testing.T) {
			got := payloadCounts(tt.value)
			if len(got) != len(tt.want) {
				t.Fatalf("payloadCounts = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Fatalf("payloadCounts = %v, want %v", got, tt.want)
				}
			}
		})
	}
}
