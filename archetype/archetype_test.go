package archetype_test

import (
	"context"
	"encoding/json"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/archetype"
	"github.com/ionalpha/flynn/resource"
)

func newStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := archetype.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	return resource.NewMemory(reg)
}

func TestAgentRoundTripsThroughStore(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	spec := archetype.Spec{
		System:       "You research and report; you do not modify files.",
		Capabilities: []string{"read", "glob", "grep"},
		Model:        "anthropic:claude-opus-4-8",
		Driver:       "general-software",
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Put(ctx, resource.Resource{
		APIVersion: archetype.GroupVersion, Kind: archetype.Kind, Name: "researcher", Spec: raw,
	})
	if err != nil {
		t.Fatalf("put agent: %v", err)
	}

	got, err := s.Get(ctx, archetype.Kind, resource.Scope{}, "researcher")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	decoded, err := archetype.DecodeSpec(got)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.System != spec.System || decoded.Model != spec.Model || len(decoded.Capabilities) != 3 {
		t.Fatalf("round-trip mismatch: %+v", decoded)
	}
	if saved.Kind != archetype.Kind {
		t.Fatalf("kind = %q", saved.Kind)
	}
}

func TestSchemaRejectsUnknownField(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	// A spec with a field outside the schema must be refused by admission.
	bad := json.RawMessage(`{"system":"x","not_a_field":true}`)
	_, err := s.Put(ctx, resource.Resource{
		APIVersion: archetype.GroupVersion, Kind: archetype.Kind, Name: "bad", Spec: bad,
	})
	if err == nil {
		t.Fatal("expected admission to reject an unknown spec field")
	}
}

func TestGrantFromCapabilities(t *testing.T) {
	spec := archetype.Spec{Capabilities: []string{"read", "grep"}}
	g := spec.Grant()
	if !g.Allows("read") || !g.Allows("grep") {
		t.Fatal("grant must allow declared capabilities")
	}
	if g.Allows("write") || g.Allows("bash") {
		t.Fatal("grant must deny undeclared capabilities")
	}
}

// TestProp_SpecRoundTrip is the rigor property: any Agent spec encodes and decodes
// back to an equal spec, so a stored Agent is a faithful record of its archetype.
func TestProp_SpecRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		spec := archetype.Spec{
			System:       rapid.String().Draw(rt, "system"),
			Capabilities: rapid.SliceOf(rapid.StringMatching(`[a-z._]{1,12}`)).Draw(rt, "caps"),
			Model:        rapid.String().Draw(rt, "model"),
			Driver:       rapid.String().Draw(rt, "driver"),
			SkillScope:   rapid.String().Draw(rt, "skill"),
			MemoryScope:  rapid.String().Draw(rt, "memory"),
		}
		raw, err := json.Marshal(spec)
		if err != nil {
			rt.Fatalf("marshal: %v", err)
		}
		got, err := archetype.DecodeSpec(resource.Resource{Spec: raw})
		if err != nil {
			rt.Fatalf("decode: %v", err)
		}
		gotRaw, err := json.Marshal(got)
		if err != nil {
			rt.Fatalf("re-marshal: %v", err)
		}
		if string(gotRaw) != string(raw) {
			rt.Fatalf("round-trip changed the spec:\n got %s\nwant %s", gotRaw, raw)
		}
	})
}
