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

// Usage errors for the subcommands whose flags follow positional arguments, so the
// message is identical whether the positionals are missing or an extra one is given.
var (
	errUsageDev  = errors.New("usage: flynn extensions dev <name> <binary> [--cap ...] [--tool ...] [--egress ...] [--arg ...]")
	errUsageCall = errors.New("usage: flynn extensions call <name> <tool> [json-input] [--egress ...]")
)

// runExtensions implements `flynn extensions <subcommand>`: the surface for both kinds
// of extension. A bundled one is an official extension this binary ships knowledge of;
// calling it downloads the signed release, proves its origin, and runs it. A dev one is
// a locally-built binary linked by path, which keeps the authoring loop fast.
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

// extensionsCall launches an extension confined and invokes one of its tools, printing
// the result. It runs the real path a served agent would (resolve the binary, launch it
// inside the sandbox, speak MCP, mount its tools, call one), so a tool that works here
// works in a run. A bundled extension resolves through the signed release path: the
// artifact is downloaded, its origin proven, and only then executed. A dev extension
// resolves to its local binary, unsigned, under this command's explicit dev opt-in.
//
// The tool name may be given bare (mint) or namespaced (token.mint); both resolve to
// the mounted "<name>.<tool>". Authority to call a tool is enforced separately at the
// dispatch waist in a real run; this command invokes it directly to test the round trip.
func extensionsCall(ctx context.Context, dataDir string, args []string) error {
	req, err := parseCallArgs(args)
	if err != nil {
		return err
	}

	// routed is filled in once the signer extension is mounted, below. The runtime needs
	// the grant closure at open time, but only calls it when the worker mounts, which is
	// after the signer is up.
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

	// The signer comes up first and is unlocked before the worker is mounted: the worker's
	// mount asks for its signer, and a signer that will not unlock should stop the run
	// before the worker has done anything, not halfway through a transaction.
	if req.signer != "" {
		routed, err = mountSigner(ctx, rt, dataDir, req.signer)
		if err != nil {
			return err
		}
	}
	return invokeExtensionTool(ctx, rt, req.name, req.tool, req.input)
}

// callRequest is a parsed `flynn extensions call`.
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

// parseCallArgs splits the leading positionals (name, tool and an optional JSON input)
// from the flags that follow them, so `call <name> <tool> <json> --egress ...` parses;
// the flag package would otherwise stop at the first positional.
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

// runtimeOptions builds the runtime grants the request asks for. The signer grant reads
// *routed when the worker mounts, not now, so the caller assigns it after mounting the
// signer.
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
// No policy is configured, and none exists to configure. The signer holds the key AND
// the parser, so it judges the payload itself; a policy here would mean this process
// understood the transaction format, which is exactly what routing the key away removes.
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

// fetcherOption lends the called tool the host's network access to the one endpoint. The
// grant is for THIS tool only. Every other mounted tool gets nil, so it borrows no network
// at all: default-deny, one authority, one destination.
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

// isCalledTool reports whether a mount of tool (bare name) on extension ext is the tool
// this request calls. The request may name the tool bare or as "<name>.<tool>".
func (r callRequest) isCalledTool(ext, tool string) bool {
	return ext == r.name && tool == strings.TrimPrefix(r.tool, r.name+".")
}

// invokeExtensionTool loads the named extension, calls one of its mounted tools with
// input, prints the result and unloads the extension.
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

// mountSigner launches a signer extension, unlocks it with the operator's passphrase from the
// vault, and returns a HostSigner that routes to it.
//
// The passphrase comes from the vault and NOWHERE else. Not an environment variable, not a
// flag: a signing key's passphrase must not be settable by anything ambient, and a flag would
// put it in the shell history and the process list. The vault is the one place a secret lives.
//
// What the host ends up holding is the passphrase and the signer's PUBLIC key. It never sees
// the private key, and it never parses what it asks the signer to sign.
//
// The signer is reached over the host's private channel, not through its mounted tools: a
// signer's tools are never mounted at all, so nothing model-facing can unlock the key or ask
// for a signature without going through the worker.
func mountSigner(ctx context.Context, rt *extensionRuntime, dataDir, name string) (extension.HostSigner, error) {
	r, err := rt.store.Get(ctx, extension.Kind, resource.Scope{}, name)
	if errors.Is(err, resource.ErrNotFound) {
		return nil, fmt.Errorf("extensions: unknown signer extension %q (see flynn extensions ls)", name)
	}
	if err != nil {
		return nil, err
	}

	// The passphrase is fetched BEFORE the signer is launched. Without it the signer is inert
	// and the run is going to fail anyway, so failing here means no subprocess was started for
	// nothing, and the operator is told what is missing rather than watching a signer come up
	// and refuse everything.
	ref := "signer/" + name
	pass, err := vault.New(dataDir, vault.WithPassphrase(terminalPassphrase)).Lookup(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("extensions: no passphrase for signer %q in the vault (set it with: flynn auth set %s): %w", name, ref, err)
	}

	// The sealed key lives at a path this host names, because a released signer is launched from
	// a catalog spec that could not have known where the operator would put it. The convention is
	// one file per signer under the data directory; the operator seals a key to it, and the host
	// hands the path to the signer at unlock. A missing file is caught here, with the path and the
	// remedy, rather than surfacing as the signer's lower-level "cannot open" from inside a mount.
	keyPath := signerKeyPath(dataDir, name)
	if _, statErr := os.Stat(keyPath); errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("extensions: signer %q has a passphrase but no sealed key at %s; seal one there first", name, keyPath)
	}

	// The signer runs confined, and on Windows that is an AppContainer whose default-deny
	// filesystem cannot open the sealed key the host names for it unless the key is granted
	// to the container. The key is encrypted at rest and its passphrase never touches disk,
	// so this grants read to ALL APPLICATION PACKAGES (a no-op off Windows) rather than to a
	// per-launch container SID that would be a dead entry by the next launch. Done before the
	// launch so the file is reachable the moment the signer is unlocked.
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

// signerKeyPath is where a signer's sealed key lives: one file per signer under the data
// directory. The host names this path so a released signer, whose catalog spec could not have
// named a file on a machine that did not yet exist, is told where its key is at unlock.
func signerKeyPath(dataDir, name string) string {
	return filepath.Join(dataDir, "signers", name+".key")
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
	// procs is the process handler itself, kept so the host can reach a signer extension over
	// its private channel. A signer's tools are never mounted, so there is no other way to it.
	procs *extension.ProcessHandler
	// grantSignerKey makes a signer's sealed key reachable by its confined process before the
	// launch (on Windows, an AppContainer read grant; a no-op elsewhere). It is a field so a
	// test can drive the failure path without a platform that can make the grant fail.
	grantSignerKey func(keyPath string) error
	close          func() error
}

// openExtensionRuntime opens the durable store and builds the extension runtime over it
// with [governed.OpenExtensions], which owns how an extension is resolved, launched and
// confined. What stays here is the store's lifetime.
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
	// The binary is the developer's own tool: `extensions link` names a local build, so it
	// opts into dev sources, which an embedder of governed.OpenExtensions does not by default.
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
