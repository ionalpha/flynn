package flow

import (
	"context"
	"reflect"
	"testing"

	"pgregory.net/rapid"
)

// TestPropDeterministic asserts a flow's result is a pure function of its inputs:
// running the same flow over the same config any number of times yields equal
// results. Determinism is the property the spine audit and replay rely on.
func TestPropDeterministic(t *testing.T) {
	flow := mustDecode(t, `{
      "steps": [
        {"id": "active", "op": "transform", "transform": {"source": "config.items", "filter": "it.active"}},
        {"id": "names", "op": "transform", "transform": {"source": "steps.active", "map": "upper(it.name)"}},
        {"op": "return", "return": {"value": "{{steps.names}}"}}
      ]
    }`)

	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 8).Draw(rt, "n")
		items := make([]any, n)
		for i := range n {
			items[i] = map[string]any{
				"name":   rapid.StringMatching(`[a-z]{1,5}`).Draw(rt, "name"),
				"active": rapid.Bool().Draw(rt, "active"),
			}
		}
		config := map[string]any{"items": items}

		in := New()
		first, err := in.Run(context.Background(), flow, config)
		if err != nil {
			rt.Fatalf("run: %v", err)
		}
		for range 3 {
			again, err := in.Run(context.Background(), flow, config)
			if err != nil {
				rt.Fatalf("rerun: %v", err)
			}
			if !reflect.DeepEqual(first, again) {
				rt.Fatalf("non-deterministic: %#v vs %#v", first, again)
			}
		}
	})
}
