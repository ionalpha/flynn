package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/governed"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/extension/catalog"
	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
)

// Shared usage errors, so a missing and an extra positional get the same message.
var (
	errUsageDev  = usageErr("extensions dev")
	errUsageCall = usageErr("extensions call")
)

// runExtensions implements `flynn extensions <subcommand>`. A bundled extension is an
// official one this binary knows; calling it downloads the signed release, proves its
// origin and runs it. A dev extension is a local build linked by path, for a fast
// authoring loop.
//
//	flynn extensions dev <name> <binary> [flags]   link a locally-built binary as a dev extension
//	flynn extensions ls                            list installed extensions and their source
//	flynn extensions call <name> <tool> [json]     launch the extension confined and call one tool
//	flynn extensions rm <name>                     unlink a dev extension
//
// A dev extension is unsigned local code, honoured only on this path and on a served
// run with dev mode on. Every listing marks it DEV so it is never taken for a signed one.
func runExtensions(args []string, dataDir string) error {
	if len(args) == 0 {
		return usageErr("extensions")
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
		return unknownCommand("extensions", args[0])
	}
}

// extensionsDev links a local binary as an unsigned dev extension. The path is checked
// at link time to report a bad one early, and the DevResolver checks again at launch,
// so a binary deleted in between fails closed.
func extensionsDev(ctx context.Context, dataDir string, args []string) error {
	fs := newFlagSet("extensions dev")
	var (
		caps   = fs.String("cap", "", "comma-separated capability tags that gate this extension")
		tools  = fs.String("tool", "", "comma-separated allow-list of tool names to mount (default: all the server advertises)")
		egress = fs.String("egress", "", "comma-separated hostnames the extension may reach (still intersected with the operator grant at call time)")
		binArg = fs.String("arg", "", "comma-separated fixed arguments passed to the binary verbatim")
	)
	// The flag package stops at the first positional, so name and binary are split off
	// before parsing to allow `dev <name> <binary> --cap ...`.
	if len(args) < 2 {
		return errUsageDev
	}
	if err := refuseFlagWords("extensions dev", args[:2]); err != nil {
		return err
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

// extensionsList prints every installed extension with its source, marking dev links
// unsigned.
func extensionsList(ctx context.Context, dataDir string) error {
	rt, err := openExtensionRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.close() }()
	return listExtensions(ctx, rt.store, os.Stdout)
}

// extensionsCall launches an extension confined and calls one tool, on the same path a
// served agent uses (resolve, sandboxed launch, MCP, mount, call), so a tool that works
// here works in a run. A bundled extension is downloaded and origin-proven before it
// runs; a dev extension runs its unsigned local binary.
//
// The tool may be named bare (mint) or namespaced (token.mint). A real run enforces
// call authority at the dispatch waist; this command calls the tool directly to test
// the round trip.
func extensionsCall(ctx context.Context, dataDir string, args []string) error {
	req, err := parseCallArgs(args)
	if err != nil {
		return err
	}

	// routed is set after the signer mounts below. The runtime takes the grant closure at
	// open time but calls it only when the worker mounts, after the signer is up.
	var routed extension.HostSigner
	runtimeOpts, err := req.runtimeOptions(&routed)
	if err != nil {
		return err
	}
	rt, err := openExtensionRuntime(ctx, dataDir, runtimeOpts...)
	if err != nil {
		return err
	}
	defer func() { _ = rt.close() }()

	// The worker's mount asks for its signer, so the signer is mounted and unlocked first.
	// A signer that will not unlock stops the run before the worker does anything.
	if req.signer != "" {
		routed, err = mountSigner(ctx, rt, dataDir, req.signer)
		if err != nil {
			return err
		}
	}
	return invokeExtensionTool(ctx, rt, req.name, req.tool, req.input)
}

type callRequest struct {
	name, tool string
	input      json.RawMessage
	egress     []string
	// signer names the extension that signs for the called tool.
	signer string
	// endpoint is the one destination the called tool may reach through the host.
	endpoint      string
	localEndpoint bool
}

// parseCallArgs splits name, tool and optional JSON input from the flags after them, so
// `call <name> <tool> <json> --egress ...` parses.
func parseCallArgs(args []string) (callRequest, error) {
	fs := newFlagSet("extensions call")
	egress := fs.String("egress", "", "comma-separated hostnames to grant the extension for this call (operator grant; effective egress is this intersected with the spec)")
	signer := fs.String("signer", "", "name of a SIGNER EXTENSION to route this tool's signing to; the signer holds the key and the transaction parser, and the host holds neither (the passphrase that unlocks it comes from the vault: flynn auth set signer/<name>)")
	endpoint := fs.String("endpoint", "", "an http(s) endpoint the called tool may reach THROUGH THE HOST; the tool stays network-denied and only hands out request bytes, so it can never reach anywhere else")
	localEndpoint := fs.Bool("endpoint-local", false, "permit --endpoint to be a loopback or private literal address (a local test node); off by default, because a grant must not silently aim the host at its own network")
	if len(args) < 2 {
		return callRequest{}, errUsageCall
	}
	req := callRequest{name: args[0], tool: args[1], input: json.RawMessage(`{}`)}
	rest := args[2:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		if rest[0] != "" {
			req.input = json.RawMessage(rest[0])
		}
		rest = rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return callRequest{}, err
	}
	if fs.NArg() != 0 {
		return callRequest{}, errUsageCall
	}
	req.egress = splitList(*egress)
	req.signer = *signer
	req.endpoint = *endpoint
	req.localEndpoint = *localEndpoint
	return req, nil
}

// runtimeOptions builds the requested grants. The signer grant reads *routed when the
// worker mounts, so the caller may assign it after mounting the signer.
func (r callRequest) runtimeOptions(routed *extension.HostSigner) ([]governed.ExtensionOption, error) {
	opts := []governed.ExtensionOption{governed.WithExtensionEgress(r.egress)}
	if r.signer != "" {
		o, err := r.signerOption(routed)
		if err != nil {
			return nil, err
		}
		opts = append(opts, o)
	}
	if r.endpoint != "" {
		o, err := r.fetcherOption()
		if err != nil {
			return nil, err
		}
		opts = append(opts, o)
	}
	return opts, nil
}

// signerOption routes the called tool's signing to the signer extension.
//
// There is no policy to configure. The signer holds the key and the parser, so it
// judges the payload; a policy here would require this process to understand the
// transaction format, which routing the key away is meant to avoid.
func (r callRequest) signerOption(routed *extension.HostSigner) (governed.ExtensionOption, error) {
	if r.signer == r.name {
		return nil, errors.New("extensions: an extension cannot be its own signer: the component that builds " +
			"a transaction must not be the one that decides to sign it, or it is vouching for its own work")
	}
	return governed.WithExtensionSigner(func(ext, tool string) extension.HostSigner {
		if r.isCalledTool(ext, tool) {
			return *routed
		}
		return nil
	}), nil
}

// fetcherOption lends the host's network to the one endpoint, for the called tool only.
// Every other mounted tool gets nil and has no network.
func (r callRequest) fetcherOption() (governed.ExtensionOption, error) {
	var fopts []extension.FetcherOption
	if r.localEndpoint {
		fopts = append(fopts, extension.WithPrivateEndpoint())
	}
	fetcher, err := extension.NewHTTPHostFetcher(r.endpoint, fopts...)
	if err != nil {
		return nil, err
	}
	return governed.WithExtensionFetcher(func(ext, tool string) extension.HostFetcher {
		if r.isCalledTool(ext, tool) {
			return fetcher
		}
		return nil
	}), nil
}

// isCalledTool reports whether tool (bare) on ext is the one this request calls, named
// bare or as "<name>.<tool>".
func (r callRequest) isCalledTool(ext, tool string) bool {
	return ext == r.name && tool == strings.TrimPrefix(r.tool, r.name+".")
}

func invokeExtensionTool(ctx context.Context, rt *extensionRuntime, name, toolName string, input json.RawMessage) error {
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

// mountSigner launches a signer extension, unlocks it with the operator's passphrase
// and returns a HostSigner that routes to it.
//
// The passphrase comes only from the vault. An environment variable is ambient, and a
// flag lands in shell history and the process list.
//
// The host holds the passphrase and the signer's public key. It never sees the private
// key and never parses what it asks the signer to sign.
//
// The signer is reached over the host's private channel. Its tools are never mounted,
// so nothing model-facing can unlock the key or get a signature except through the
// worker.
func mountSigner(ctx context.Context, rt *extensionRuntime, dataDir, name string) (extension.HostSigner, error) {
	r, err := rt.store.Get(ctx, extension.Kind, resource.Scope{}, name)
	if errors.Is(err, resource.ErrNotFound) {
		return nil, fmt.Errorf("extensions: unknown signer extension %q (see flynn extensions ls)", name)
	}
	if err != nil {
		return nil, err
	}

	// The passphrase is fetched before launch. Without it the signer is inert, so this
	// fails before starting a subprocess and names what is missing.
	ref := "signer/" + name
	pass, err := vault.New(dataDir, vault.WithPassphrase(terminalPassphrase)).Lookup(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("extensions: no passphrase for signer %q in the vault (set it with: flynn auth set %s): %w", name, ref, err)
	}

	// A released signer's catalog spec cannot know where the operator keeps its key, so the
	// host names the path ([signerKeyPath]) and passes it at unlock. A missing file is
	// reported here with the path and remedy, before the signer's lower-level error.
	keyPath := signerKeyPath(dataDir, name)
	if _, statErr := os.Stat(keyPath); errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("extensions: signer %q has a passphrase but no sealed key at %s; seal one there first", name, keyPath)
	}

	// On Windows the signer runs in an AppContainer whose default-deny filesystem cannot
	// open the key without a grant. The key is encrypted at rest and its passphrase never
	// touches disk, so read goes to ALL APPLICATION PACKAGES (a no-op elsewhere); a
	// per-launch container SID would be dead by the next launch. It runs before launch so
	// the file is reachable at unlock.
	if err := rt.grantSignerKey(keyPath); err != nil {
		return nil, fmt.Errorf("extensions: grant the confined signer read on its sealed key: %w", err)
	}

	if _, err := rt.loader.Load(ctx, r); err != nil {
		return nil, err
	}
	ch, err := rt.procs.SignerChannelFor(r.ID)
	if err != nil {
		return nil, err
	}
	return extension.NewRoutedSigner(ctx, ch, pass, keyPath)
}

// signerKeyPath is a signer's sealed key file: one per signer under the data directory.
func signerKeyPath(dataDir, name string) string {
	return filepath.Join(dataDir, "signers", name+".key")
}

// extensionsRemove unlinks a dev extension and refuses bundled or forked ones, which
// are catalog state.
func extensionsRemove(ctx context.Context, dataDir string, args []string) error {
	if len(args) != 1 {
		return usageErr("extensions rm")
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

// buildDevSpec builds a dev link's spec: one process surface at absPath, plus optional
// capability tags, tool allow-list, egress and fixed args. absPath must be absolute, as
// the resolver requires.
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

// putDevExtension writes a dev extension under the dev source label, so catalog sync
// never touches it and listings show it unsigned. Re-linking keeps the observed status,
// such as a prior enable.
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

// deleteDevExtension refuses a non-dev extension, so the dev command cannot delete a
// catalog entry.
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

// resolveDevBinary returns the binary's absolute path, rejecting a missing file or a
// directory at link time.
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

// findExtensionTool matches a bare or namespaced tool name against one extension's
// mounts, which are always named "<ext>.<tool>".
func findExtensionTool(tools []mission.Tool, extName, toolName string) mission.Tool {
	full := extName + "." + toolName
	for _, t := range tools {
		if n := t.Def().Name; n == toolName || n == full {
			return t
		}
	}
	return nil
}

// validateExtensionName rejects names that would break namespacing or resource keys. A
// dot is reserved, because mounted tools are named "<ext>.<tool>".
func validateExtensionName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("extensions: name must not be empty")
	}
	if strings.ContainsAny(name, ". /\\") {
		return fmt.Errorf("extensions: name %q must not contain a dot, slash, or space", name)
	}
	return nil
}

// splitList parses a comma-separated flag into trimmed, non-empty items, or nil when
// empty, so an unset flag leaves its spec field empty.
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

// newFlagSet returns a flag set that hands parse errors back, so dispatch exits 2.
func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}

