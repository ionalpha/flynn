package gbnf

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzCompileTotality asserts the compiler is total: no input schema, however
// malformed, makes it panic. When it does produce a grammar, that grammar must be
// well-formed and safe to recognize against, so a runtime is never handed something
// it cannot parse or that could hang the recognizer.
func FuzzCompileTotality(f *testing.F) {
	f.Add([]byte(readToolSchema))
	f.Add([]byte(`{"type":"object","properties":{"a":{"type":"array","items":{"type":"object"}}}}`))
	f.Add([]byte(`{"type":"string","enum":["x","y"]}`))
	f.Add([]byte(`{"type":"object","additionalProperties":true}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(`{"type":}`))
	f.Add([]byte(``))
	f.Add([]byte(`{"type":"object","required":["x"],"properties":{},"additionalProperties":false}`))
	f.Add([]byte(strings.Repeat(`{"type":"array","items":`, 200) + `{}` + strings.Repeat(`}`, 200)))
	f.Add([]byte(`{"type":"string","enum":["a\"b","c\\d"]}`))

	probes := []string{"", "{}", `{"path":"a"}`, "null", "[]", `{"a":{"b":{"c":1}}}`}
	f.Fuzz(func(t *testing.T, raw []byte) {
		g, err := Arguments(json.RawMessage(raw))
		if err != nil {
			return // refusing a schema is fine; only a panic or a bad grammar is not
		}
		if err := WellFormed(g.String(), g.Root()); err != nil {
			t.Fatalf("compiled grammar is not well-formed: %v\nschema: %s\ngrammar:\n%s", err, raw, g.String())
		}
		for _, p := range probes {
			_ = g.Accepts(p) // must not panic or hang (budget-bounded)
		}
	})
}

// FuzzRecognizerTotality asserts the recognizer terminates and never panics on
// arbitrary input, including for a grammar that contains the recursive generic JSON
// value rule. The step budget makes even a pathological input return a verdict
// rather than loop.
func FuzzRecognizerTotality(f *testing.F) {
	closed := mustGrammar(f, readToolSchema)
	freeform := mustGrammar(f, `{"type":"object","additionalProperties":true}`)
	nested := mustGrammar(f, `{"type":"object","properties":{"v":{}},"additionalProperties":false}`)

	f.Add(`{"path":"a.go","offset":1}`)
	f.Add(`{"v":{"a":[1,{"b":"c"}]}}`)
	f.Add("{{{{{{{{{{")
	f.Add(`{"path":"` + str(2000, 'x') + `"}`)
	f.Fuzz(func(_ *testing.T, input string) {
		_ = closed.Accepts(input)
		_ = freeform.Accepts(input)
		_ = nested.Accepts(input)
	})
}

// FuzzEquivalence keeps the central correctness property under fuzzing: for a schema
// selected from the corpus and a candidate the generator derives from the fuzzed
// bytes, the grammar accepts the candidate exactly when the independent reference
// validator does. The fuzzer evolves inputs toward the boundary, so this hunts for a
// disagreement the fixed-seed property test might miss.
func FuzzEquivalence(f *testing.F) {
	grammars := make([]*Grammar, len(corpus))
	schemas := make([]schemaNode, len(corpus))
	for i, raw := range corpus {
		g, err := Arguments(json.RawMessage(raw))
		if err != nil {
			f.Fatalf("corpus %d: %v", i, err)
		}
		grammars[i] = g
		if err := json.Unmarshal([]byte(raw), &schemas[i]); err != nil {
			f.Fatalf("corpus %d: %v", i, err)
		}
	}

	f.Add([]byte{0})
	f.Add([]byte{3, 1, 4, 1, 5, 9, 2, 6})
	f.Add([]byte{255, 128, 64, 32, 16, 8, 4, 2, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			return
		}
		idx := int(data[0]) % len(corpus)
		c := &cursor{data: data[1:]}
		text := genValue(&schemas[idx], c, 0)
		var v any
		if json.Unmarshal([]byte(text), &v) != nil {
			return
		}
		want := refValid(v, &schemas[idx])
		got := grammars[idx].Accepts(text)
		if got != want {
			t.Fatalf("corpus %d: Accepts=%v refValid=%v for %s", idx, got, want, text)
		}
	})
}

func mustGrammar(f *testing.F, schema string) *Grammar {
	f.Helper()
	g, err := Arguments(json.RawMessage(schema))
	if err != nil {
		f.Fatalf("compile %s: %v", schema, err)
	}
	return g
}

func str(n int, r byte) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = r
	}
	return string(b)
}
