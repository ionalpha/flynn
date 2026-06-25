package provider_test

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
)

func TestResolveKnownProviders(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "a-key")
	t.Setenv("OPENAI_API_KEY", "o-key")

	for _, spec := range []string{"anthropic:claude-opus-4-8", "openai:gpt-5.5", "anthropic", "openai"} {
		m, err := provider.Resolve(spec)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", spec, err)
		}
		if m == nil {
			t.Fatalf("Resolve(%q) returned a nil model", spec)
		}
	}
}

func TestResolveUnknownProvider(t *testing.T) {
	if _, err := provider.Resolve("groq:llama"); err == nil {
		t.Fatal("unknown provider should error")
	}
	if _, err := provider.Resolve(""); err == nil {
		t.Fatal("empty spec should error")
	}
}

func TestResolveMissingKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := provider.Resolve("anthropic:claude-opus-4-8"); err == nil {
		t.Fatal("missing API key should error")
	}
}

// TestResolveRejectsPlaintextRemoteBaseURL guards the transport: a plaintext http
// base URL to a non-loopback host must be refused, so the API key is never sent
// where it could be sniffed. A localhost http override (a local model server) is
// allowed.
func TestResolveRejectsPlaintextRemoteBaseURL(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "o-key")

	t.Setenv("OPENAI_BASE_URL", "http://api.example.com/v1")
	if _, err := provider.Resolve("openai:gpt-5.5"); err == nil {
		t.Fatal("plaintext remote base URL should be refused")
	}

	t.Setenv("OPENAI_BASE_URL", "http://localhost:1234/v1")
	if _, err := provider.Resolve("openai:gpt-5.5"); err != nil {
		t.Fatalf("localhost http base URL should be allowed: %v", err)
	}

	t.Setenv("OPENAI_BASE_URL", "https://gateway.example.com/v1")
	if _, err := provider.Resolve("openai:gpt-5.5"); err != nil {
		t.Fatalf("https base URL should be allowed: %v", err)
	}
}

// mapSource is a secret.Source backed by a fixed map, standing in for a keychain
// or vault so ResolveWith can be exercised without touching the environment.
type mapSource map[string]string

func (m mapSource) Lookup(_ context.Context, ref string) (secret.Text, error) {
	if v, ok := m[ref]; ok && v != "" {
		return secret.New(v), nil
	}
	return secret.Text{}, secret.ErrNotFound
}

// TestResolveWithCustomSource proves the vault seam: a credential supplied by a
// non-environment Source resolves a working model, and an absent key errors.
func TestResolveWithCustomSource(t *testing.T) {
	src := mapSource{"ANTHROPIC_API_KEY": "from-vault"}
	m, err := provider.ResolveWith(context.Background(), "anthropic:claude-opus-4-8", src)
	if err != nil || m == nil {
		t.Fatalf("ResolveWith from vault: model=%v err=%v", m, err)
	}
	if _, err := provider.ResolveWith(context.Background(), "openai:gpt-5.5", src); err == nil {
		t.Fatal("absent key in vault should error")
	}
}

// TestResolveProperty pins the dispatch contract over the provider-name space:
// with keys present, every supported provider resolves to a non-nil model and
// every unsupported one errors.
func TestResolveProperty(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "a-key")
	t.Setenv("OPENAI_API_KEY", "o-key")
	supported := map[string]bool{"anthropic": true, "openai": true}

	rapid.Check(t, func(rt *rapid.T) {
		name := rapid.SampledFrom([]string{"anthropic", "openai", "gemini", "groq", "xyz"}).Draw(rt, "provider")
		withModel := rapid.Bool().Draw(rt, "withModel")
		spec := name
		if withModel {
			spec = name + ":some-model"
		}

		m, err := provider.Resolve(spec)
		if supported[name] {
			if err != nil || m == nil {
				rt.Fatalf("supported %q failed: model=%v err=%v", spec, m, err)
			}
		} else if err == nil {
			rt.Fatalf("unsupported %q should error", spec)
		}
	})
}
