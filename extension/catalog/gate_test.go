package catalog_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/extension/catalog"
	"github.com/ionalpha/flynn/internal/integrations"
	"github.com/ionalpha/flynn/internal/ops"
	"github.com/ionalpha/flynn/resource"
)

// validateSpec loads a spec through the real extension loader with the integration
// handler registered, so it runs exactly the validation a runtime load does: every
// surface routes to its handler, which decodes its block, validates the auth
// configuration, and decodes every operation flow. A spec that loads is one that
// will work in production. The spec is also admitted against the Extension kind
// schema by a kind-checking store.
func validateSpec(t *testing.T, name string, raw json.RawMessage) error {
	t.Helper()
	ctx := context.Background()

	// Admission: the kind schema must accept the envelope.
	reg := resource.NewRegistry()
	if err := extension.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	store := resource.NewMemory(reg)
	if _, err := store.Put(ctx, resource.Resource{
		APIVersion: extension.GroupVersion, Kind: extension.Kind, Name: name, Spec: raw,
	}); err != nil {
		return err
	}

	// Deep validation: load through the same handler set the runtime registers, so a
	// spec using any shipped surface (an API integration or a hosting ops provider) is
	// validated exactly as it will load in production.
	ereg := extension.NewRegistry()
	if err := ereg.Register(integrations.NewHandler()); err != nil {
		t.Fatalf("register handler: %v", err)
	}
	if err := ops.RegisterWith(ereg); err != nil {
		t.Fatalf("register ops handler: %v", err)
	}
	if err := ereg.Register(specOnlyProcess{}); err != nil {
		t.Fatalf("register process handler: %v", err)
	}
	loader := extension.NewLoader(ereg)
	_, err := loader.Load(ctx, resource.Resource{
		APIVersion: extension.GroupVersion, Kind: extension.Kind, ID: name, Name: name, Spec: raw,
	})
	return err
}

// specOnlyProcess validates a process surface the way the gate needs it validated: it
// decodes the block and checks nothing else. The production handler resolves the release,
// which downloads and verifies a signed artifact over the network, and launching an
// extension is not something a build-time gate should do. What the gate can prove without
// running anything is that the block a shipped spec carries is well-formed; that it names
// a release rather than a local binary is proven separately, by the catalog's own source
// check (TestCatalogRefusesDevSource).
type specOnlyProcess struct{}

func (specOnlyProcess) Capability() string { return extension.SurfaceProcess }

func (specOnlyProcess) OnLoad(_ context.Context, m extension.Mount) error {
	var block extension.ProcessBlock
	return json.Unmarshal(m.Block, &block)
}

func (specOnlyProcess) OnUnload(context.Context, string) error { return nil }

// TestCatalogGate is the build-time guarantee: every shipped official spec admits
// and loads. A broken official spec fails here, in CI, not at a user's runtime.
func TestCatalogGate(t *testing.T) {
	entries, err := catalog.Entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the catalog is empty")
	}
	for _, e := range entries {
		if err := validateSpec(t, e.Name, e.Raw); err != nil {
			t.Errorf("official spec %q is not loadable: %v", e.Name, err)
		}
	}
}

// TestCatalogGateBites proves the gate is not vacuous: a deliberately broken spec
// (a malformed operation flow expression) fails validation, so a real breakage would
// be caught rather than passing silently.
func TestCatalogGateBites(t *testing.T) {
	broken := json.RawMessage(`{
      "baseURL": "https://api.example.com",
      "auth": {"type": "none"},
      "surfaces": {
        "integration": {
          "operations": [
            {"name": "bad", "flow": {"steps": [
              {"op": "transform", "transform": {"value": "1 +"}}
            ]}}
          ]
        }
      }
    }`)
	if err := validateSpec(t, "broken", broken); err == nil {
		t.Fatal("the gate should reject a spec with a malformed operation flow")
	}
}
