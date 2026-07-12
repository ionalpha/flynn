package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/extension/catalog"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
)

// runExtensions implements `flynn extensions <subcommand>`: the extension authoring
// loop. It links a locally-built extension binary, lists what is installed, calls one
// of its tools to test the round trip, and unlinks it. No release, signing, or
// download is involved, which is what makes the inner loop fast: build the binary,
// point flynn at it, call a tool, rebuild, call again.
//
//	flynn extensions dev <name> <binary> [flags]   link a locally-built binary as a dev extension
//	flynn extensions ls                            list installed extensions and their source
//	flynn extensions call <name> <tool> [json]     launch the extension confined and call one tool
//	flynn extensions rm <name>                     unlink a dev extension
//
// A dev extension runs UNSIGNED local code, so it is honoured only under this
// explicitly dev-enabled path (and a served run with dev mode turned on); a normal run
// refuses it. It is marked DEV wherever it is listed so it is never mistaken for a
// verified, signed extension.
// Usage errors for the subcommands whose flags follow positional arguments, so the
// message is identical whether the positionals are missing or an extra one is given.
var (
	errUsageDev  = errors.New("usage: flynn extensions dev <name> <binary> [--cap ...] [--tool ...] [--egress ...] [--arg ...]")
	errUsageCall = errors.New("usage: flynn extensions call <name> <tool> [json-input] [--egress ...]")
)

func runExtensions(args []string, dataDir string) error {
	if len(args) == 0 {
		return errors.New("usage: flynn extensions <dev|ls|call|rm>")
	}
	ctx := context.Background()
	switch args[0] {
	case "dev", "link":
		return extensionsDev(ctx, dataDir, args[1:])
	case "ls", "list":
		return extensionsList(ctx, dataDir)
	case "call":
		return extensionsCall(ctx, dataDir, args[1:])
	case "rm", "unlink":
		return extensionsRemove(ctx, dataDir, args[1:])
	default:
		return fmt.Errorf("extensions: unknown subcommand %q (want dev, ls, call, or rm)", args[0])
	}
}

// extensionsDev links a locally-built binary as a dev (unsigned) extension. It resolves
// the binary to an absolute path and stat-checks it now so a bad path is reported at
// link time, builds the process-surface spec, and persists it marked dev. The
// DevResolver re-checks the path at launch, so a binary deleted between link and call
// still fails closed.
func extensionsDev(ctx context.Context, dataDir string, args []string) error {
	fs := newFlagSet("extensions dev")
	var (
		caps   = fs.String("cap", "", "comma-separated capability tags that gate this extension")
		tools  = fs.String("tool", "", "comma-separated allow-list of tool names to mount (default: all the server advertises)")
		egress = fs.String("egress", "", "comma-separated hostnames the extension may reach (still intersected with the operator grant at call time)")
		binArg = fs.String("arg", "", "comma-separated fixed arguments passed to the binary verbatim")
	)
	// The name and binary are leading positionals; flags follow them. Split them out
	// before parsing so the friendly order `dev <name> <binary> --cap ...` works (the
	// flag package otherwise stops at the first positional and leaves the flags unparsed).
	if len(args) < 2 {
		return errUsageDev
	}
	name, binary := args[0], args[1]
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errUsageDev
	}
	if err := validateExtensionName(name); err != nil {
		return err
	}

	abs, err := resolveDevBinary(binary)
	if err != nil {
		return err
	}

	spec, err := buildDevSpec(name, abs, splitList(*caps), splitList(*tools), splitList(*egress), splitList(*binArg))
	if err != nil {
		return err
	}

	rt, err := openExtensionRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.close() }()

	if err := putDevExtension(ctx, rt.store, name, spec); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "linked dev extension %q -> %s (DEV, unsigned)\n", name, abs)
	_, _ = fmt.Fprintf(os.Stdout, "test it with: flynn extensions call %s <tool> '<json>'\n", name)
	return nil
}

