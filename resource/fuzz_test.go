package resource_test

import (
	"testing"

	"github.com/ionalpha/flynn/resource"
)

// FuzzParseSelector checks that selector parsing never panics on arbitrary input,
// and that a successfully parsed selector can be matched against labels without
// panicking. The parser is a hand-written tokenizer over untrusted strings, the
// classic place a stray index or slice bound bites.
func FuzzParseSelector(f *testing.F) {
	for _, seed := range []string{
		"", "k=v", "k!=v", "k==v", "!k", "k", "k in (a, b)", "k notin (a)",
		"a=b, c in (d,e), !f", "(", ")", "k in (", "k in )", "=", "  ", ",,,",
		"k in (a,(b),c)", "kkkkkkkk", "a===b",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(_ *testing.T, s string) {
		sel, err := resource.ParseSelector(s)
		if err != nil {
			return
		}
		// Matching must be total: it never panics for any labels.
		_ = sel.Matches(map[string]string{"k": "v", "a": "b"})
		_ = sel.Matches(nil)
	})
}

// FuzzSchemaAdmission checks that compiling an arbitrary schema and validating an
// arbitrary spec against it never panics: both halves run over untrusted JSON
// (schemas and specs an AI or a user may author for a new kind), so they must fail
// cleanly, never crash.
func FuzzSchemaAdmission(f *testing.F) {
	seeds := []struct{ schema, spec string }{
		{`{"type":"object","required":["a"]}`, `{"a":1}`},
		{`{"type":"string","enum":["x","y"]}`, `"z"`},
		{`{"type":"array","items":{"type":"integer"},"minItems":2}`, `[1]`},
		{`{"type":"number","minimum":0,"maximum":10}`, `5`},
		{`{"type":"object","additionalProperties":false,"properties":{"a":{}}}`, `{"b":2}`},
		{`not json`, `1`},
		{`{"type":"string","pattern":"["}`, `"x"`},
		{`{}`, ``},
		{`{"type":["string","null"]}`, `null`},
	}
	for _, s := range seeds {
		f.Add([]byte(s.schema), []byte(s.spec))
	}
	f.Fuzz(func(_ *testing.T, schema, spec []byte) {
		reg := resource.NewRegistry()
		// A schema that fails to compile is a clean error, not a panic.
		if err := reg.Register(resource.Kind{APIVersion: "fuzz/v1", Name: "F", Schema: schema}); err != nil {
			return
		}
		// Admission over an arbitrary spec must terminate without panicking.
		_ = reg.Validate("fuzz/v1", "F", spec)
	})
}
