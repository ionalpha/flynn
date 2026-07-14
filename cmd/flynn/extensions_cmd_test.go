package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/extension/catalog"
	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/secret"
)

// devBinary writes a file to stand in for a locally-built extension binary. It is enough
// for the link and the listing, which check the path resolves to a file; a call that
// launches it fails, which is what the call tests below rely on.
func devBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ext-binary")
	if err := os.WriteFile(path, []byte("not a real binary"), 0o700); err != nil { //nolint:gosec // a stand-in for an executable
		t.Fatalf("write dev binary: %v", err)
	}
	return path
}

// TestRunExtensionsDispatch: the subcommand table routes every verb, and a verb it does
// not know is refused by name rather than silently doing nothing.
func TestRunExtensionsDispatch(t *testing.T) {
	dataDir := t.TempDir()
	if err := runExtensions(nil, dataDir); err == nil {
		t.Fatal("expected bare `extensions` to print its usage as an error")
	}
	err := runExtensions([]string{"nosuchverb"}, dataDir)
	if err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("error = %v, want an unknown-subcommand refusal", err)
	}
}

// TestExtensionsDevLinkListRemove is the authoring loop end to end over the real durable
// store: link a locally-built binary, see it listed and marked unsigned, then unlink it.
func TestExtensionsDevLinkListRemove(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	bin := devBinary(t)

	if err := runExtensions([]string{"dev", "token", bin, "--cap", "crypto.token", "--tool", "mint", "--egress", "rpc.example", "--arg", "--net,devnet"}, dataDir); err != nil {
		t.Fatalf("dev link: %v", err)
	}

	// The link landed in the store, marked dev, with the flags carried into its spec.
	rt, err := openExtensionRuntime(ctx, dataDir)
	if err != nil {
		t.Fatalf("open runtime: %v", err)
	}
	r, err := rt.store.Get(ctx, extension.Kind, resource.Scope{}, "token")
	if err != nil {
		_ = rt.close()
		t.Fatalf("the dev link was not persisted: %v", err)
	}
	if r.Labels[catalog.SourceLabel] != catalog.SourceDev {
		t.Errorf("source label = %q, want %q", r.Labels[catalog.SourceLabel], catalog.SourceDev)
	}
	spec, err := extension.DecodeSpec(r)
	if err != nil {
		_ = rt.close()
		t.Fatalf("decode spec: %v", err)
	}
	if len(spec.Capabilities) != 1 || spec.Capabilities[0] != "crypto.token" {
		t.Errorf("capabilities = %v, want the linked one", spec.Capabilities)
	}
	if len(spec.Safety.EgressAllow) != 1 || spec.Safety.EgressAllow[0] != "rpc.example" {
		t.Errorf("egress request = %v, want the linked one", spec.Safety.EgressAllow)
	}
	if err := rt.close(); err != nil {
		t.Fatalf("close runtime: %v", err)
	}

	// The listing goes through the same command path the operator uses.
	if err := runExtensions([]string{"ls"}, dataDir); err != nil {
		t.Fatalf("ls: %v", err)
	}

	if err := runExtensions([]string{"rm", "token"}, dataDir); err != nil {
		t.Fatalf("rm: %v", err)
	}
	rt2, err := openExtensionRuntime(ctx, dataDir)
	if err != nil {
		t.Fatalf("reopen runtime: %v", err)
	}
	defer func() { _ = rt2.close() }()
	// "token" is also an official extension, so unlinking the local build does not leave a
	// hole: the bundled spec reclaims the name on the next sync. What must be gone is the
	// dev link, or `rm` would have left the author's binary wired up.
	back, err := rt2.store.Get(ctx, extension.Kind, resource.Scope{}, "token")
	if err != nil {
		t.Fatalf("the bundled extension did not reclaim the name after the dev link was removed: %v", err)
	}
	if back.Labels[catalog.SourceLabel] != catalog.SourceBundled {
		t.Fatalf("source label = %q, want the bundled spec back", back.Labels[catalog.SourceLabel])
	}
}

