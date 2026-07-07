package provider_test

import (
	"testing"

	"github.com/ionalpha/flynn/provider"
)

func TestCanonicalSpec(t *testing.T) {
	cases := map[string]string{
		"openai":                    "openai:gpt-5.5",
		"anthropic":                 "anthropic:claude-opus-4-8",
		"deepseek":                  "deepseek:deepseek-chat",
		"gemini":                    "gemini:gemini-2.5-flash",
		"openai:gpt-4o":             "openai:gpt-4o",             // a full spec is left alone
		"ollama:qwen2.5-coder:1.5b": "ollama:qwen2.5-coder:1.5b", // a local id is left alone
		"llamacpp":                  "llamacpp",                  // keyless local server, no default id
		"bogus":                     "bogus",                     // an unknown provider is left alone
	}
	for in, want := range cases {
		if got := provider.CanonicalSpec(in); got != want {
			t.Errorf("CanonicalSpec(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultModelID(t *testing.T) {
	if got, ok := provider.DefaultModelID("openai"); !ok || got != "gpt-5.5" {
		t.Errorf("DefaultModelID(openai) = %q, %v", got, ok)
	}
	if _, ok := provider.DefaultModelID("nope"); ok {
		t.Error("DefaultModelID(nope) reported known")
	}
	if got, ok := provider.DefaultModelID("llamacpp"); !ok || got != "" {
		t.Errorf("DefaultModelID(llamacpp) = %q, %v; want empty, true", got, ok)
	}
}
