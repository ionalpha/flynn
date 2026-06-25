package main

import (
	"context"
	"slices"
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
