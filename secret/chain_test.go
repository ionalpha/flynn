package secret_test

import (
	"context"
	"encoding"
	"errors"
	"net/url"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/secret"
)

// mapSource resolves references from a fixed map and reports ErrNotFound for the
// rest, standing in for a backend that simply does not hold a credential.
type mapSource map[string]string

func (m mapSource) Lookup(_ context.Context, ref string) (secret.Text, error) {
	if v, ok := m[ref]; ok {
		return secret.New(v), nil
	}
	return secret.Text{}, secret.ErrNotFound
}

// brokenSource stands in for a backend that is present but unusable: a locked
// keychain, a corrupt vault. Every lookup fails with something other than
// ErrNotFound, which the chain must surface rather than mask with a fallback.
type brokenSource struct{ err error }

func (b brokenSource) Lookup(context.Context, string) (secret.Text, error) {
	return secret.Text{}, b.err
}

var errKeychainLocked = errors.New("keychain locked")

func TestChainReturnsFirstHit(t *testing.T) {
	ctx := context.Background()
	first := mapSource{"API_KEY": "from-first"}
	second := mapSource{"API_KEY": "from-second", "OTHER": "only-second"}
	c := secret.Chain(first, second)

	tests := []struct {
		name string
		ref  string
		want string
	}{
		{"earlier source wins", "API_KEY", "from-first"},
		{"falls through a miss", "OTHER", "only-second"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Lookup(ctx, tc.ref)
			if err != nil {
				t.Fatalf("Lookup(%q): %v", tc.ref, err)
			}
			if got.Expose() != tc.want {
				t.Fatalf("Lookup(%q) = %q, want %q", tc.ref, got.Expose(), tc.want)
			}
		})
	}
}

func TestChainNoSourceHoldsTheReference(t *testing.T) {
	c := secret.Chain(mapSource{}, mapSource{"OTHER": "x"})
	if _, err := c.Lookup(context.Background(), "ABSENT"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("absent reference: got %v, want ErrNotFound", err)
	}
	// An empty chain has nothing to consult and must still answer ErrNotFound
	// rather than an empty Text with a nil error.
	if _, err := secret.Chain().Lookup(context.Background(), "ANY"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("empty chain: got %v, want ErrNotFound", err)
	}
}

// TestChainSurfacesBackendFailure is the security-relevant branch: a broken
// backend must stop the chain. Masking it with a later fallback would silently
// downgrade a locked keychain to whatever a weaker source happens to hold, which
// is how a credential gets resolved from the wrong place without anyone noticing.
func TestChainSurfacesBackendFailure(t *testing.T) {
	ctx := context.Background()
	fallback := mapSource{"API_KEY": "must-not-be-reached"}
	c := secret.Chain(brokenSource{err: errKeychainLocked}, fallback)

	got, err := c.Lookup(ctx, "API_KEY")
	if !errors.Is(err, errKeychainLocked) {
		t.Fatalf("broken backend: got %v, want errKeychainLocked", err)
	}
	if !got.Empty() {
		t.Fatalf("a failed chain returned a value: %q", got.Expose())
	}
}

// TestChainSkipsOnlyNotFound pins that the skip rule keys on the ErrNotFound
// sentinel, including when it is wrapped, and on nothing else.
func TestChainSkipsOnlyNotFound(t *testing.T) {
	ctx := context.Background()
	wrapped := brokenSource{err: &url.Error{Op: "get", URL: "vault", Err: secret.ErrNotFound}}
	c := secret.Chain(wrapped, mapSource{"K": "v"})

	got, err := c.Lookup(ctx, "K")
	if err != nil {
		t.Fatalf("a wrapped ErrNotFound should be skipped, got %v", err)
	}
	if got.Expose() != "v" {
		t.Fatalf("after skipping, got %q, want %q", got.Expose(), "v")
	}
}

// TestChainOrderProperty pins the contract over arbitrary layerings: the value a
// chain returns is always the one from the earliest source holding the reference.
func TestChainOrderProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		const ref = "API_KEY"

		// Each layer either holds the reference (with a value tagged by its index)
		// or does not.
		holds := rapid.SliceOfN(rapid.Bool(), 1, 6).Draw(rt, "holds")
		sources := make([]secret.Source, 0, len(holds))
		want := ""
		for i, h := range holds {
			m := mapSource{}
			if h {
				val := "value-" + string(rune('a'+i))
				m[ref] = val
				if want == "" {
					want = val
				}
			}
			sources = append(sources, m)
		}

		got, err := secret.Chain(sources...).Lookup(ctx, ref)
		if want == "" {
			if !errors.Is(err, secret.ErrNotFound) {
				rt.Fatalf("no source holds %q: got %v, want ErrNotFound", ref, err)
			}
			return
		}
		if err != nil {
			rt.Fatalf("Lookup: %v", err)
		}
		if got.Expose() != want {
			rt.Fatalf("Lookup = %q, want the earliest holder %q", got.Expose(), want)
		}
	})
}

// TestChainOfEnvSource wires the real default backend into a chain, so the
// composition a binary actually builds (keychain, then file, then environment) is
// exercised end to end rather than only against test doubles.
func TestChainOfEnvSource(t *testing.T) {
	t.Setenv("FLYNN_TEST_CRED", "env-value")
	c := secret.Chain(mapSource{}, secret.EnvSource{})

	got, err := c.Lookup(context.Background(), "FLYNN_TEST_CRED")
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != "env-value" {
		t.Fatalf("Lookup = %q, want %q", got.Expose(), "env-value")
	}
}

// TestMarshalTextRedacts covers the encoding.TextMarshaler path, which is what an
// encoder that has no JSON opinion (a YAML or TOML writer, a config dumper) calls.
// Text must satisfy the interface and answer with the marker, never the value.
func TestMarshalTextRedacts(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		value := "SECRET-" + rapid.StringMatching(`[A-Za-z0-9/+_-]{1,40}`).Draw(rt, "value")

		var tm encoding.TextMarshaler = secret.New(value)
		b, err := tm.MarshalText()
		if err != nil {
			rt.Fatalf("MarshalText: %v", err)
		}
		if strings.Contains(string(b), value) {
			rt.Fatalf("MarshalText leaked the value: %q", b)
		}
		if string(b) != secret.Redacted {
			rt.Fatalf("MarshalText = %q, want %q", b, secret.Redacted)
		}
	})

	// The zero Text takes the same path, so an unset credential field also renders
	// as the marker rather than as an empty string that reads like a real value.
	b, err := secret.Text{}.MarshalText()
	if err != nil || string(b) != secret.Redacted {
		t.Fatalf("zero Text MarshalText = %q err %v, want %q", b, err, secret.Redacted)
	}
}
