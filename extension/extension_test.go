package extension

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/resource"
)

// TestKindRoundTripsThroughStore proves an Extension is an ordinary resource: it is
// admitted against the kind schema, assigned identity and a version, and read back
// with its spec intact, exactly like every other kind.
func TestKindRoundTripsThroughStore(t *testing.T) {
	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	store := resource.NewMemory(reg)
	ctx := context.Background()

	spec := Spec{
		DisplayName:  "Example API",
		Version:      "1.0.0",
		Provider:     "example",
		BaseURL:      "https://api.example.com",
		Capabilities: []string{"example.read"},
		Auth:         AuthSpec{Type: "bearer", CredentialRef: "example-token"},
		Safety:       SafetySpec{ReadOnly: true},
		Surfaces: map[string]json.RawMessage{
			SurfaceIntegration: json.RawMessage(`{"endpoints":["/v1/things"]}`),
		},
	}
	body, err := spec.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	put, err := store.Put(ctx, resource.Resource{
		APIVersion: GroupVersion,
		Kind:       Kind,
		Name:       "example",
		Spec:       body,
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if put.ID == "" {
		t.Fatal("store did not assign an id")
	}
	if put.Version == 0 {
		t.Fatal("store did not assign a version")
	}

	got, err := store.Get(ctx, Kind, resource.Scope{}, "example")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	decoded, err := DecodeSpec(got)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.DisplayName != spec.DisplayName || decoded.BaseURL != spec.BaseURL {
		t.Fatalf("round-trip mismatch: %+v", decoded)
	}
	if decoded.Auth.Type != "bearer" || decoded.Auth.CredentialRef != "example-token" {
		t.Fatalf("auth not preserved: %+v", decoded.Auth)
	}
	raw, ok := decoded.Surface(SurfaceIntegration)
	if !ok || !strings.Contains(string(raw), "things") {
		t.Fatalf("surface block not preserved: %q", raw)
	}
}

// TestSchemaRejectsUnknownField proves the kind schema is real admission control:
// a spec with a stray top-level key is refused before it is stored.
func TestSchemaRejectsUnknownField(t *testing.T) {
	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	store := resource.NewMemory(reg)

	_, err := store.Put(context.Background(), resource.Resource{
		APIVersion: GroupVersion,
		Kind:       Kind,
		Name:       "bad",
		Spec:       json.RawMessage(`{"baseURL":"https://x","bogusField":true}`),
	})
	if err == nil {
		t.Fatal("expected admission to reject an unknown field")
	}
	if !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}

// TestSurfacesStayOpen proves a surface key the kind does not know is still
// admitted: the schema constrains the envelope, never the individual surface
// blocks, which is what keeps the surface set open for new handlers.
func TestSurfacesStayOpen(t *testing.T) {
	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	store := resource.NewMemory(reg)

	_, err := store.Put(context.Background(), resource.Resource{
		APIVersion: GroupVersion,
		Kind:       Kind,
		Name:       "novel",
		Spec:       json.RawMessage(`{"surfaces":{"some-future-surface":{"anything":42}}}`),
	})
	if err != nil {
		t.Fatalf("a novel surface block should be admitted, got %v", err)
	}
}

// TestDecodeEmptySpec proves a bare extension (no spec bytes) decodes to the zero
// Spec rather than erroring.
func TestDecodeEmptySpec(t *testing.T) {
	s, err := DecodeSpec(resource.Resource{})
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if len(s.Surfaces) != 0 || s.BaseURL != "" {
		t.Fatalf("expected zero spec, got %+v", s)
	}
}

// TestStatusEncodeDecode round-trips a Status.
func TestStatusEncodeDecode(t *testing.T) {
	st := Status{ObservedGeneration: 3, Grade: GradeProbed, Enabled: true, MountedSurfaces: []string{"integration"}}
	body, err := st.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeStatus(resource.Resource{Status: body})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Grade != GradeProbed || !got.Enabled || got.ObservedGeneration != 3 {
		t.Fatalf("status round-trip mismatch: %+v", got)
	}
}
