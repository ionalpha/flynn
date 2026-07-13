package resource

import (
	"encoding/json"
	"strings"
	"testing"
)

// compile is a small helper around the built-in compiler for the unit tests.
func compileT(t *testing.T, schema string) Validator {
	t.Helper()
	v, err := newBuiltinCompiler().Compile([]byte(schema))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return v
}

func mustDecode(t *testing.T, doc string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func TestBuiltinSchema(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		doc     string
		wantErr bool
	}{
		{"type ok", `{"type":"string"}`, `"hi"`, false},
		{"type mismatch", `{"type":"string"}`, `5`, true},
		{"integer is number", `{"type":"number"}`, `7`, false},
		{"number not integer", `{"type":"integer"}`, `7.5`, true},
		{"required present", `{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}}`, `{"a":"x"}`, false},
		{"required missing", `{"type":"object","required":["a"]}`, `{}`, true},
		{"enum ok", `{"enum":["a","b"]}`, `"b"`, false},
		{"enum bad", `{"enum":["a","b"]}`, `"c"`, true},
		{"const ok", `{"const":42}`, `42`, false},
		{"const bad", `{"const":42}`, `43`, true},
		{"minimum ok", `{"type":"number","minimum":0}`, `0`, false},
		{"minimum bad", `{"type":"number","minimum":0}`, `-1`, true},
		{"exclusiveMinimum", `{"type":"number","exclusiveMinimum":0}`, `0`, true},
		{"maxLength ok", `{"type":"string","maxLength":3}`, `"abc"`, false},
		{"maxLength bad", `{"type":"string","maxLength":3}`, `"abcd"`, true},
		{"pattern ok", `{"type":"string","pattern":"^a.*z$"}`, `"abcz"`, false},
		{"pattern bad", `{"type":"string","pattern":"^a.*z$"}`, `"abc"`, true},
		{"minItems bad", `{"type":"array","minItems":2}`, `[1]`, true},
		{"items recurse bad", `{"type":"array","items":{"type":"integer"}}`, `[1,"two"]`, true},
		{"additionalProperties false", `{"type":"object","properties":{"a":{}},"additionalProperties":false}`, `{"a":1,"b":2}`, true},
		{"nested ok", `{"type":"object","properties":{"o":{"type":"object","required":["n"],"properties":{"n":{"type":"integer"}}}}}`, `{"o":{"n":3}}`, false},
		{"nested bad", `{"type":"object","properties":{"o":{"type":"object","required":["n"]}}}`, `{"o":{}}`, true},
		{"unknown keyword ignored", `{"type":"string","x-vendor":"whatever"}`, `"ok"`, false},
		{"maximum bad", `{"type":"number","maximum":10}`, `11`, true},
		{"maximum ok", `{"type":"number","maximum":10}`, `10`, false},
		{"exclusiveMaximum", `{"type":"number","exclusiveMaximum":10}`, `10`, true},
		{"minLength bad", `{"type":"string","minLength":3}`, `"ab"`, true},
		{"maxItems bad", `{"type":"array","maxItems":2}`, `[1,2,3]`, true},
		{"maxItems ok", `{"type":"array","maxItems":2}`, `[1,2]`, false},
		{"minItems ok", `{"type":"array","minItems":1}`, `[1]`, false},
		{"union type ok", `{"type":["string","null"]}`, `null`, false},
		{"null against a typed schema", `{"type":"string"}`, `null`, true},
		{"boolean type", `{"type":"boolean"}`, `true`, false},
		{"fractional number is not an integer", `{"type":"integer"}`, `1.5`, true},
		{"additionalProperties schema ok", `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":{"type":"integer"}}`, `{"a":"x","b":2}`, false},
		{"additionalProperties schema violated", `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":{"type":"integer"}}`, `{"a":"x","b":"two"}`, true},
		{"nested array items object", `{"type":"array","items":{"type":"object","required":["n"]}}`, `[{"n":1},{}]`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := compileT(t, tc.schema)
			err := v.Validate(mustDecode(t, tc.doc))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate(%s) err = %v, wantErr = %v", tc.doc, err, tc.wantErr)
			}
		})
	}
}

