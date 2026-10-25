package archetypes_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/archetypes"
	"github.com/ionalpha/flynn/internal/archetype"
)

// TestSpecIsTheEngineType fails if [archetypes.Spec] stops being an alias: a
// distinct type would make the value a host builds unassignable to the one the
// engine stores, and review.Archetype's result unusable as agent.Config.Agent.
func TestSpecIsTheEngineType(t *testing.T) {
	if reflect.TypeOf(archetypes.Spec{}) != reflect.TypeOf(archetype.Spec{}) {
		t.Fatal("archetypes.Spec is not identical to internal/archetype.Spec")
	}
}

// TestSpecRoundTripsAndGrantsExactlyItsCapabilities covers a Spec built by a host
// through the exported name: it survives JSON, and the authority derived from it is
// exactly the capabilities it declares.
func TestSpecRoundTripsAndGrantsExactlyItsCapabilities(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		names := rapid.SliceOfNDistinct(rapid.StringMatching(`[a-z]{1,8}`), 0, 6, rapid.ID[string]).Draw(t, "caps")
		in := archetypes.Spec{
			System:       rapid.String().Draw(t, "system"),
			Capabilities: names,
			Model:        rapid.StringMatching(`[a-z]{0,6}(:[a-z0-9-]{1,8})?`).Draw(t, "model"),
		}
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		var out archetypes.Spec
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		again, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(raw) {
			t.Fatalf("round trip changed the spec: %s -> %s", raw, again)
		}
		g := in.Grant()
		for _, n := range names {
			if !g.Allows(n) {
				t.Fatalf("grant refuses declared capability %q", n)
			}
		}
		if g.Allows("not-declared-capability-0") {
			t.Fatal("grant allows an undeclared capability")
		}
	})
}
