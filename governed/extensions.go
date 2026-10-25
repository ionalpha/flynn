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

// Extensions is the extension machinery over a host's resource store: a loader whose only
// handler for a process extension is [Extensions.Process], and through the loader the tools
// a run may mount. The loader satisfies [ToolBridge], so it is passed to [WithExtensions]
// as it is.
type Extensions struct {
	// Loader mounts and unmounts extensions and lists the tools they provide.
	Loader *extension.Loader
	// Process is the handler that launches a process extension, kept so the host can reach a
	// signer extension over its private channel. A signer's tools are never mounted, so
	// there is no other way to it.
	Process *extension.ProcessHandler
}

// ExtensionOption configures [OpenExtensions].
type ExtensionOption func(*extensionConfig)

type extensionConfig struct {
	egressGrant []string
	hostSigner  func(ext, tool string) extension.HostSigner
	hostFetcher func(ext, tool string) extension.HostFetcher
}

// WithExtensionEgress sets the host's outbound allow-list for a launched extension. The
// effective egress is this list intersected with the spec's request, so it can only narrow
// what the spec asks for. With none given, every extension launches with egress denied.
func WithExtensionEgress(hosts []string) ExtensionOption {
	return func(c *extensionConfig) { c.egressGrant = hosts }
}

// WithExtensionSigner grants a host-held signing key to specific mounted tools. fn returns
// the signer a tool may obtain signatures from, or nil for a tool that does not sign. The
// key stays in the host: a tool that builds something needing a signature hands out the
// bytes and receives only the signature back. With none given, every tool is non-signing.
func WithExtensionSigner(fn func(ext, tool string) extension.HostSigner) ExtensionOption {
	return func(c *extensionConfig) { c.hostSigner = fn }
}

// WithExtensionFetcher grants a host-held endpoint to specific mounted tools. fn returns
// the fetcher a tool may send requests through, or nil for a tool that borrows no network.
// The tool hands out the request bytes and receives the response, and never names, or
// influences, the destination, which is how an extension reaches a service while its own
// egress is denied. With none given, every tool is network-free.
func WithExtensionFetcher(fn func(ext, tool string) extension.HostFetcher) ExtensionOption {
	return func(c *extensionConfig) { c.hostFetcher = fn }
}

// OpenExtensions syncs the bundled catalog into store and builds a loader whose only
// handler is the process handler. A released extension resolves by downloading it and
// proving its origin against the pinned signing identity; a dev extension resolves by its
// local path, which is unsigned and honoured here because this function is the explicit
// opt-in. A spec that declares a release is never satisfied by a local binary, so a stray
// dev block cannot downgrade a signed extension to an unsigned one. Every launch is
// confined, and is refused rather than run unconfined. Each launch's scratch directory
// lives under dataDir and is removed when the connection stops.
func OpenExtensions(ctx context.Context, store resource.Store, dataDir string, opts ...ExtensionOption) (*Extensions, error) {
	var cfg extensionConfig
	for _, o := range opts {
		o(&cfg)
	}
	// A bundled process extension names a signed release and resolves through
	// verification, so syncing installs nothing and downloads nothing: it records that the
	// host knows of the extension, which is what makes an official one listable and callable
	// out of the box.
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
			Dev: extension.DevResolver{Enabled: true},
		},
		procOpts...,
	)
	reg := extension.NewRegistry()
	if err := reg.Register(handler); err != nil {
		return nil, err
	}
	return &Extensions{Loader: extension.NewLoader(reg), Process: handler}, nil
}

// OpenIntegrations builds the loader that serves API integrations and hosting providers
// from one operation engine over store. Credentials come from the credential records in
// store and the secrets behind them from secrets, so an operation runs through the same
// credential resolution, role enforcement and egress confinement wherever it is started.
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
