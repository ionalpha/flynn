package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/extension/catalog"
	"github.com/ionalpha/flynn/resource"
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
	if _, err := rt2.store.Get(ctx, extension.Kind, resource.Scope{}, "token"); !errors.Is(err, resource.ErrNotFound) {
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
			if len(exts) != 0 {
				t.Fatalf("a refused link installed %d extension(s)", len(exts))
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
	keyPath := writeKeyFile(t, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))

	cases := map[string]struct {
		args []string
		want string
	}{
		"no arguments":       {[]string{}, "usage"},
		"no tool":            {[]string{"token"}, "usage"},
		"stray positional":   {[]string{"token", "mint", `{}`, "extra"}, "usage"},
		"unknown flag":       {[]string{"token", "mint", "--no-such-flag"}, "flag provided"},
		"unknown extension":  {[]string{"nosuchext", "mint"}, "unknown extension"},
		"unreadable key":     {[]string{"token", "mint", "--sign", filepath.Join(t.TempDir(), "absent.json")}, "read signing key"},
		"unknown policy":     {[]string{"token", "mint", "--sign", keyPath, "--sign-policy", "sign-anything-please"}, "unknown --sign-policy"},
		"private endpoint":   {[]string{"token", "mint", "--endpoint", "http://127.0.0.1:8899"}, ""},
		"malformed endpoint": {[]string{"token", "mint", "--endpoint", "not-a-url"}, ""},
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
	keyPath := writeKeyFile(t, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))

	args := []string{
		"token", "mint", `{"name":"t"}`,
		"--egress", "rpc.example",
		"--sign", keyPath,
		"--sign-policy", "solana-token",
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
	if cfg.egressGrant != nil || cfg.hostSigner != nil || cfg.signPolicy != nil || cfg.hostFetcher != nil {
		t.Fatal("the zero runtime must grant nothing")
	}
	signer, err := loadDevSigner(writeKeyFile(t, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))))
	if err != nil {
		t.Fatalf("load signer: %v", err)
	}
	policy, err := signPolicyFor("any", signer)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	fetcher, err := extension.NewHTTPHostFetcher("https://rpc.example")
	if err != nil {
		t.Fatalf("fetcher: %v", err)
	}
	for _, opt := range []extRuntimeOption{
		withEgressGrant([]string{"rpc.example"}),
		withHostSigner(func(string, string) extension.HostSigner { return signer }),
		withSignPolicy(func(string, string) extension.SignPolicy { return policy }),
		withHostFetcher(func(string, string) extension.HostFetcher { return fetcher }),
	} {
		opt(&cfg)
	}
	if len(cfg.egressGrant) != 1 || cfg.egressGrant[0] != "rpc.example" {
		t.Errorf("egress grant = %v, want the one granted", cfg.egressGrant)
	}
	if cfg.hostSigner == nil || cfg.hostSigner("token", "mint") == nil {
		t.Error("the signing key was not granted to the tool")
	}
	if cfg.signPolicy == nil || cfg.signPolicy("token", "mint") == nil {
		t.Error("a granted key must carry the policy it is bound by")
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
