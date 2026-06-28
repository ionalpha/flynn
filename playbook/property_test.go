package playbook

import (
	"testing"

	"pgregory.net/rapid"
)

// TestServiceResultFieldsProperty asserts the invariant the runner relies on when it
// registers a service from a flow result: reading the result's fields never panics and
// never invents data. For any object a flow might return (any mix of present, absent,
// empty, or non-string values), firstString returns either a string that was actually
// present and non-empty under one of the keys or "", and stringMap returns only the
// non-empty string-valued entries. So a flow that omits or mistypes a field can never cause
// the runner to register a service with fabricated or malformed values.
func TestServiceResultFieldsProperty(t *testing.T) {
	key := rapid.SampledFrom([]string{"name", "service", "url", "id", "app", "extra"})
	rapid.Check(t, func(rt *rapid.T) {
		m := map[string]any{}
		n := rapid.IntRange(0, 5).Draw(rt, "n")
		for i := 0; i < n; i++ {
			k := key.Draw(rt, "k")
			switch rapid.IntRange(0, 3).Draw(rt, "kind") {
			case 0:
				m[k] = rapid.String().Draw(rt, "s")
			case 1:
				m[k] = "" // present but empty
			case 2:
				m[k] = rapid.Float64().Draw(rt, "f") // non-string
			default:
				m[k] = map[string]any{"x": rapid.String().Draw(rt, "ax")}
			}
		}

		// firstString never returns a value that was not present and non-empty.
		got := firstString(m, "name", "service")
		if got != "" {
			name, _ := m["name"].(string)
			svc, _ := m["service"].(string)
			if got != name && got != svc {
				rt.Fatalf("firstString invented %q", got)
			}
		}

		// stringMap returns only non-empty string values.
		for k, v := range stringMap(m["address"]) {
			if v == "" {
				rt.Fatalf("stringMap kept an empty value for %q", k)
			}
			if orig, ok := m["address"].(map[string]any)[k].(string); !ok || orig != v {
				rt.Fatalf("stringMap altered the value for %q", k)
			}
		}
	})
}