// extensionsList prints every installed extension with its source, so a dev link is
// visible and unmistakably marked unsigned next to bundled and forked entries.
func extensionsList(ctx context.Context, dataDir string) error {
	rt, err := openExtensionRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.close() }()
	return listExtensions(ctx, rt.store, os.Stdout)
}

// extensionsCall launches a dev extension confined and invokes one of its tools,
// printing the result. It is the authoring inner loop's fast feedback: it runs the real
// path a served agent would (resolve the local binary, launch it inside the sandbox,
// speak MCP, mount its tools, call one), so a tool that works here works in a run. A
// released source is refused here on purpose; this command is the dev path only.
//
// The tool name may be given bare (mint) or namespaced (token.mint); both resolve to
// the mounted "<name>.<tool>". Authority to call a tool is enforced separately at the
// dispatch waist in a real run; this command invokes it directly to test the round trip.
func extensionsCall(ctx context.Context, dataDir string, args []string) error {
	fs := newFlagSet("extensions call")
	egress := fs.String("egress", "", "comma-separated hostnames to grant the extension for this call (operator grant; effective egress is this intersected with the spec)")
	sign := fs.String("sign", "", "path to a dev signing key (a JSON array of the 64 raw key bytes) the called tool signs its work with; the key stays in the host and the tool only receives signatures")
	endpoint := fs.String("endpoint", "", "an http(s) endpoint the called tool may reach THROUGH THE HOST; the tool stays network-denied and only hands out request bytes, so it can never reach anywhere else")
	localEndpoint := fs.Bool("endpoint-local", false, "permit --endpoint to be a loopback or private literal address (a local test node); off by default, because a grant must not silently aim the host at its own network")
	// name, tool, and an optional JSON input are leading positionals; flags follow. Pull
	// the positionals out first so `call <name> <tool> <json> --egress ...` parses.
	if len(args) < 2 {
		return errUsageCall
	}
	name, toolName := args[0], args[1]
	input := json.RawMessage(`{}`)
	rest := args[2:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		if rest[0] != "" {
			input = json.RawMessage(rest[0])
		}
		rest = rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errUsageCall
	}

	runtimeOpts := []extRuntimeOption{withEgressGrant(splitList(*egress))}
	bareTool := strings.TrimPrefix(toolName, name+".")
	if *sign != "" {
		signer, err := loadDevSigner(*sign)
		if err != nil {
			return err
		}
		runtimeOpts = append(runtimeOpts, withHostSigner(func(ext, tool string) extension.HostSigner {
			if ext == name && tool == bareTool {
				return signer
			}
			return nil
		}))
	}
	if *endpoint != "" {
		var fopts []extension.FetcherOption
		if *localEndpoint {
			fopts = append(fopts, extension.WithPrivateEndpoint())
		}
		fetcher, err := extension.NewHTTPHostFetcher(*endpoint, fopts...)
		if err != nil {
			return err
		}
		// The grant is for THIS tool only. Every other mounted tool gets nil, so it borrows no
		// network at all: default-deny, one authority, one destination.
		runtimeOpts = append(runtimeOpts, withHostFetcher(func(ext, tool string) extension.HostFetcher {
			if ext == name && tool == bareTool {
				return fetcher
			}
			return nil
		}))
	}

	rt, err := openExtensionRuntime(ctx, dataDir, runtimeOpts...)
	if err != nil {
		return err
	}
	defer func() { _ = rt.close() }()

	r, err := rt.store.Get(ctx, extension.Kind, resource.Scope{}, name)
	if errors.Is(err, resource.ErrNotFound) {
		return fmt.Errorf("extensions: unknown extension %q (see flynn extensions ls)", name)
	}
	if err != nil {
		return err
	}
	if _, err := rt.loader.Load(ctx, r); err != nil {
		return err
	}
	defer func() { _ = rt.loader.Unload(ctx, r.ID) }()

	tool := findExtensionTool(rt.loader.Tools(), name, toolName)
	if tool == nil {
		return fmt.Errorf("extensions: %q has no tool %q (call it with a tool the binary advertises)", name, toolName)
	}
	out, err := tool.Invoke(ctx, input)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stdout, out)
	return nil
}

