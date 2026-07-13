package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
)

// fileVaultEnv points the vault at its sealed-file backend with a passphrase from the
// environment, so a test never touches the developer's OS keychain and never needs a
// terminal to unlock the vault.
func fileVaultEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("FLYNN_VAULT_FILE", "1")
	t.Setenv("FLYNN_VAULT_PASSPHRASE", "test-passphrase")
	return t.TempDir()
}

// TestAuthSetStoresTheKey: a key stored once is used automatically, so nothing need be
// exported for a later run.
func TestAuthSetStoresTheKey(t *testing.T) {
	ctx := context.Background()
	store := appTestVault(t)

	if err := authSet(ctx, store, []string{"anthropic"}, fixedPrompt("sk-ant-test")); err != nil {
		t.Fatalf("auth set: %v", err)
	}
	ref, ok := provider.KeyRef("anthropic")
	if !ok {
		t.Fatal("anthropic must have a key reference")
	}
	got, err := store.Lookup(ctx, ref)
	if err != nil {
		t.Fatalf("the key was not stored: %v", err)
	}
	if got.Expose() != "sk-ant-test" {
		t.Fatalf("stored key = %q, want the one that was entered", got.Expose())
	}
}

// TestAuthSetRefusesBadInput: a keyless provider is told apart from a typo, and nothing
// is stored when the prompt yields nothing.
func TestAuthSetRefusesBadInput(t *testing.T) {
	cases := map[string]struct {
		args   []string
		prompt secretPrompt
		want   string
	}{
		"no provider":       {nil, fixedPrompt("k"), "usage"},
		"two providers":     {[]string{"anthropic", "openai"}, fixedPrompt("k"), "usage"},
		"unknown provider":  {[]string{"nosuchprovider"}, fixedPrompt("k"), "unknown provider"},
		"keyless provider":  {[]string{"llamacpp"}, fixedPrompt("k"), "needs no API key"},
		"no key entered":    {[]string{"anthropic"}, fixedPrompt(""), "no key entered"},
		"prompt unreadable": {[]string{"anthropic"}, failingPrompt(errors.New("no terminal")), "no terminal"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := appTestVault(t)
			err := authSet(ctx, store, tc.args, tc.prompt)
			if err == nil {
				t.Fatal("expected the key to be refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
			if ref, ok := provider.KeyRef("anthropic"); ok {
				if _, err := store.Lookup(ctx, ref); err == nil {
					t.Error("a refused set stored a key anyway")
				}
			}
		})
	}
}

// TestAuthRemoveClearsTheKey: rm is how a leaked key is retired, so afterwards the vault
// must not answer with it.
func TestAuthRemoveClearsTheKey(t *testing.T) {
	ctx := context.Background()
	store := appTestVault(t)
	ref, _ := provider.KeyRef("openai")

	if err := store.Set(ctx, ref, secret.New("sk-old")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := authRemove(ctx, store, []string{"openai"}); err != nil {
		t.Fatalf("auth rm: %v", err)
	}
	if _, err := store.Lookup(ctx, ref); err == nil {
		t.Fatal("the key survived `auth rm`")
	}
	// Removing a key that was never stored is not an error: the end state is the one asked
	// for either way.
	if err := authRemove(ctx, store, []string{"openai"}); err != nil {
		t.Fatalf("removing an absent key must not fail: %v", err)
	}
}

// TestAuthRemoveRefusesBadInput: a keyless provider has no key to remove, and a typo is
// not silently treated as one.
func TestAuthRemoveRefusesBadInput(t *testing.T) {
	cases := map[string][]string{
		"no provider":      nil,
		"two providers":    {"anthropic", "openai"},
		"unknown provider": {"nosuchprovider"},
		"keyless provider": {"llamacpp"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if err := authRemove(context.Background(), appTestVault(t), args); err == nil {
				t.Fatal("expected the removal to be refused")
			}
		})
	}
}

// TestAuthListReportsEveryProviderAndTheApp: the listing is the answer to "what is
// configured", so it must cover every provider, mark a keyless one as needing nothing,
// and report the App identity, which authenticates every review.
func TestAuthListReportsEveryProviderAndTheApp(t *testing.T) {
	ctx := context.Background()
	store := appTestVault(t)
	ref, _ := provider.KeyRef("anthropic")
	if err := store.Set(ctx, ref, secret.New("sk-ant")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var buf bytes.Buffer
	if err := authList(ctx, store, &buf); err != nil {
		t.Fatalf("auth list: %v", err)
	}
	out := buf.String()
	for _, name := range provider.Providers() {
		if !strings.Contains(out, name) {
			t.Errorf("the listing does not mention the %s provider:\n%s", name, out)
		}
	}
	if !strings.Contains(lineContaining(out, "anthropic"), "stored") {
		t.Errorf("a stored key is not reported as stored:\n%s", out)
	}
	if !strings.Contains(lineContaining(out, "openai"), "not set") {
		t.Errorf("a provider with no key is not reported as unset:\n%s", out)
	}
	if !strings.Contains(lineContaining(out, "llamacpp"), "no key required") {
		t.Errorf("a keyless provider must say so rather than reading as misconfigured:\n%s", out)
	}
	if !strings.Contains(lineContaining(out, "github app"), "not set") {
		t.Errorf("the App identity is missing from the listing:\n%s", out)
	}
}

// TestIsKnownProvider tells a provider that needs no key apart from a typo, which is what
// lets `auth set` explain the difference.
func TestIsKnownProvider(t *testing.T) {
	for _, name := range provider.Providers() {
		if !isKnownProvider(name) {
			t.Errorf("%q is a provider but was not recognised", name)
		}
	}
	if isKnownProvider("nosuchprovider") {
		t.Error("an unknown name must not be recognised as a provider")
	}
}

// TestRunAuthDispatch: the verb table routes every subcommand, and one it does not know
// is refused by name. The verbs that only read (ls, list) run to completion; the ones
// that need a value are exercised through their own cores above.
func TestRunAuthDispatch(t *testing.T) {
	dataDir := fileVaultEnv(t)

	if err := runAuth(nil, dataDir); err == nil {
		t.Fatal("expected bare `auth` to print its usage as an error")
	}
	err := runAuth([]string{"nosuchverb"}, dataDir)
	if err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("error = %v, want an unknown-subcommand refusal", err)
	}

	// The listing verbs read the vault and the credential store, and succeed over empty
	// ones: nothing configured is not a failure.
	for _, args := range [][]string{{"list"}, {"ls"}, {"ls", "cloudflare"}} {
		if err := runAuth(args, dataDir); err != nil {
			t.Fatalf("`auth %s`: %v", strings.Join(args, " "), err)
		}
	}

	// rm-app clears an identity that was never stored, which is how an interrupted set-app
	// is recovered from.
	if err := runAuth([]string{"rm-app"}, dataDir); err != nil {
		t.Fatalf("`auth rm-app`: %v", err)
	}

	// The routing of rm: a bare provider name removes a model key, an <integration>/<name>
	// reference removes a credential. Both are reached, and each reports its own error.
	if err := runAuth([]string{"rm", "nosuchprovider"}, dataDir); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("a bare name must route to the provider key removal, got %v", err)
	}
	if err := runAuth([]string{"rm", "cf/nosuchname"}, dataDir); err == nil || strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("a slashed reference must route to the credential removal, got %v", err)
	}

	// use and set report their own usage rather than the group's.
	if err := runAuth([]string{"use"}, dataDir); err == nil || !strings.Contains(err.Error(), "auth use") {
		t.Fatalf("error = %v, want the `auth use` usage", err)
	}
	if err := runAuth([]string{"set"}, dataDir); err == nil || !strings.Contains(err.Error(), "auth set") {
		t.Fatalf("error = %v, want the `auth set` usage", err)
	}
	if err := runAuth([]string{"add"}, dataDir); err == nil || !strings.Contains(err.Error(), "auth add") {
		t.Fatalf("error = %v, want the `auth add` usage", err)
	}
}

// TestPromptHiddenNeedsATerminal: a secret piped in would be captured in a process
// listing or a script, so it is refused rather than read.
func TestPromptHiddenNeedsATerminal(t *testing.T) {
	// The test process's stdin is not a terminal, which is the condition under test.
	if _, err := promptHidden("Enter API key: "); err == nil {
		t.Fatal("a secret must not be readable without a terminal")
	}
}

// TestTerminalPassphraseFallsBackToTheEnvironment: with no terminal to prompt on, the
// sealed-file vault's passphrase comes from the environment, so an unattended run can
// still open its vault.
func TestTerminalPassphraseFallsBackToTheEnvironment(t *testing.T) {
	t.Setenv("FLYNN_VAULT_PASSPHRASE", "from-the-environment")
	got, err := terminalPassphrase(false)
	if err != nil {
		t.Fatalf("terminalPassphrase: %v", err)
	}
	if got.Expose() != "from-the-environment" {
		t.Fatalf("passphrase = %q, want the environment's", got.Expose())
	}
}
