package resource

import (
	"encoding/json"
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
