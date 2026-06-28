package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/archetype"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/instance"
	"github.com/ionalpha/flynn/resource"
)

func TestResolveKindKnownAndFallbackAndUnknown(t *testing.T) {
	reg, err := missionRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	// Curated kinds resolve by their aliases with multiple columns.
	for _, alias := range []string{"instances", "instance", "agents", "runs", "goal"} {
		ck, ok := resolveKind(reg, alias)
		if !ok {
			t.Fatalf("alias %q did not resolve", alias)
		}
		if len(ck.descriptor.Columns) < 2 {
			t.Fatalf("alias %q resolved with too few columns: %d", alias, len(ck.descriptor.Columns))
		}
	}
	// A registered-but-uncurated kind still resolves, with a name-only fallback.
	if ck, ok := resolveKind(reg, "kind"); !ok || len(ck.descriptor.Columns) != 1 {
		t.Fatalf("fallback kind resolve = %+v, %v", ck, ok)
	}
	// Case-insensitive.
	if _, ok := resolveKind(reg, "AGENTS"); !ok {
		t.Fatal("alias resolution should be case-insensitive")
	}
	// Unknown stays unknown.
	if _, ok := resolveKind(reg, "nope"); ok {
		t.Fatal("unknown alias should not resolve")
	}
}

func TestResolveIDByIDAndName(t *testing.T) {
	ctx := context.Background()
	reg := resource.NewRegistry()
	if err := archetype.RegisterKind(reg); err != nil {
		t.Fatalf("register: %v", err)
	}
	store := resource.NewMemory(reg)
	created, err := store.Put(ctx, resource.Resource{
		APIVersion: archetype.GroupVersion, Kind: archetype.Kind, Name: "researcher",
		Spec: json.RawMessage(`{"model":"anthropic:claude-opus-4-8"}`),
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	// By name.
	if id, err := resolveID(ctx, store, archetype.Kind, "researcher"); err != nil || id != created.ID {
		t.Fatalf("resolve by name = %q, %v; want %q", id, err, created.ID)
	}
	// By id.
	if id, err := resolveID(ctx, store, archetype.Kind, created.ID); err != nil || id != created.ID {
		t.Fatalf("resolve by id = %q, %v; want %q", id, err, created.ID)
	}
	// Unknown.
	if _, err := resolveID(ctx, store, archetype.Kind, "ghost"); err == nil {
		t.Fatal("resolving an unknown ref should error")
	}
}

func TestRenderTableEmptyAndPopulated(t *testing.T) {
	var empty bytes.Buffer
	renderTable(&empty, controlplane.Table{Columns: []string{"NAME"}})
	if !strings.Contains(empty.String(), "no resources found") {
		t.Fatalf("empty table render = %q", empty.String())
	}

	var buf bytes.Buffer
	renderTable(&buf, controlplane.Table{
		Columns: []string{"NAME", "STATE"},
		Rows:    []controlplane.Row{{Name: "local", Cells: []string{"local", "Idle"}}},
	})
	out := buf.String()
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "local") || !strings.Contains(out, "Idle") {
		t.Fatalf("table render missing content: %q", out)
	}
}

func TestRenderDetailShowsIdentityAndEvents(t *testing.T) {
	ctx := context.Background()
	reg := resource.NewRegistry()
	if err := instance.RegisterKind(reg); err != nil {
		t.Fatalf("register: %v", err)
	}
	store := resource.NewMemory(reg)
	r, err := instance.Register(ctx, store, resource.Scope{}, "node-a", instance.Spec{Host: "h1", Version: "v1"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	ck, _ := resolveKind(reg, "instances")
	detail := controlplane.Detail{
		Resource: r,
		Columns:  []string{"NAME", "HOST"},
		Row:      controlplane.Row{Name: "node-a", Cells: []string{"node-a", "h1"}},
	}
	var buf bytes.Buffer
	renderDetail(&buf, ck.kind, detail)
	out := buf.String()
	for _, want := range []string{"Instance", "node-a", r.ID, "HOST"} {
		if !strings.Contains(out, want) {
			t.Fatalf("detail render missing %q in:\n%s", want, out)
		}
	}
}
