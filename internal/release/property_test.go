package release

import (
	"encoding/json"
	"os"
	"testing"

	"pgregory.net/rapid"
)

// Verification either proves a release or refuses it, and there is no third outcome:
// no input, however mangled, may make it panic or return a Provenance it did not
// prove. The generator here is deliberately dumb, because an attacker is not obliged
// to send well-formed JSON.
func TestVerifyNeverPanicsOnArbitraryInput(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		raw := rapid.SliceOfN(rapid.Byte(), 0, 4096).Draw(t, "bundle")
		p, err := Verify(raw)
		if err == nil {
			t.Fatalf("random bytes verified as a release: %+v", p)
		}
	})
}

// Any change to a verified bundle must break it. This is the property the whole
// package exists to have: an attacker who can rewrite any field of a genuine release
// bundle, and who cannot forge a Fulcio identity or a Rekor entry, has no field left
// to rewrite that gets them an accepted forgery.
func TestNoSingleFieldRewriteSurvivesVerification(t *testing.T) {
	raw, err := os.ReadFile(goldenBundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(raw); err != nil {
		t.Fatalf("the golden bundle does not verify: %v", err)
	}

	// The leaves of the bundle, by JSON path. Every one of them is either signed, or
	// bound to something signed, so rewriting any single one has to be fatal.
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	paths := leafPaths(doc, nil)
	if len(paths) < 10 {
		t.Fatalf("only found %d leaves in the bundle; the walk is wrong", len(paths))
	}

	rapid.Check(t, func(t *rapid.T) {
		path := rapid.SampledFrom(paths).Draw(t, "field")
		value := rapid.SampledFrom([]any{
			"", "AAAA", "0", float64(0), float64(1), true, nil,
			"MEUCIQDrandomrandomrandomrandomrandomrandomrandomrandomrandom",
		}).Draw(t, "value")

		var mutated map[string]any
		if err := json.Unmarshal(raw, &mutated); err != nil {
			t.Fatal(err)
		}
		if !setPath(mutated, path, value) {
			return
		}
		out, err := json.Marshal(mutated)
		if err != nil {
			t.Fatal(err)
		}
		// An unchanged field is not a mutation; skip rather than assert on it.
		if string(out) == string(raw) {
			return
		}
		if _, err := Verify(out); err == nil {
			t.Fatalf("rewriting %v to %#v produced a bundle that still verified", path, value)
		}
	})
}

// leafPaths walks the bundle and returns the path to every scalar in it.
func leafPaths(v any, prefix []any) [][]any {
	var out [][]any
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			out = append(out, leafPaths(child, append(append([]any{}, prefix...), k))...)
		}
	case []any:
		for i, child := range t {
			out = append(out, leafPaths(child, append(append([]any{}, prefix...), i))...)
		}
	default:
		if len(prefix) > 0 {
			out = append(out, prefix)
		}
	}
	return out
}

// setPath writes value at path, reporting whether the path existed.
func setPath(doc any, path []any, value any) bool {
	cur := doc
	for i, step := range path {
		last := i == len(path)-1
		switch node := cur.(type) {
		case map[string]any:
			key, ok := step.(string)
			if !ok {
				return false
			}
			if _, exists := node[key]; !exists {
				return false
			}
			if last {
				node[key] = value
				return true
			}
			cur = node[key]
		case []any:
			idx, ok := step.(int)
			if !ok || idx < 0 || idx >= len(node) {
				return false
			}
			if last {
				node[idx] = value
				return true
			}
			cur = node[idx]
		default:
			return false
		}
	}
	return false
}
