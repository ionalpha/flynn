package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
)

// fakeSource is a vault backed by a literal set of present references, for testing
// which providers auto-detection considers configured.
type fakeSource map[string]secret.Text

func (f fakeSource) Lookup(_ context.Context, ref string) (secret.Text, error) {
	v, ok := f[ref]
	if !ok {
		return secret.Text{}, secret.ErrNotFound
	}
	return v, nil
}

// TestConfiguredProviders proves auto-detection reports exactly the providers whose
// key is present, in the canonical provider order, so the resolver can prefer a
// set-up provider over prompting.
func TestConfiguredProviders(t *testing.T) {
	ref := func(name string) string {
		r, ok := provider.KeyRef(name)
		if !ok {
			t.Fatalf("no key ref for provider %q", name)
		}
		return r
	}
	providers := provider.Providers()
	if len(providers) < 2 {
		t.Skip("need at least two providers to test ordering")
	}
	a, b := providers[0], providers[1]

	cases := []struct {
		name string
		src  fakeSource
		want []string
	}{
		{"none", fakeSource{}, nil},
		{"first only", fakeSource{ref(a): secret.New("k")}, []string{a}},
		{"second only", fakeSource{ref(b): secret.New("k")}, []string{b}},
		{"both in canonical order", fakeSource{ref(b): secret.New("k"), ref(a): secret.New("k")}, []string{a, b}},
	}
	for _, c := range cases {
		got := configuredProviders(context.Background(), c.src)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: configuredProviders = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestExplicitModelWithoutCredentialIsRefused proves a provider the user named on the
// command line is never answered by silently resolving a different one. The fallback
// that prefers an already-configured provider exists for a spec the user did not
// choose; applying it to an explicit --model would send the run, and the prompts in
// it, to a provider the user did not ask for. The same setup resolves to the
// configured provider when the spec was not explicit, so the two paths are told apart
// by intent alone.
func TestExplicitModelWithoutCredentialIsRefused(t *testing.T) {
	// One provider is configured (its key is in the environment) and the one the
	// caller names is not.
	const configured, requested = "openai", "anthropic"
	ref, ok := provider.KeyRef(configured)
	if !ok {
		t.Fatalf("no key ref for provider %q", configured)
	}
	t.Setenv(ref, "test-key")
	if r, ok := provider.KeyRef(requested); ok {
		t.Setenv(r, "")
	}
	dataDir := t.TempDir()

	_, _, _, err := resolveModelOrOnboard(context.Background(), requested, true, dataDir)
	if !errors.Is(err, provider.ErrCredentialNotSet) {
		t.Fatalf("explicit --model %s with no credential: err = %v, want ErrCredentialNotSet", requested, err)
	}
	if !strings.Contains(err.Error(), requested) {
		t.Errorf("error does not name the refused provider: %v", err)
	}

	// Not explicit: the same missing credential recovers onto the configured provider.
	_, _, spec, err := resolveModelOrOnboard(context.Background(), requested, false, dataDir)
	if err != nil {
		t.Fatalf("default spec with a configured provider available: %v", err)
	}
	if !strings.HasPrefix(spec, configured+":") {
		t.Errorf("resolved spec = %q, want the configured provider %q", spec, configured)
	}
}
