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
	"github.com/ionalpha/flynn/extension/signpolicy"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/vault"
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
	signPolicy := fs.String("sign-policy", "solana-token", `what the signing key may be used for: "solana-token" (only a mint that revokes both its authorities and moves no SOL) or "any" (sign whatever the tool asks: development only)`)
	sign := fs.String("sign", "", "path to a dev signing key (a JSON array of the 64 raw key bytes) the called tool signs its work with; the key stays in the host and the tool only receives signatures")
	signer := fs.String("signer", "", "name of a SIGNER EXTENSION to route this tool's signing to; the signer holds the key and the transaction parser, and the host holds neither (the passphrase that unlocks it comes from the vault: flynn auth set signer/<name>)")
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

	if *sign != "" && *signer != "" {
		return errors.New("extensions: --sign and --signer are two different custody models; pick one. " +
			"--signer keeps the key out of this process entirely, and is the one you want")
	}

	// routed is filled in once the signer extension is mounted, below. The grant closure is
	// built here because the runtime needs it at open time, but it is only ever CALLED when the
	// worker mounts, which happens after the signer is up.
	var routed extension.HostSigner
	if *signer != "" {
		if *signer == name {
			return errors.New("extensions: an extension cannot be its own signer: the component that builds " +
				"a transaction must not be the one that decides to sign it, or it is vouching for its own work")
		}
		runtimeOpts = append(runtimeOpts, withHostSigner(func(ext, tool string) extension.HostSigner {
			if ext == name && tool == bareTool {
				return routed
			}
			return nil
		}))
		// NO withSignPolicy. The signer extension holds the key AND the parser, so it judges the
		// payload itself (extension.SelfPolicing). A policy here would mean this process
		// understood the transaction format, which is exactly what routing the key away removes.
	}

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
		// A key is granted with a policy or not at all: the host refuses to sign for a tool
		// that has no policy, so the two are decided together, here, rather than one of them
		// being forgotten somewhere else.
		//
		// The policy is chosen by what the key is FOR. A signing key granted to a Solana
		// token tool may be used to mint a token that is safe by the same rules the extension
		// claims to follow, and for nothing else: not to move SOL, not to transfer or burn
		// what it minted, and not to mint anything whose authorities it does not revoke in the
		// same transaction. The extension proposes and the host disposes, so an extension that
		// has been compromised outright still cannot obtain a signature over a token it could
		// later inflate, freeze, or steal.
		policy, err := signPolicyFor(*signPolicy, signer)
		if err != nil {
			return err
		}
		runtimeOpts = append(runtimeOpts, withSignPolicy(func(ext, tool string) extension.SignPolicy {
			if ext == name && tool == bareTool {
				return policy
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

	// The signer comes up FIRST, and is unlocked before the worker is mounted at all. The
	// worker's mount is what asks for its signer, so by then there has to be one; and a signer
	// that will not unlock should stop the run before the worker has done anything, not halfway
	// through a transaction it can no longer finish.
	if *signer != "" {
		routed, err = mountSigner(ctx, rt, dataDir, *signer)
		if err != nil {
			return err
		}
	}

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
func mountSigner(ctx context.Context, rt *extensionRuntime, dataDir, name string) (extension.HostSigner, error) {
	r, err := rt.store.Get(ctx, extension.Kind, resource.Scope{}, name)
	if errors.Is(err, resource.ErrNotFound) {
		return nil, fmt.Errorf("extensions: unknown signer extension %q (see flynn extensions ls)", name)
	}
	if err != nil {
		return nil, err
	}
	if _, err := rt.loader.Load(ctx, r); err != nil {
		return nil, err
	}

	ref := "signer/" + name
	pass, err := vault.New(dataDir, vault.WithPassphrase(terminalPassphrase)).Lookup(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("extensions: no passphrase for signer %q in the vault (set it with: flynn auth set %s): %w", name, ref, err)
	}

	tools := rt.loader.Tools()
	unlock := findExtensionTool(tools, name, extension.SignerUnlockTool)
	sign := findExtensionTool(tools, name, extension.SignerSignTool)
	if unlock == nil || sign == nil {
		return nil, fmt.Errorf("extensions: %q is not a signer extension: it advertises no %s and %s",
			name, extension.SignerUnlockTool, extension.SignerSignTool)
	}
	return extension.NewRoutedSigner(ctx, unlock, sign, pass)
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
	signPolicy  func(ext, tool string) extension.SignPolicy
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

// withSignPolicy bounds what a granted key may be asked to sign. A tool with a key but no
// policy signs nothing, so this is not optional hardening: it is the other half of the grant.
func withSignPolicy(fn func(ext, tool string) extension.SignPolicy) extRuntimeOption {
	return func(o *extRuntimeOptions) { o.signPolicy = fn }
}

// signPolicies is every policy this binary is willing to bind a key to, by name.
//
// "solana-token" is the real one: it reads the transaction the extension asks to have signed
// and approves it only if it is a mint that revokes both its authorities, touching only the
// programs a mint touches, paying no SOL to anyone. "any" signs whatever it is handed, which
// is what a developer driving an unreleased extension against a throwaway key needs and what
// nothing else should ever want. It has to be named to be reached, so nobody arrives at blind
// signing by forgetting to think about it.
//
// A second format is a second entry here and a second file in extension/signpolicy. The host
// package stays free of every one of them: it holds the port, not the knowledge.
var signPolicies = map[string]func(extension.HostSigner) extension.SignPolicy{
	"solana-token": func(s extension.HostSigner) extension.SignPolicy {
		return signpolicy.Solana{Payer: s.Public()}
	},
	"any": func(extension.HostSigner) extension.SignPolicy { return extension.AnyPayload{} },
}

// signPolicyFor turns the --sign-policy choice into the policy the granted key is bound by.
//
// A name this binary does not know is an error, not a default. The alternative is a key that
// gets bound to whichever policy the fallback happened to name, which is the wrong policy by
// definition: the operator asked for one that does not exist. Silently substituting another
// would be a key granted for a purpose nobody chose, and a typo would be enough to do it.
func signPolicyFor(choice string, signer extension.HostSigner) (extension.SignPolicy, error) {
	build, ok := signPolicies[choice]
	if !ok {
		known := make([]string, 0, len(signPolicies))
		for name := range signPolicies {
			known = append(known, name)
		}
		sort.Strings(known)
		return nil, fmt.Errorf("unknown --sign-policy %q: known policies are %s", choice, strings.Join(known, ", "))
	}
	return build(signer), nil
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
	if cfg.signPolicy != nil {
		procOpts = append(procOpts, extension.WithSignPolicy(cfg.signPolicy))
	}
	if cfg.hostFetcher != nil {
		procOpts = append(procOpts, extension.WithHostFetcher(cfg.hostFetcher))
	}
	// A published extension resolves through signature verification against the pinned
	// first-party origin; a dev-linked one resolves only under the explicit dev opt-in.
	// A spec that declares a release is never satisfied by a local binary, so a stray dev
	// block cannot downgrade a signed extension to an unsigned one.
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
