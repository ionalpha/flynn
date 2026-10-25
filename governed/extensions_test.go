package governed

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/secret"
)

// noSecrets resolves nothing, for a test that never reaches a credential.
type noSecrets struct{}

func (noSecrets) Lookup(context.Context, string) (secret.Text, error) { return secret.Text{}, nil }

// TestExtensionOptions: the grants are off unless asked for, which is what makes a launched
// extension default-deny (no egress, no key, no network of its own).
func TestExtensionOptions(t *testing.T) {
	var cfg extensionConfig
	if cfg.egressGrant != nil || cfg.hostSigner != nil || cfg.hostFetcher != nil {
		t.Fatal("the zero runtime must grant nothing")
	}
	fetcher, err := extension.NewHTTPHostFetcher("https://rpc.example")
	if err != nil {
		t.Fatalf("fetcher: %v", err)
	}
	// There is no policy option to set: the host holds no parser, so it has nothing to
	// police a payload with. The signer does that, where the key is.
	for _, opt := range []ExtensionOption{
		WithExtensionEgress([]string{"rpc.example"}),
		WithExtensionSigner(func(string, string) extension.HostSigner { return nil }),
		WithExtensionFetcher(func(string, string) extension.HostFetcher { return fetcher }),
	} {
		opt(&cfg)
	}
	if len(cfg.egressGrant) != 1 || cfg.egressGrant[0] != "rpc.example" {
		t.Errorf("egress grant = %v, want the one granted", cfg.egressGrant)
	}
	if cfg.hostSigner == nil {
		t.Error("the signer grant was not installed")
	}
	if cfg.hostFetcher == nil || cfg.hostFetcher("token", "mint") == nil {
		t.Error("the host endpoint was not granted to the tool")
	}
}

// TestOpenIntegrationsServesBothSurfaces: the loader registers a handler for the API
// surface and the hosting surface from one registry, so an ops provider and an API
// integration load through the same engine.
func TestOpenIntegrationsServesBothSurfaces(t *testing.T) {
	reg := resource.NewRegistry()
	if err := extension.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	if err := credential.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	loader, err := OpenIntegrations(resource.NewMemory(reg), noSecrets{})
	if err != nil {
		t.Fatalf("OpenIntegrations: %v", err)
	}
	if loader == nil {
		t.Fatal("OpenIntegrations returned no loader")
	}
	if got := loader.Tools(); len(got) != 0 {
		t.Fatalf("no extension is loaded yet, so there should be no tools: %v", got)
	}
	var _ ToolBridge = loader
}

// TestOpenExtensionsRefusesADevSourceUnlessAskedTo: an unsigned local binary named by a spec
// in the store is not run by an embedder that did not ask for dev extensions. The path does
// not exist, so with the option on the load still fails, and for a different reason.
func TestOpenExtensionsRefusesADevSourceUnlessAskedTo(t *testing.T) {
	block, err := json.Marshal(extension.ProcessBlock{
		Dev: &extension.DevSource{Path: filepath.Join(t.TempDir(), "absent")},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	load := func(opts ...ExtensionOption) error {
		b, _ := memBackends(t)
		exts, err := OpenExtensions(context.Background(), b.Resources, t.TempDir(), opts...)
		if err != nil {
			t.Fatalf("OpenExtensions: %v", err)
		}
		return exts.Process.OnLoad(context.Background(), extension.Mount{ID: "x", Name: "x", Block: block})
	}

	if err := load(); err == nil || !strings.Contains(err.Error(), "dev mode is not enabled") {
		t.Fatalf("default load = %v, want the dev-mode refusal", err)
	}
	if err := load(WithDevExtensions()); err == nil || strings.Contains(err.Error(), "dev mode is not enabled") {
		t.Fatalf("load with dev extensions = %v, want a refusal for the missing binary", err)
	}
}

// TestOpenExtensionsSyncsTheCatalogAndIsDefaultDeny: opening the runtime records the
// bundled extensions and mounts nothing, so a host that opens it has launched no code.
func TestOpenExtensionsSyncsTheCatalogAndIsDefaultDeny(t *testing.T) {
	b, _ := memBackends(t)
	exts, err := OpenExtensions(context.Background(), b.Resources, t.TempDir())
	if err != nil {
		t.Fatalf("OpenExtensions: %v", err)
	}
	if exts.Loader == nil || exts.Process == nil {
		t.Fatal("OpenExtensions returned an incomplete runtime")
	}
	if got := exts.Loader.Tools(); len(got) != 0 {
		t.Fatalf("opening the runtime mounted tools: %v", got)
	}
	listed, err := b.Resources.List(context.Background(), extension.Kind, resource.Scope{}, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) == 0 {
		t.Fatal("the bundled catalog was not synced into the store")
	}
}