// extensionRuntime is the store plus a loader with the dev process handler, so these
// commands launch a linked binary as a served run would, without the release download
// and signature check.
type extensionRuntime struct {
	store  resource.Store
	loader *extension.Loader
	// procs reaches a signer over its private channel; its tools are never mounted.
	procs *extension.ProcessHandler
	// grantSignerKey makes a sealed key readable by the confined signer before launch. It
	// is a field so a test can drive the failure path.
	grantSignerKey func(keyPath string) error
	close          func() error
}

// openExtensionRuntime opens the store and the runtime through
// [governed.OpenExtensions], which owns resolution, launch and confinement. This type
// owns the store's lifetime.
func openExtensionRuntime(ctx context.Context, dataDir string, opts ...governed.ExtensionOption) (*extensionRuntime, error) {
	durable, err := openDataStore(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	reg, err := governed.Registry()
	if err != nil {
		_ = durable.Close()
		return nil, err
	}
	store := durable.Resources(reg)
	// `extensions dev` links the developer's own build, so dev sources are enabled; an
	// embedder of governed.OpenExtensions does not get them by default.
	exts, err := governed.OpenExtensions(ctx, store, dataDir, append([]governed.ExtensionOption{governed.WithDevExtensions()}, opts...)...)
	if err != nil {
		_ = durable.Close()
		return nil, err
	}
	return &extensionRuntime{
		store:          store,
		loader:         exts.Loader,
		procs:          exts.Process,
		grantSignerKey: sandbox.GrantSealedKeyReadable,
		close:          durable.Close,
	}, nil
}
