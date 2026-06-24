// Package provider resolves a "provider:model" string to a concrete llm.Model. It
// is the small seam where the agent's configured model name becomes a backend,
// reading the provider's API key (and optional base URL for compatible endpoints)
// from the environment. It is the place the README's cost-aware router grows from:
// today it dispatches on the provider name; later it can choose per step.
package provider

import (
	"fmt"
	"os"
	"strings"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/anthropic"
	"github.com/ionalpha/flynn/llm/openai"
)

// Resolve turns a "provider:model" string (e.g. "anthropic:claude-opus-4-8",
// "openai:gpt-5.5") into an llm.Model. A bare provider name uses that provider's
// default model. The provider's API key is read from its standard environment
// variable, and an optional base-URL override (for a proxy or a compatible
// endpoint) from a second one.
func Resolve(spec string) (llm.Model, error) {
	name, model, _ := strings.Cut(spec, ":")
	switch name {
	case "anthropic":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("provider: ANTHROPIC_API_KEY is not set")
		}
		opts := []anthropic.Option{anthropic.WithModel(model)}
		if u := os.Getenv("ANTHROPIC_BASE_URL"); u != "" {
			opts = append(opts, anthropic.WithBaseURL(u))
		}
		return anthropic.New(key, opts...), nil
	case "openai":
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("provider: OPENAI_API_KEY is not set")
		}
		opts := []openai.Option{openai.WithModel(model)}
		if u := os.Getenv("OPENAI_BASE_URL"); u != "" {
			opts = append(opts, openai.WithBaseURL(u))
		}
		return openai.New(key, opts...), nil
	case "":
		return nil, fmt.Errorf("provider: empty spec; want provider:model (e.g. anthropic:claude-opus-4-8)")
	default:
		return nil, fmt.Errorf("provider: unknown provider %q (want one of %s)", name, strings.Join(Providers(), ", "))
	}
}

// Providers lists the supported provider names.
func Providers() []string { return []string{"anthropic", "openai"} }