// extensionsRemove unlinks a dev extension. It refuses to delete a bundled or forked
// extension so this command cannot remove catalog state; those are managed elsewhere.
func extensionsRemove(ctx context.Context, dataDir string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: flynn extensions rm <name>")
	}
	name := args[0]
	rt, err := openExtensionRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.close() }()

	if err := deleteDevExtension(ctx, rt.store, name); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "unlinked dev extension %q\n", name)
	return nil
}

// buildDevSpec assembles the Extension spec for a dev link: a single process surface
// pointing at the local binary by absolute path, plus the optional capability tags,
// tool allow-list, egress request, and fixed args. The path must already be absolute
// (the resolver requires it); a relative path is a programming error here.
func buildDevSpec(name, absPath string, caps, tools, egress, binArgs []string) (extension.Spec, error) {
	if !filepath.IsAbs(absPath) {
		return extension.Spec{}, fmt.Errorf("extensions: dev binary path %q must be absolute", absPath)
	}
	block := extension.ProcessBlock{
		Dev:   &extension.DevSource{Path: absPath},
		Args:  binArgs,
		Tools: tools,
	}
	blockRaw, err := json.Marshal(block)
	if err != nil {
		return extension.Spec{}, fmt.Errorf("extensions: encode process surface: %w", err)
	}
	return extension.Spec{
		DisplayName:  name,
		Capabilities: caps,
		Safety:       extension.SafetySpec{EgressAllow: egress},
		Surfaces:     map[string]json.RawMessage{extension.SurfaceProcess: blockRaw},
	}, nil
}

// putDevExtension writes (or replaces) a dev extension resource, marked with the dev
// source label so the catalog sync never touches it and every listing shows it
// unsigned. Re-linking a rebuilt binary preserves the existing observed status (e.g. a
// prior enable), so a rebuild does not silently reset it; a fresh link starts with none.
func putDevExtension(ctx context.Context, store resource.Store, name string, spec extension.Spec) error {
	specRaw, err := spec.Encode()
	if err != nil {
		return err
	}
	var status json.RawMessage
	if existing, err := store.Get(ctx, extension.Kind, resource.Scope{}, name); err == nil {
		status = existing.Status
	} else if !errors.Is(err, resource.ErrNotFound) {
		return err
	}
	r := resource.Resource{
		APIVersion: extension.GroupVersion,
		Kind:       extension.Kind,
		Name:       name,
		Labels:     map[string]string{catalog.SourceLabel: catalog.SourceDev},
		Spec:       specRaw,
		Status:     status,
	}
	if _, err := store.Put(ctx, r); err != nil {
		return fmt.Errorf("extensions: link %q: %w", name, err)
	}
	return nil
}

// deleteDevExtension removes a dev link. It refuses a non-dev extension so a bundled or
// forked catalog entry cannot be deleted through the dev command.
func deleteDevExtension(ctx context.Context, store resource.Store, name string) error {
	r, err := store.Get(ctx, extension.Kind, resource.Scope{}, name)
	if errors.Is(err, resource.ErrNotFound) {
		return fmt.Errorf("extensions: unknown extension %q (see flynn extensions ls)", name)
	}
	if err != nil {
		return err
	}
	if src := r.Labels[catalog.SourceLabel]; src != catalog.SourceDev {
		return fmt.Errorf("extensions: %q is not a dev link (source %q); refusing to remove it here", name, orUser(src))
	}
	if err := store.Delete(ctx, extension.Kind, resource.Scope{}, name); err != nil {
		return fmt.Errorf("extensions: remove %q: %w", name, err)
	}
	return nil
}