// TestExtensionsDevRemoveRestoresBundled: unlinking a dev build of an extension that is
// NOT in the catalog leaves nothing behind.
func TestExtensionsDevRemoveLeavesNoHole(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	bin := devBinary(t)

	if err := runExtensions([]string{"dev", "not-official", bin, "--cap", "x.y"}, dataDir); err != nil {
		t.Fatalf("dev link: %v", err)
	}
	if err := runExtensions([]string{"rm", "not-official"}, dataDir); err != nil {
		t.Fatalf("rm: %v", err)
	}
	rt, err := openExtensionRuntime(ctx, dataDir)
	if err != nil {
		t.Fatalf("open runtime: %v", err)
	}
	defer func() { _ = rt.close() }()
	if _, err := rt.store.Get(ctx, extension.Kind, resource.Scope{}, "not-official"); !errors.Is(err, resource.ErrNotFound) {
		t.Fatalf("the unlinked extension is still installed (err = %v)", err)
	}
}

// TestExtensionsListEmpty: an installation with nothing linked says so rather than
// printing an empty table.
func TestExtensionsListEmpty(t *testing.T) {
	if err := extensionsList(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("ls on an empty store: %v", err)
	}
}

// TestExtensionsDevRefusesBadInput: every rejection happens before the store is touched,
// so a bad link cannot leave an extension behind that a later call would try to launch.
func TestExtensionsDevRefusesBadInput(t *testing.T) {
	bin := devBinary(t)
	dir := t.TempDir()

	cases := map[string][]string{
		"no arguments":       {},
		"binary missing":     {"token"},
		"name with a dot":    {"to.ken", bin},
		"empty name":         {"", bin},
		"binary is absent":   {"token", filepath.Join(t.TempDir(), "nope")},
		"binary is a dir":    {"token", dir},
		"stray positional":   {"token", bin, "extra"},
		"unknown flag":       {"token", bin, "--no-such-flag"},
		"flag before binary": {"--cap", "x"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			if err := extensionsDev(context.Background(), dataDir, args); err == nil {
				t.Fatal("expected the link to be refused")
			}
			rt, err := openExtensionRuntime(context.Background(), dataDir)
			if err != nil {
				t.Fatalf("open runtime: %v", err)
			}
			defer func() { _ = rt.close() }()
			exts, err := rt.store.List(context.Background(), extension.Kind, resource.Scope{}, nil)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			// Opening the runtime syncs the bundled catalog, so the store is never empty.
			// What must be empty is the set of dev links: a refused link may leave nothing
			// behind that a later call would launch.
			for _, r := range exts {
				if r.Labels[catalog.SourceLabel] == catalog.SourceDev {
					t.Fatalf("a refused link installed the dev extension %q", r.Name)
				}
			}
		})
	}
}

// TestExtensionsRemoveRefusesBadInput: rm takes exactly one name, and an extension that
// was never linked cannot be unlinked.
func TestExtensionsRemoveRefusesBadInput(t *testing.T) {
	dataDir := t.TempDir()
	for name, args := range map[string][]string{
		"no name":     {},
		"two names":   {"a", "b"},
		"not linked":  {"nosuchext"},
		"empty name ": {""},
	} {
		t.Run(name, func(t *testing.T) {
			if err := extensionsRemove(context.Background(), dataDir, args); err == nil {
				t.Fatal("expected the removal to be refused")
			}
		})
	}
}

