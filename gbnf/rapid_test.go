package gbnf

import (
	"encoding/json"
	"testing"

	"pgregory.net/rapid"
)

// TestRapidEquivalence is the property-based form of the correctness proof: for a
// schema drawn from the corpus and a candidate the generator derives from drawn
// bytes, the compiled grammar must accept the candidate exactly when the independent
// reference validator does. rapid shrinks any disagreement to a minimal failing
// input, so a regression surfaces as the smallest schema-and-value that breaks the
// equivalence rather than whatever large case happened to trip first.
func TestRapidEquivalence(t *testing.T) {
	grammars := make([]*Grammar, len(corpus))
	schemas := make([]schemaNode, len(corpus))
	for i, raw := range corpus {
		g, err := Arguments(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("corpus %d: compile: %v", i, err)
		}
		grammars[i] = g
		if err := json.Unmarshal([]byte(raw), &schemas[i]); err != nil {
			t.Fatalf("corpus %d: parse: %v", i, err)
		}
	}

	rapid.Check(t, func(rt *rapid.T) {
		idx := rapid.IntRange(0, len(corpus)-1).Draw(rt, "schema")
		data := rapid.SliceOfN(rapid.Byte(), 1, 64).Draw(rt, "bytes")
		text := genValue(&schemas[idx], &cursor{data: data}, 0)

		var v any
		if json.Unmarshal([]byte(text), &v) != nil {
			return // the generator should always emit valid JSON; skip the rare miss
		}
		want := refValid(v, &schemas[idx])
		got := grammars[idx].Accepts(text)
		if got != want {
			rt.Fatalf("corpus %d: Accepts=%v refValid=%v for %s", idx, got, want, text)
		}
	})
}
