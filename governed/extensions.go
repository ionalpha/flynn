package governed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/extension/catalog"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/integrations"
	"github.com/ionalpha/flynn/internal/ops"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/secret"
)

// Extensions is the extension loader over a host's store. The loader satisfies
// [ToolBridge], so it goes to [WithExtensions] as it is.
type Extensions struct {
	Loader *extension.Loader
	// Process is kept so the host can reach a signer extension over its private channel; a
	// signer's tools are never mounted, so there is no other way to it.
	Process *extension.ProcessHandler
}

// ExtensionOption configures [OpenExtensions].
type ExtensionOption func(*extensionConfig)

type extensionConfig struct {
	egressGrant []string
	hostSigner  func(ext, tool string) extension.HostSigner
	hostFetcher func(ext, tool string) extension.HostFetcher
	devSources  bool
}

// WithDevExtensions honours a dev source, an unsigned local binary. Without it only a signed
// release resolves, so a spec in the store cannot hand a host an unsigned binary. Never set
// it on a deployed host.
func WithDevExtensions() ExtensionOption {
	return func(c *extensionConfig) { c.devSources = true }
}

// WithExtensionEgress sets the host's egress allow-list, intersected with each spec's
// request. Unset, every extension launches with egress denied.
func WithExtensionEgress(hosts []string) ExtensionOption {
	return func(c *extensionConfig) { c.egressGrant = hosts }
}

// WithExtensionSigner grants a host-held key to the tools fn returns a signer for. The key
// stays in the host: a tool sends bytes and gets only the signature back.
func WithExtensionSigner(fn func(ext, tool string) extension.HostSigner) ExtensionOption {
	return func(c *extensionConfig) { c.hostSigner = fn }
}

// WithExtensionFetcher grants a host-held endpoint to the tools fn returns a fetcher for.
// The tool sends request bytes and never names the destination, so it reaches a service
// while its own egress stays denied.
func WithExtensionFetcher(fn func(ext, tool string) extension.HostFetcher) ExtensionOption {
	return func(c *extensionConfig) { c.hostFetcher = fn }
}

// OpenExtensions syncs the bundled catalog into store and builds the loader. A release
// resolves only after its origin is proven against the pinned signing identity, and a spec
// that declares one is never satisfied by a local binary, so a stray dev block cannot
// downgrade it. A launch that cannot be confined is refused. Scratch directories under
// dataDir are removed when the connection stops.
func OpenExtensions(ctx context.Context, store resource.Store, dataDir string, opts ...ExtensionOption) (*Extensions, error) {
	var cfg extensionConfig
	for _, o := range opts {
		o(&cfg)
	}
	// Syncing downloads nothing; it records the official extensions so they are listable
	// and callable out of the box.
	if _, err := catalog.Sync(ctx, store); err != nil {
		return nil, err
	}
	workRoot := filepath.Join(dataDir, "extension-run")
	if err := os.MkdirAll(workRoot, 0o700); err != nil {
		return nil, fmt.Errorf("extensions: create run directory: %w", err)
	}

	procOpts := []extension.ProcessOption{extension.WithEgressGrant(cfg.egressGrant)}
	if cfg.hostSigner != nil {
		procOpts = append(procOpts, extension.WithHostSigner(cfg.hostSigner))
	}
	if cfg.hostFetcher != nil {
		procOpts = append(procOpts, extension.WithHostFetcher(cfg.hostFetcher))
	}
	handler := extension.NewProcessHandler(
		extension.NewSandboxLauncher(workRoot),
		extension.SourceResolver{
			Release: extension.ReleaseResolver{
				Origin:     extension.DefaultOrigin,
				Dir:        filepath.Join(dataDir, "extensions"),
				Downloader: fetch.New(),
			},
			Dev: extension.DevResolver{Enabled: cfg.devSources},
		},
		procOpts...,
	)
	reg := extension.NewRegistry()
	if err := reg.Register(handler); err != nil {
		return nil, err
	}
	return &Extensions{Loader: extension.NewLoader(reg), Process: handler}, nil
}

// OpenIntegrations serves API integrations and hosting providers from one operation engine,
// so every operation gets the same credential resolution, role enforcement and egress
// confinement wherever it starts.
func OpenIntegrations(store resource.Store, secrets secret.Source) (*extension.Loader, error) {
	opts := []integrations.Option{
		integrations.WithSecrets(secrets),
		integrations.WithCredentials(credential.NewStore(store)),
	}
	reg := extension.NewRegistry()
	if err := reg.Register(integrations.NewHandler(opts...)); err != nil {
		return nil, err
	}
	if err := ops.RegisterWith(reg, opts...); err != nil {
		return nil, err
	}
	return extension.NewLoader(reg), nil
}