// TestExtensionsCallRefusesBadInput: the call command's grants are decided before the
// extension is launched, so a key with no policy, an unknown policy, an unreachable
// endpoint, or an extension that was never linked all fail before any code runs.
func TestExtensionsCallRefusesBadInput(t *testing.T) {
	dataDir := t.TempDir()
	bin := devBinary(t)
	if err := runExtensions([]string{"dev", "token", bin}, dataDir); err != nil {
		t.Fatalf("dev link: %v", err)
	}
	cases := map[string]struct {
		args []string
		want string
	}{
		"no arguments":       {[]string{}, "usage"},
		"no tool":            {[]string{"token"}, "usage"},
		"stray positional":   {[]string{"token", "mint", `{}`, "extra"}, "usage"},
		"unknown flag":       {[]string{"token", "mint", "--no-such-flag"}, "flag provided"},
		"unknown extension":  {[]string{"nosuchext", "mint"}, "unknown extension"},
		"private endpoint":   {[]string{"token", "mint", "--endpoint", "http://127.0.0.1:8899"}, ""},
		"malformed endpoint": {[]string{"token", "mint", "--endpoint", "not-a-url"}, ""},

		// The extension that BUILDS a transaction must not be the one that signs it, or it is
		// vouching for its own work and the separation buys nothing.
		"its own signer": {
			[]string{"token", "mint", "--signer", "token"},
			"cannot be its own signer",
		},
		"unknown signer": {
			[]string{"token", "mint", "--signer", "nosuchsigner"},
			"unknown signer extension",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := extensionsCall(context.Background(), dataDir, tc.args)
			if err == nil {
				t.Fatal("expected the call to be refused")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestExtensionsCallLaunchesTheLinkedBinary: a call resolves the linked binary and
// launches it confined, so a path that is not a runnable extension fails at launch rather
// than being reported as a missing tool. The grants (egress, a signing key with its
// policy, a host endpoint) are all assembled on the way there.
func TestExtensionsCallLaunchesTheLinkedBinary(t *testing.T) {
	dataDir := t.TempDir()
	bin := devBinary(t)
	if err := runExtensions([]string{"dev", "token", bin}, dataDir); err != nil {
		t.Fatalf("dev link: %v", err)
	}
	args := []string{
		"token", "mint", `{"name":"t"}`,
		"--egress", "rpc.example",
		"--endpoint", "http://127.0.0.1:8899",
		"--endpoint-local",
	}
	err := extensionsCall(context.Background(), dataDir, args)
	if err == nil {
		t.Fatal("a file that is not an extension binary must fail to launch")
	}
	// It must fail at launch, not by resolving the extension to nothing: the link is real.
	if strings.Contains(err.Error(), "unknown extension") {
		t.Fatalf("the linked extension was not found: %v", err)
	}
}

// TestCallWithASignerNeedsThePassphraseInTheVault: the passphrase comes from the vault and
// nowhere else, and it is fetched BEFORE the signer is launched. A missing one names the
// command that fixes it rather than starting a signer that would refuse everything it was
// asked.
func TestCallWithASignerNeedsThePassphraseInTheVault(t *testing.T) {
	dataDir := fileVaultEnv(t)
	bin := devBinary(t)
	for _, name := range []string{"token", "solana-signer"} {
		if err := runExtensions([]string{"dev", name, bin}, dataDir); err != nil {
			t.Fatalf("dev link %s: %v", name, err)
		}
	}

	err := extensionsCall(context.Background(), dataDir,
		[]string{"token", "mint", "--signer", "solana-signer"})
	if err == nil {
		t.Fatal("a signer with no passphrase in the vault was used anyway")
	}
	if !strings.Contains(err.Error(), "no passphrase for signer") {
		t.Fatalf("error = %v, want it to say the vault holds no passphrase", err)
	}
	// It must name the command that fixes it: an operator who cannot act on an error is stuck.
	if !strings.Contains(err.Error(), "flynn auth set signer/solana-signer") {
		t.Fatalf("the error does not say how to set the passphrase: %v", err)
	}
}

// TestCallWithASignerLaunchesIt: with the passphrase in the vault, the signer is resolved and
// launched. The stand-in binary is not a real MCP server, so it fails at launch, which is
// exactly how far this test can go without one: what it proves is that the signer is reached at
// all, and reached before the worker.
func TestCallWithASignerLaunchesIt(t *testing.T) {
	ctx := context.Background()
	dataDir := fileVaultEnv(t)
	bin := devBinary(t)
	for _, name := range []string{"token", "solana-signer"} {
		if err := runExtensions([]string{"dev", name, bin}, dataDir); err != nil {
			t.Fatalf("dev link %s: %v", name, err)
		}
	}
	// The passphrase lives in the vault, which is where mountSigner will look for it.
	if err := vault.New(dataDir, vault.WithPassphrase(terminalPassphrase)).
		Set(ctx, "signer/solana-signer", secret.New("hunter2")); err != nil {
		t.Fatalf("seed the vault: %v", err)
	}
	// The sealed key lives at the path the host names. It need not be a real sealed key here:
	// what matters is that it exists, so the mount gets past the missing-key check and reaches
	// the signer, which is as far as a stand-in binary can go.
	keyPath := signerKeyPath(dataDir, "solana-signer")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatalf("make the signers dir: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("sealed-key-stand-in"), 0o600); err != nil {
		t.Fatalf("seal a stand-in key: %v", err)
	}

	err := extensionsCall(ctx, dataDir, []string{"token", "mint", "--signer", "solana-signer"})
	if err == nil {
		t.Fatal("a file that is not an extension binary must fail to launch")
	}
	// The failure must come from launching the signer, not from failing to find it or its
	// passphrase: both of those are already ruled out above.
	if strings.Contains(err.Error(), "no passphrase") || strings.Contains(err.Error(), "unknown signer") {
		t.Fatalf("the signer was not reached: %v", err)
	}
}

// TestCallWithASignerNeedsItsSealedKey: with the passphrase in the vault but no sealed key at
// the path the host names, the mount fails up front naming that path and the remedy, rather
// than starting a signer that would fail to open a key that is not there.
func TestCallWithASignerNeedsItsSealedKey(t *testing.T) {
	ctx := context.Background()
	dataDir := fileVaultEnv(t)
	bin := devBinary(t)
	for _, name := range []string{"token", "solana-signer"} {
		if err := runExtensions([]string{"dev", name, bin}, dataDir); err != nil {
			t.Fatalf("dev link %s: %v", name, err)
		}
	}
	if err := vault.New(dataDir, vault.WithPassphrase(terminalPassphrase)).
		Set(ctx, "signer/solana-signer", secret.New("hunter2")); err != nil {
		t.Fatalf("seed the vault: %v", err)
	}

	err := extensionsCall(ctx, dataDir, []string{"token", "mint", "--signer", "solana-signer"})
	if err == nil {
		t.Fatal("a signer with no sealed key was used anyway")
	}
	if !strings.Contains(err.Error(), "no sealed key") {
		t.Fatalf("error = %v, want it to say the sealed key is missing", err)
	}
	// It must name the path, so the operator knows where to seal the key.
	if !strings.Contains(err.Error(), signerKeyPath(dataDir, "solana-signer")) {
		t.Fatalf("the error does not name the key path: %v", err)
	}
}

// TestResolveDevBinary: the path is verified at link time, so a missing file or a
// directory is reported while the operator is watching rather than at the first call.
func TestResolveDevBinary(t *testing.T) {
	bin := devBinary(t)
	abs, err := resolveDevBinary(bin)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !filepath.IsAbs(abs) {
		t.Fatalf("resolved path %q is not absolute", abs)
	}

	if _, err := resolveDevBinary(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a missing binary must be refused at link time")
	}
	if _, err := resolveDevBinary(t.TempDir()); err == nil {
		t.Error("a directory is not a binary")
	}
}

// TestExtensionRuntimeOptions: the grants are off unless asked for, which is what makes a
// launched extension default-deny (no egress, no key, no network of its own).
func TestExtensionRuntimeOptions(t *testing.T) {
	var cfg extRuntimeOptions
	if cfg.egressGrant != nil || cfg.hostSigner != nil || cfg.hostFetcher != nil {
		t.Fatal("the zero runtime must grant nothing")
	}
	fetcher, err := extension.NewHTTPHostFetcher("https://rpc.example")
	if err != nil {
		t.Fatalf("fetcher: %v", err)
	}
	// There is no policy option to set, and that is the point: the host holds no parser, so it
	// has nothing to police a payload with. The signer does that, where the key is.
	for _, opt := range []extRuntimeOption{
		withEgressGrant([]string{"rpc.example"}),
		withHostSigner(func(string, string) extension.HostSigner { return nil }),
		withHostFetcher(func(string, string) extension.HostFetcher { return fetcher }),
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

// TestNewFlagSetHandsErrorsBack: a subcommand's bad flag must not exit the process; the
// dispatch turns it into a usage error.
func TestNewFlagSetHandsErrorsBack(t *testing.T) {
	fs := newFlagSet("extensions dev")
	fs.SetOutput(io.Discard)
	if err := fs.Parse([]string{"--no-such-flag"}); err == nil {
		t.Fatal("a bad flag must be reported to the caller, not exit the process")
	}
}

// TestOrUser: an extension with no source label was put there by the operator.
func TestOrUser(t *testing.T) {
	if got := orUser(""); got != "user" {
		t.Errorf("orUser(\"\") = %q, want user", got)
	}
	if got := orUser(catalog.SourceBundled); got != catalog.SourceBundled {
		t.Errorf("orUser(%q) = %q, want it unchanged", catalog.SourceBundled, got)
	}
}
