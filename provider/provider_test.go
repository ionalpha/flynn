package provider_test

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/provider"
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
