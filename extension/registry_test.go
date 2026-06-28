package extension

import (
	"strings"
	"testing"
)

func TestRegistryResolveAndHas(t *testing.T) {
	reg := NewRegistry()
	h := &recordHandler{capability: SurfaceIntegration}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !reg.Has(SurfaceIntegration) {
		t.Fatal("Has should report the registered surface")
	}
	got, err := reg.Resolve(SurfaceIntegration)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != h {
		t.Fatal("resolve returned a different handler")
	}
}

func TestRegistryResolveUnknownFailsClosed(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(&recordHandler{capability: SurfaceTool}); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err := reg.Resolve(SurfaceOps)
	if err == nil {
		t.Fatal("expected an error for an unregistered surface")
	}
	// The error should name the available surfaces, to aid diagnosis.
	if !strings.Contains(err.Error(), SurfaceTool) {
		t.Fatalf("error should list available surfaces, got %v", err)
	}
}

func TestRegistryRejectsNilEmptyAndDuplicate(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(nil); err == nil {
		t.Fatal("expected nil handler to be refused")
	}
	if err := reg.Register(&recordHandler{capability: ""}); err == nil {
		t.Fatal("expected empty capability to be refused")
	}
	if err := reg.Register(&recordHandler{capability: SurfaceAuth}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := reg.Register(&recordHandler{capability: SurfaceAuth}); err == nil {
		t.Fatal("expected duplicate capability to be refused")
	}
}

func TestRegistryCapabilitiesSorted(t *testing.T) {
	reg := NewRegistry()
	for _, c := range []string{SurfaceTool, SurfaceAuth, SurfaceIntegration} {
		if err := reg.Register(&recordHandler{capability: c}); err != nil {
			t.Fatalf("register %s: %v", c, err)
		}
	}
	got := reg.Capabilities()
	want := []string{SurfaceAuth, SurfaceIntegration, SurfaceTool} // sorted
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("not sorted: %v", got)
		}
	}
}
