package archetype_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/archetype"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

func putAgent(t *testing.T, s resource.Store, name string, spec archetype.Spec) {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), resource.Resource{
		APIVersion: archetype.GroupVersion, Kind: archetype.Kind, Name: name, Spec: raw,
	}); err != nil {
		t.Fatalf("put agent %s: %v", name, err)
	}
}

func TestResolveFlatAgentIsItself(t *testing.T) {
	s := newStore(t)
	putAgent(t, s, "a", archetype.Spec{System: "s", Capabilities: []string{"read"}, Model: "m"})
	got, err := archetype.Resolve(context.Background(), s, resource.Scope{}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.System != "s" || got.Model != "m" || len(got.Capabilities) != 1 || got.Capabilities[0] != "read" {
		t.Fatalf("flat resolve = %+v", got)
	}
}

func TestCompositionOverridesScalarsAndUnionsSets(t *testing.T) {
	s := newStore(t)
	putAgent(t, s, "base", archetype.Spec{
		System: "base prompt", Model: "m-base", Driver: "d-base",
		Capabilities: []string{"read"}, SkillScope: "base-skills",
	})
	putAgent(t, s, "specialist", archetype.Spec{
		Extends:      []string{"base"},
		System:       "specialist prompt", // overrides base
		Capabilities: []string{"write"},   // unions with base
		// Model/Driver/SkillScope omitted -> inherited from base
	})
	got, err := archetype.Resolve(context.Background(), s, resource.Scope{}, "specialist")
	if err != nil {
		t.Fatal(err)
	}
	if got.System != "specialist prompt" {
		t.Fatalf("system = %q, want the specialist override", got.System)
	}
	if got.Model != "m-base" || got.Driver != "d-base" || got.SkillScope != "base-skills" {
		t.Fatalf("inherited scalars wrong: %+v", got)
	}
	if want := []string{"read", "write"}; !equalSorted(got.Capabilities, want) {
		t.Fatalf("capabilities = %v, want %v (union)", got.Capabilities, want)
	}
}

func TestLaterBaseAndSelfWinScalars(t *testing.T) {
	s := newStore(t)
	putAgent(t, s, "b1", archetype.Spec{Model: "m1", Driver: "d1"})
	putAgent(t, s, "b2", archetype.Spec{Model: "m2"}) // later base overrides earlier for Model
	putAgent(t, s, "a", archetype.Spec{Extends: []string{"b1", "b2"}, Driver: "d-self"})
	got, err := archetype.Resolve(context.Background(), s, resource.Scope{}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "m2" {
		t.Fatalf("model = %q, want m2 (later base wins)", got.Model)
	}
	if got.Driver != "d-self" {
		t.Fatalf("driver = %q, want d-self (self overrides bases)", got.Driver)
	}
}

func TestDiamondIsAllowed(t *testing.T) {
	s := newStore(t)
	putAgent(t, s, "d", archetype.Spec{Capabilities: []string{"x"}})
	putAgent(t, s, "b", archetype.Spec{Extends: []string{"d"}, Capabilities: []string{"b"}})
	putAgent(t, s, "c", archetype.Spec{Extends: []string{"d"}, Capabilities: []string{"c"}})
	putAgent(t, s, "a", archetype.Spec{Extends: []string{"b", "c"}})
	got, err := archetype.Resolve(context.Background(), s, resource.Scope{}, "a")
	if err != nil {
		t.Fatalf("a diamond must resolve, got: %v", err)
	}
	if want := []string{"b", "c", "x"}; !equalSorted(got.Capabilities, want) {
		t.Fatalf("capabilities = %v, want %v (x once)", got.Capabilities, want)
	}
}

func TestCompositionCycleErrors(t *testing.T) {
	s := newStore(t)
	putAgent(t, s, "a", archetype.Spec{Extends: []string{"b"}})
	putAgent(t, s, "b", archetype.Spec{Extends: []string{"a"}})
	_, err := archetype.Resolve(context.Background(), s, resource.Scope{}, "a")
	if fault.Classify(err) != fault.Terminal {
		t.Fatalf("a composition cycle must be a terminal error, got: %v", err)
	}
}

func TestResolveUnknownAgentErrors(t *testing.T) {
	s := newStore(t)
	if _, err := archetype.Resolve(context.Background(), s, resource.Scope{}, "nope"); err == nil {
		t.Fatal("resolving an unknown agent must error")
	}
}

func TestResolvedGrantReflectsUnion(t *testing.T) {
	s := newStore(t)
	putAgent(t, s, "base", archetype.Spec{Capabilities: []string{"read"}})
	putAgent(t, s, "a", archetype.Spec{Extends: []string{"base"}, Capabilities: []string{"write"}})
	got, err := archetype.Resolve(context.Background(), s, resource.Scope{}, "a")
	if err != nil {
		t.Fatal(err)
	}
	g := got.Grant()
	if !g.Allows("read") || !g.Allows("write") {
		t.Fatal("resolved grant must allow the union of the chain's capabilities")
	}
	if g.Allows("bash") {
		t.Fatal("resolved grant must not allow an undeclared capability")
	}
}

// TestProp_ResolvedCapabilitiesAreUnion is a composition property: a one-level
// composed Agent's resolved capabilities are exactly the sorted, de-duplicated
// union of the base's and the specialist's, regardless of the inputs.
func TestProp_ResolvedCapabilitiesAreUnion(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		gen := rapid.SliceOf(rapid.StringMatching(`[a-z]{1,4}`))
		baseCaps := gen.Draw(rt, "base")
		selfCaps := gen.Draw(rt, "self")

		s := newStore(t)
		putAgent(t, s, "base", archetype.Spec{Capabilities: baseCaps})
		putAgent(t, s, "a", archetype.Spec{Extends: []string{"base"}, Capabilities: selfCaps})

		got, err := archetype.Resolve(context.Background(), s, resource.Scope{}, "a")
		if err != nil {
			rt.Fatalf("resolve: %v", err)
		}
		want := dedupSort(append(append([]string{}, baseCaps...), selfCaps...))
		if !equalSorted(got.Capabilities, want) {
			rt.Fatalf("resolved caps = %v, want union %v", got.Capabilities, want)
		}
	})
}

func equalSorted(a, b []string) bool {
	a = dedupSort(a)
	b = dedupSort(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func dedupSort(xs []string) []string {
	set := map[string]struct{}{}
	for _, x := range xs {
		set[x] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}
