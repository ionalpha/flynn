package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
)

func TestOnboardChoiceSpec(t *testing.T) {
	hosted := []string{"anthropic:opus", "openai:gpt"}
	const def = "openai:gpt"
	cases := []struct{ in, want string }{
		{"", def},                          // empty takes the default
		{"1", "anthropic:opus"},            // number selects from the list
		{"2", "openai:gpt"},                //
		{"3", ""},                          // out of range is a mistake, not a spec
		{"0", ""},                          //
		{"deepseek:chat", "deepseek:chat"}, // a typed spec passes through
		{"  openai:gpt  ", "openai:gpt"},   // trimmed
	}
	for _, c := range cases {
		if got := onboardChoiceSpec(c.in, hosted, def); got != c.want {
			t.Errorf("onboardChoiceSpec(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDefaultOnboardSpec(t *testing.T) {
	hosted := []string{"anthropic:opus", "openai:gpt"}
	if got := defaultOnboardSpec("openai:gpt-5.5", hosted); got != "openai:gpt-5.5" {
		t.Errorf("a keyed provider spec should be kept, got %q", got)
	}
	if got := defaultOnboardSpec("qwen2.5:0.5b", hosted); got != "anthropic:opus" {
		t.Errorf("a non-keyed spec should fall to the first hosted model, got %q", got)
	}
	if got := defaultOnboardSpec("qwen2.5:0.5b", nil); got != "qwen2.5:0.5b" {
		t.Errorf("with no hosted list the spec should be kept, got %q", got)
	}
}

func TestHostedCatalogModelsAreProviderSpecs(t *testing.T) {
	ids := hostedCatalogModels()
	if len(ids) == 0 {
		t.Fatal("onboarding offered no hosted models")
	}
	for _, id := range ids {
		if !strings.Contains(id, ":") {
			t.Fatalf("hosted id %q is not a provider:model spec", id)
		}
	}
}

// TestEnsureProviderKeyNoPromptWhenSatisfied checks the paths that must not prompt: a
// keyless or local spec needs nothing, and a hosted spec whose key is already stored is
// left alone. The interactive prompt itself (a hosted spec with no key) needs a terminal
// and is not exercised here.
func TestEnsureProviderKeyNoPromptWhenSatisfied(t *testing.T) {
	t.Setenv("FLYNN_VAULT_FILE", "1")
	t.Setenv("FLYNN_VAULT_PASSPHRASE", "pw")
	dir := t.TempDir()
	ctx := context.Background()

	for _, spec := range []string{"llamacpp:x", "qwen2.5:0.5b-instruct", "notaspec"} {
		if err := ensureProviderKey(ctx, spec, dir); err != nil {
			t.Errorf("ensureProviderKey(%q) = %v, want nil (nothing to key)", spec, err)
		}
	}

	ref, ok := provider.KeyRef("openai")
	if !ok {
		t.Fatal("openai should take a key")
	}
	store := vault.New(dir, vault.WithPassphrase(func(bool) (secret.Text, error) { return secret.New("pw"), nil }))
	if err := store.Set(ctx, ref, secret.New("sk-test")); err != nil {
		t.Fatal(err)
	}
	if err := ensureProviderKey(ctx, "openai:gpt-5.5", dir); err != nil {
		t.Errorf("ensureProviderKey with a stored key = %v, want nil (no prompt)", err)
	}
}