// listExtensions renders every installed extension with its source and whether it is a
// signed release or an unsigned dev link.
func listExtensions(ctx context.Context, store resource.Store, w io.Writer) error {
	exts, err := store.List(ctx, extension.Kind, resource.Scope{}, nil)
	if err != nil {
		return err
	}
	if len(exts) == 0 {
		_, _ = fmt.Fprintln(w, "no extensions installed")
		return nil
	}
	sort.Slice(exts, func(i, j int) bool { return exts[i].Name < exts[j].Name })

	_, _ = fmt.Fprintf(w, "  %-18s %-14s %-10s %s\n", "EXTENSION", "SOURCE", "SIGNED", "CAPABILITIES")
	for _, r := range exts {
		spec, _ := extension.DecodeSpec(r)
		source := orUser(r.Labels[catalog.SourceLabel])
		signed := "yes"
		if source == catalog.SourceDev {
			signed = "DEV"
		}
		caps := "-"
		if len(spec.Capabilities) > 0 {
			caps = strings.Join(spec.Capabilities, ",")
		}
		_, _ = fmt.Fprintf(w, "  %-18s %-14s %-10s %s\n", r.Name, source, signed, caps)
	}
	return nil
}

// resolveDevBinary turns a user-supplied binary path into a verified absolute path,
// reporting a missing file or a directory at link time rather than at first call.
func resolveDevBinary(binary string) (string, error) {
	abs, err := filepath.Abs(binary)
	if err != nil {
		return "", fmt.Errorf("extensions: resolve %q: %w", binary, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("extensions: %q: %w", abs, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("extensions: %q is a directory, not a binary", abs)
	}
	return abs, nil
}

// findExtensionTool resolves a tool by bare or namespaced name against the mounted
// tools of one extension. The mounted name is always "<ext>.<tool>", so a bare name is
// matched by prefixing the extension name.
func findExtensionTool(tools []mission.Tool, extName, toolName string) mission.Tool {
	full := extName + "." + toolName
	for _, t := range tools {
		if n := t.Def().Name; n == toolName || n == full {
			return t
		}
	}
	return nil
}

// validateExtensionName rejects a name that would break namespacing or resource keys. A
// dot is reserved: mounted tool names are "<ext>.<tool>", so a dot in the extension name
// would make a tool's namespaced identity ambiguous.
func validateExtensionName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("extensions: name must not be empty")
	}
	if strings.ContainsAny(name, ". /\\") {
		return fmt.Errorf("extensions: name %q must not contain a dot, slash, or space", name)
	}
	return nil
}

// splitList parses a comma-separated flag value into a trimmed, non-empty list, or nil
// for an empty value, so an unset list flag leaves the corresponding spec field empty.
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func orUser(s string) string {
	if s == "" {
		return "user"
	}
	return s
}

// newFlagSet builds a subcommand flag set that reports parse errors to the caller
// instead of exiting the process, so a bad flag surfaces as a usage error the dispatch
// turns into exit code 2.
func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}

// extensionRuntime bundles the durable store and a loader wired with the dev process
// handler, so the extensions commands resolve and launch a linked binary exactly as a
// served run would, minus the release download and signature check.
type extensionRuntime struct {
	store  resource.Store
	loader *extension.Loader
	close  func() error
}

// extRuntimeOptions configures a runtime built by openExtensionRuntime.
type extRuntimeOptions struct {
	egressGrant []string
	hostSigner  func(ext, tool string) extension.HostSigner
	hostFetcher func(ext, tool string) extension.HostFetcher
}

// extRuntimeOption customizes the runtime.
type extRuntimeOption func(*extRuntimeOptions)

// withEgressGrant sets the operator's outbound host allow-list for a launched
// extension. The effective egress is this grant intersected with the spec's request, so
// a grant can only narrow what the spec asks for, never widen it, and the default (no
// grant) launches every extension with egress fully denied.
func withEgressGrant(hosts []string) extRuntimeOption {
	return func(o *extRuntimeOptions) { o.egressGrant = hosts }
}

