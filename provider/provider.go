// Package provider resolves a "provider:model" string to a concrete llm.Model. It
// is the small seam where the agent's configured model name becomes a backend,
// resolving the provider's API key through a secret.Source (the vault seam) and an
// optional base-URL override for compatible endpoints. The key is carried as a
// secret.Text and never as a bare string, and a plaintext remote base URL is
// refused so a credential is never sent over an unencrypted transport. It is the
// place the README's cost-aware router grows from: today it dispatches on the
// provider name; later it can choose per step.
package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/anthropic"
	"github.com/ionalpha/flynn/llm/openai"
	"github.com/ionalpha/flynn/secret"
)

// ErrCredentialNotSet reports that a provider's API key was found in neither the
// vault nor the environment. It is wrapped (not replaced) so the precise variable
// name still shows in the message, and a caller can detect the case with errors.Is
// to start an interactive setup prompt instead of failing outright.
var ErrCredentialNotSet = errors.New("provider: credential not set")

// Resolve turns a "provider:model" string (e.g. "anthropic:claude-opus-4-8",
// "openai:gpt-5.5") into an llm.Model, resolving credentials from the process
// environment. It is the zero-config entry point; ResolveWith supplies a custom
// secret.Source (a keychain, a file vault, a remote broker).
func Resolve(spec string) (llm.Model, error) {
	return ResolveWith(context.Background(), spec, secret.EnvSource{})
}

// ResolveWith turns a "provider:model" string into an llm.Model, resolving the
// provider's API key through src. A bare provider name uses that provider's
// default model. The key reference and the optional base-URL override are the
// provider's standard environment-variable names; with a non-env Source the same
// names address the vault. A configured base URL that is not safe to send a
// credential to (plaintext http to a non-loopback host) is rejected.
func ResolveWith(ctx context.Context, spec string, src secret.Source) (llm.Model, error) {
	name, model, _ := strings.Cut(spec, ":")
	switch name {
	case "anthropic":
		key, baseURL, err := credentials(ctx, src, "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL")
		if err != nil {
			return nil, err
		}
		return anthropic.New(key, anthropic.WithModel(model), anthropic.WithBaseURL(baseURL)), nil
	case "openai":
		key, baseURL, err := credentials(ctx, src, "OPENAI_API_KEY", "OPENAI_BASE_URL")
		if err != nil {
			return nil, err
		}
		return openai.New(key, openai.WithModel(model), openai.WithBaseURL(baseURL)), nil
	case "deepseek":
		// DeepSeek speaks the OpenAI Chat Completions format, so the same adapter
		// reaches it by pointing at its endpoint; its default model is the general
		// chat model.
		return openAICompatible(ctx, src, model, "DEEPSEEK_API_KEY", "DEEPSEEK_BASE_URL", "https://api.deepseek.com", "deepseek-chat")
	case "gemini":
		// Gemini exposes an OpenAI-compatible endpoint, so the same adapter reaches it
		// by pointing at that base URL; the default model is the fast, low-cost tier.
		return openAICompatible(ctx, src, model, "GEMINI_API_KEY", "GEMINI_BASE_URL", "https://generativelanguage.googleapis.com/v1beta/openai", "gemini-2.5-flash")
	case "":
		return nil, errors.New("provider: empty spec; want provider:model (e.g. anthropic:claude-opus-4-8)")
	default:
		return nil, fmt.Errorf("provider: unknown provider %q (want one of %s)", name, strings.Join(Providers(), ", "))
	}
}

// openAICompatible resolves a provider that speaks the OpenAI Chat Completions
// format but lives at its own endpoint, reusing the OpenAI adapter with a default
// base URL and model. A configured base-URL override (the provider's *_BASE_URL
// reference) wins over the default, so a proxy or a self-hosted gateway still works;
// otherwise the provider's standard endpoint is used. Routing these through one
// adapter keeps a single, tested mapping for every OpenAI-shaped backend.
func openAICompatible(ctx context.Context, src secret.Source, model, keyRef, baseRef, defaultBaseURL, defaultModel string) (llm.Model, error) {
	key, baseURL, err := credentials(ctx, src, keyRef, baseRef)
	if err != nil {
		return nil, err
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if model == "" {
		model = defaultModel
	}
	return openai.New(key, openai.WithModel(model), openai.WithBaseURL(baseURL)), nil
}

// credentials resolves a provider's API key (required) and base-URL override
// (optional) from the Source, refusing a base URL that would send the key in the
// clear. The base URL is a configuration value, not a secret, so an absent one is
// the empty string and the backend's default is used.
func credentials(ctx context.Context, src secret.Source, keyRef, baseRef string) (secret.Text, string, error) {
	key, err := src.Lookup(ctx, keyRef)
	if errors.Is(err, secret.ErrNotFound) {
		return secret.Text{}, "", fmt.Errorf("%w (%s)", ErrCredentialNotSet, keyRef)
	}
	if err != nil {
		return secret.Text{}, "", fmt.Errorf("provider: resolve %s: %w", keyRef, err)
	}
	var baseURL string
	if u, err := src.Lookup(ctx, baseRef); err == nil {
		baseURL = u.Expose()
	}
	if !llm.SafeBaseURL(baseURL) {
		return secret.Text{}, "", fmt.Errorf("provider: %s must be https (or http to localhost); refusing to send the API key over an unencrypted transport", baseRef)
	}
	return key, baseURL, nil
}

// Providers lists the supported provider names.
func Providers() []string { return []string{"anthropic", "openai", "deepseek", "gemini"} }

// KeyRef returns the reference a provider's API key is stored under, the same name
// in the environment and in the vault, and whether the provider is known. The auth
// command uses it to seal a key under the name Resolve will look it up by.
func KeyRef(name string) (string, bool) {
	switch name {
	case "anthropic":
		return "ANTHROPIC_API_KEY", true
	case "openai":
		return "OPENAI_API_KEY", true
	case "deepseek":
		return "DEEPSEEK_API_KEY", true
	case "gemini":
		return "GEMINI_API_KEY", true
	default:
		return "", false
	}
}

// CredentialEnvVars are the environment variables the default Resolve reads a
// provider API key from. A binary can unset these once a model is resolved so the
// process stops carrying the raw key in its environment, defense in depth on top
// of the sandbox already withholding the parent environment from commands.
func CredentialEnvVars() []string {
	return []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "DEEPSEEK_API_KEY", "GEMINI_API_KEY"}
}
