package playbook

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/resource"
)

// TestCatalogGate is the build-time guarantee: every shipped playbook admits against the
// kind schema and its flow is a well-formed, decodable procedure. A broken official
// playbook fails here, in CI, not at a user's first run.
func TestCatalogGate(t *testing.T) {
	entries, err := Entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the playbook catalog is empty")
	}

	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	store := resource.NewMemory(reg)

	for _, e := range entries {
		// Admission: the kind schema must accept the spec bytes as shipped.
		if _, err := store.Put(context.Background(), resource.Resource{
			APIVersion: GroupVersion, Kind: Kind, Name: e.Name, Spec: e.Raw,
		}); err != nil {
			t.Errorf("official playbook %q is not admitted: %v", e.Name, err)
			continue
		}
		// Deep validation: the flow must decode and validate (every op well-formed, ids
		// unique, expressions and templates parse).
		if _, err := e.Spec.DecodeFlow(); err != nil {
			t.Errorf("official playbook %q has an invalid flow: %v", e.Name, err)
		}
		// A playbook that registers a service must classify it with a provider.
		if e.Spec.Service != nil && e.Spec.Service.Provider == "" {
			t.Errorf("official playbook %q declares a service with no provider", e.Name)
		}
	}
}

// TestCatalogGateBites proves the gate is not vacuous: a playbook whose flow uses an op
// that does not exist fails validation, so a real breakage would be caught.
func TestCatalogGateBites(t *testing.T) {
	bad := Spec{Flow: []byte(`{"steps":[{"op":"teleport","teleport":{}}]}`)}
	if _, err := bad.DecodeFlow(); err == nil {
		t.Fatal("a flow with an unknown op must fail to decode")
	}
}
