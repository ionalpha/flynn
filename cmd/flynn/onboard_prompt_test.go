package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/provider"
)

// noProviderKeys empties the credential source a test resolves against: every provider
// key is cleared out of the environment, and the vault is pinned to its sealed file so
// the lookup never reaches the developer's OS keychain. With the file under a fresh temp
// data directory, the source starts with nothing in it.
func noProviderKeys(t *testing.T) {
	t.Helper()
	for _, k := range provider.CredentialEnvVars() {
		t.Setenv(k, "")
	}
	t.Setenv("FLYNN_VAULT_FILE", "1")
	t.Setenv("FLYNN_VAULT_PASSPHRASE", "test-passphrase")
}

// TestPromptVisibleReadsAnswer proves the visible prompt writes its label and returns
// the answer trimmed, and that a stream that ends with no answer at all is an error
// rather than a silent empty choice.
func TestPromptVisibleReadsAnswer(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"answer", "openai\n", "openai", false},
		{"trimmed", "  gemini \r\n", "gemini", false},
		{"empty line takes the default", "\n", "", false},
		{"unterminated last line still answers", "deepseek", "deepseek", false},
		{"no input at all", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := promptVisible(bufio.NewReader(strings.NewReader(c.input)), &out, "Provider: ")
			if c.wantErr {
				if err == nil {
					t.Fatalf("promptVisible(%q) = %q, want an error", c.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("promptVisible(%q): %v", c.input, err)
			}
			if got != c.want {
				t.Fatalf("promptVisible(%q) = %q, want %q", c.input, got, c.want)
			}
			if !strings.Contains(out.String(), "Provider: ") {
				t.Fatalf("label not written to out: %q", out.String())
			}
		})
	}
}

// TestKeyedProviders proves the credential prompt offers exactly the providers that
// take an API key, in canonical order, so a keyless provider is never asked for one.
func TestKeyedProviders(t *testing.T) {
	got := keyedProviders()
	if len(got) == 0 {
		t.Fatal("no keyed providers")
	}
	var want []string
	for _, name := range provider.Providers() {
		if _, ok := provider.KeyRef(name); ok {
			want = append(want, name)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("keyedProviders = %v, want %v", got, want)
	}
	if slices.Contains(got, "llamacpp") {
		t.Fatal("a keyless provider was offered an API-key prompt")
	}
}

// TestOnboardModelPicksAndRecords drives the first-run setup over scripted streams: the
// hosted pick list is shown, the typed answer becomes the model spec, and the choice is
// written as the default so a later launch reuses it without --model.
func TestOnboardModelPicksAndRecords(t *testing.T) {
	noProviderKeys(t)
	dir := t.TempDir()
	var out bytes.Buffer

	// A local model id typed directly: no provider key is needed, so nothing is prompted.
	spec, err := onboardModel(context.Background(), strings.NewReader("qwen3-8b\n"), &out, "openai:gpt-5.5", dir)
	if err != nil {
		t.Fatalf("onboardModel: %v", err)
	}
	if spec != "qwen3-8b" {
		t.Fatalf("spec = %q, want the typed model id", spec)
	}
	shown := out.String()
	for _, want := range []string{"Welcome to Flynn", "1) ", "Model [openai:gpt-5.5]:", "Using qwen3-8b"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("onboarding output missing %q:\n%s", want, shown)
		}
	}
	if got, ok := readActiveModel(dir); !ok || got != "qwen3-8b" {
		t.Fatalf("recorded default = (%q, %v), want the chosen model", got, ok)
	}
}

// TestOnboardModelNumberedChoice proves a number selects from the offered list, and an
// out-of-range number is refused rather than being taken as a model id.
func TestOnboardModelNumberedChoice(t *testing.T) {
	noProviderKeys(t)
	hosted := hostedCatalogModels()
	if len(hosted) == 0 {
		t.Skip("no hosted models offered")
	}

	// Picking a hosted model by number lands on a keyed provider, so it asks for a key;
	// with no terminal to type one on, the refusal is the honest outcome.
	var out bytes.Buffer
	_, err := onboardModel(context.Background(), strings.NewReader("1\n"), &out, "openai:gpt-5.5", t.TempDir())
	if err == nil {
		t.Fatal("a hosted pick with no key and no terminal should refuse")
	}
	if !strings.Contains(out.String(), "1) "+hosted[0]) {
		t.Fatalf("pick list did not offer the first hosted model:\n%s", out.String())
	}

	out.Reset()
	_, err = onboardModel(context.Background(), strings.NewReader("999\n"), &out, "openai:gpt-5.5", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no model chosen") {
		t.Fatalf("out-of-range choice err = %v, want \"no model chosen\"", err)
	}
}

// TestOnboardModelReadError proves an input stream that closes before an answer fails the
// setup instead of silently taking the default.
func TestOnboardModelReadError(t *testing.T) {
	noProviderKeys(t)
	var out bytes.Buffer
	if _, err := onboardModel(context.Background(), strings.NewReader(""), &out, "openai:gpt-5.5", t.TempDir()); err == nil {
		t.Fatal("closed input should fail onboarding")
	}
}

// TestEnsureProviderKeyRefusesWithoutTerminal proves a keyed provider with no stored key
// cannot be onboarded off a pipe: a secret is only ever typed at a terminal, so the
// no-terminal path refuses rather than reading the key from the script's stdin.
func TestEnsureProviderKeyRefusesWithoutTerminal(t *testing.T) {
	noProviderKeys(t)
	err := ensureProviderKey(context.Background(), "openai:gpt-5.5", t.TempDir())
	if err == nil {
		t.Fatal("ensureProviderKey with no key and no terminal = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("err = %v, want a refusal naming the missing terminal", err)
	}
}

// TestResolveModelOrOnboardNonInteractive covers the two non-interactive recoveries the
// resolver makes when the default spec names a provider with no credential: several
// configured providers pick the first deterministically, and nothing configured at all
// surfaces the original credential error for the caller to report.
func TestResolveModelOrOnboardNonInteractive(t *testing.T) {
	t.Run("several configured pick the first", func(t *testing.T) {
		noProviderKeys(t)
		// anthropic and openai are configured; the requested provider is not.
		t.Setenv("ANTHROPIC_API_KEY", "test-key")
		t.Setenv("OPENAI_API_KEY", "test-key")

		dir := t.TempDir()
		want := configuredProviders(context.Background(), credentialSource(dir))
		if !slices.Equal(want, []string{"anthropic", "openai"}) {
			t.Fatalf("configured = %v, want both keys detected in canonical order", want)
		}
		_, _, spec, err := resolveModelOrOnboard(context.Background(), "deepseek", false, dir)
		if err != nil {
			t.Fatalf("resolveModelOrOnboard: %v", err)
		}
		if !strings.HasPrefix(spec, want[0]+":") {
			t.Fatalf("resolved spec = %q, want the first configured provider %q", spec, want[0])
		}
	})

	t.Run("nothing configured surfaces the credential error", func(t *testing.T) {
		noProviderKeys(t)
		_, _, _, err := resolveModelOrOnboard(context.Background(), "openai:gpt-5.5", false, t.TempDir())
		if !errors.Is(err, provider.ErrCredentialNotSet) {
			t.Fatalf("err = %v, want ErrCredentialNotSet with no terminal to onboard on", err)
		}
	})
}