// TestCompileRejectsMalformedSchemas gates the compiler: a schema document that is
// not a JSON object, or that carries a sub-schema the compiler cannot build, must
// fail at compile time. Registration compiles up front precisely so these never
// surface as a mysterious admission failure on the first write.
func TestCompileRejectsMalformedSchemas(t *testing.T) {
	cases := []struct {
		name   string
		schema string
	}{
		{"not json", `{`},
		{"not an object", `"a string"`},
		{"property sub-schema is not an object", `{"properties":{"a":"nope"}}`},
		{"additionalProperties sub-schema is broken", `{"additionalProperties":{"pattern":"[unclosed"}}`},
		{"items sub-schema is not an object", `{"items":42}`},
		{"pattern is not a valid regexp", `{"type":"string","pattern":"[unclosed"}`},
		{"nested property pattern is invalid", `{"properties":{"a":{"pattern":"(("}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newBuiltinCompiler().Compile([]byte(tc.schema)); err == nil {
				t.Fatalf("Compile(%s) = nil error, want a compile failure", tc.schema)
			}
		})
	}
}

// TestNewSchemaCompilerIsTheAdmissionEngine proves the exported compiler is the
// same engine registered kinds are admitted through, so a caller outside the store
// (a CLI validating a spec before writing it) gets the identical verdict.
func TestNewSchemaCompilerIsTheAdmissionEngine(t *testing.T) {
	schema := []byte(`{"type":"object","required":["size"],"properties":{"size":{"enum":["s","m"]}}}`)
	v, err := NewSchemaCompiler().Compile(schema)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := v.Validate(mustDecode(t, `{"size":"m"}`)); err != nil {
		t.Fatalf("a valid spec must be admitted: %v", err)
	}
	if err := v.Validate(mustDecode(t, `{"size":"xl"}`)); err == nil {
		t.Fatal("an out-of-enum spec must be rejected")
	}

	reg := NewRegistry()
	if err := reg.Register(Kind{APIVersion: "test/v1", Name: "Widget", Schema: schema}); err != nil {
		t.Fatal(err)
	}
	// The registry's verdict on the same spec must match the exported compiler's.
	if err := reg.Validate("test/v1", "Widget", []byte(`{"size":"m"}`)); err != nil {
		t.Fatalf("registry admission disagreed with the exported compiler: %v", err)
	}
	if err := reg.Validate("test/v1", "Widget", []byte(`{"size":"xl"}`)); err == nil {
		t.Fatal("registry admission disagreed with the exported compiler on a bad spec")
	}
}

// TestValidateReportsNonJSONGoValues covers an instance handed to the validator as
// a native Go value rather than a decoded JSON tree (a host wiring its own decoder):
// the type mismatch is reported with the Go type instead of panicking or silently
// admitting the value.
func TestValidateReportsNonJSONGoValues(t *testing.T) {
	v := compileT(t, `{"type":"string"}`)
	err := v.Validate(42) // an int, not a float64: not a JSON-decoded value
	if err == nil {
		t.Fatal("a non-JSON Go value must not satisfy a typed schema")
	}
	if !strings.Contains(err.Error(), "int") {
		t.Fatalf("the error must name the offending type, got %q", err)
	}
}

func TestHashStableAndContentSensitive(t *testing.T) {
	base := Resource{
		APIVersion: "test/v1", Kind: "W", Name: "a",
		Labels: map[string]string{"k": "v"},
		Spec:   json.RawMessage(`{"b":2,"a":1}`),
	}
	// Volatile envelope fields do not affect the hash.
	withEnv := base
	withEnv.SyncVersion = 99
	withEnv.Version = 7
	h1, err := Hash(base)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := Hash(withEnv)
	if h1 != h2 {
		t.Fatal("envelope fields must not change the content hash")
	}
	// Spec key order does not affect the hash (canonicalization).
	reordered := base
	reordered.Spec = json.RawMessage(`{"a":1,"b":2}`)
	h3, _ := Hash(reordered)
	if h1 != h3 {
		t.Fatal("spec key order must not change the content hash")
	}
	// Different content does.
	changed := base
	changed.Spec = json.RawMessage(`{"a":1,"b":3}`)
	h4, _ := Hash(changed)
	if h1 == h4 {
		t.Fatal("different content must change the hash")
	}
}