// withHostSigner grants a host-held signing key to specific mounted tools. fn returns the
// signer a tool may obtain signatures from, or nil for a tool that does not sign. The key
// stays in the host; a tool that builds something needing a signature hands out the bytes and
// receives only the signature back. The default (no fn) leaves every tool non-signing.
func withHostSigner(fn func(ext, tool string) extension.HostSigner) extRuntimeOption {
	return func(o *extRuntimeOptions) { o.hostSigner = fn }
}

// withHostFetcher grants a host-held endpoint to specific mounted tools. fn returns the fetcher a
// tool may send requests through, or nil for a tool that borrows no network. The endpoint stays in
// the host: a tool that needs to reach a service hands out the request bytes and receives the
// response, and it never names, and cannot influence, the destination. This is how an extension
// speaks to a service while running with its own egress fully denied. The default (no fn) leaves
// every tool network-free.
func withHostFetcher(fn func(ext, tool string) extension.HostFetcher) extRuntimeOption {
	return func(o *extRuntimeOptions) { o.hostFetcher = fn }
}

// loadDevSigner reads an ed25519 signing key from a file holding a JSON array of the 64 raw
// private-key bytes, and returns a host signer over it. This is the dev path: a key on disk
// for the authoring and testing loop. A production deployment supplies a vault- or
// hardware-backed HostSigner instead, so no key ever sits in a file.
func loadDevSigner(path string) (extension.HostSigner, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is the operator-supplied dev signing key
	if err != nil {
		return nil, fmt.Errorf("extensions: read signing key: %w", err)
	}
	var nums []int
	if err := json.Unmarshal(raw, &nums); err != nil {
		return nil, fmt.Errorf("extensions: signing key must be a JSON array of byte values: %w", err)
	}
	if len(nums) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("extensions: signing key must be %d bytes, got %d", ed25519.PrivateKeySize, len(nums))
	}
	key := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
	for i, n := range nums {
		if n < 0 || n > 255 {
			return nil, fmt.Errorf("extensions: signing key byte %d is out of range", i)
		}
		key[i] = byte(n)
	}
	return extension.NewEd25519HostSigner(key)
}

// openExtensionRuntime opens the durable store and wires a loader whose only surface
// handler is the process handler running in dev mode: it resolves a local dev binary
// (DevResolver, unsigned but explicitly opted in here) and launches it confined
// (SandboxLauncher, refuse-rather-than-downgrade). A released source is refused by the
// dev resolver, so this path can never run remote or unverified code. The scratch jail
// for each launch lives under the data directory and is removed when the connection
// stops.
func openExtensionRuntime(ctx context.Context, dataDir string, opts ...extRuntimeOption) (*extensionRuntime, error) {
	var cfg extRuntimeOptions
	for _, opt := range opts {
		opt(&cfg)
	}

	durable, err := openDataStore(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	reg, err := missionRegistry()
	if err != nil {
		_ = durable.Close()
		return nil, err
	}
	store := durable.Resources(reg)

	workRoot := filepath.Join(dataDir, "extension-run")
	if err := os.MkdirAll(workRoot, 0o700); err != nil {
		_ = durable.Close()
		return nil, fmt.Errorf("extensions: create run directory: %w", err)
	}

	ereg := extension.NewRegistry()
	procOpts := []extension.ProcessOption{
		extension.WithEgressGrant(cfg.egressGrant),
	}
	if cfg.hostSigner != nil {
		procOpts = append(procOpts, extension.WithHostSigner(cfg.hostSigner))
	}
	if cfg.hostFetcher != nil {
		procOpts = append(procOpts, extension.WithHostFetcher(cfg.hostFetcher))
	}
	handler := extension.NewProcessHandler(
		extension.NewSandboxLauncher(workRoot),
		extension.DevResolver{Enabled: true},
		procOpts...,
	)
	if err := ereg.Register(handler); err != nil {
		_ = durable.Close()
		return nil, err
	}
	return &extensionRuntime{
		store:  store,
		loader: extension.NewLoader(ereg),
		close:  durable.Close,
	}, nil
}
