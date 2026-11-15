// Package provider resolves a "provider:model" string to a concrete llm.Model, reading
// the API key through a secret.Source (the vault interface) and an optional base-URL
// override for compatible endpoints. The key is carried as a secret.Text, and a plaintext
// remote base URL is refused so a credential never crosses an unencrypted transport.
package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/anthropic"
	"github.com/ionalpha/flynn/llm/embed"
	"github.com/ionalpha/flynn/llm/openai"
	"github.com/ionalpha/flynn/secret"
)

// ErrCredentialNotSet reports that a provider's API key was found in neither the vault
// nor the environment. It is wrapped, so the variable name still shows in the message and
// a caller can detect it with errors.Is to start an interactive setup prompt.
var ErrCredentialNotSet = errors.New("provider: credential not set")

// Default model ids for the OpenAI-compatible providers, the values [ResolveWith] hands
// each adapter, kept here so a bare provider name expands to the model it resolves to.
const (
	deepseekDefaultModel = "deepseek-chat"
	geminiDefaultModel   = "gemini-2.5-flash"
	// localLlamaCPPBaseURL is where a local model server listens unless the operator
	// moved it.
	localLlamaCPPBaseURL = "http://localhost:8080/v1"
)

// Resolve turns a "provider:model" string (e.g. "anthropic:claude-opus-5-5",
// "openai:gpt-5.5") into an llm.Model, resolving credentials from the process
// environment. It is the zero-config entry point; ResolveWith supplies a custom
// secret.Source (a keychain, a file vault, a remote broker).
func Resolve(spec string) (llm.Model, error) {
	return ResolveWith(context.Background(), spec, secret.EnvSource{})
}

// ResolveWith turns a "provider:model" string into an llm.Model, resolving the API key
// through src. A bare provider name uses its default model. Key and base-URL references
// are the provider's standard environment variable names, which address the vault under a
// non-env Source. A base URL unsafe for a credential (plaintext http to a non-loopback
// host) is rejected.
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
		return openai.New(key, openai.WithModel(model), openai.WithBaseURL(baseURL), openai.WithVision()), nil
	case "deepseek":
		// DeepSeek speaks the OpenAI Chat Completions format, so the OpenAI adapter
		// reaches it at its endpoint.
		return openAICompatible(ctx, src, model, "DEEPSEEK_API_KEY", "DEEPSEEK_BASE_URL", "https://api.deepseek.com", deepseekDefaultModel)
	case "gemini":
		// Gemini exposes an OpenAI-compatible endpoint; the default is the low-cost tier.
		return openAICompatible(ctx, src, model, "GEMINI_API_KEY", "GEMINI_BASE_URL", "https://generativelanguage.googleapis.com/v1beta/openai", geminiDefaultModel, openai.WithVision())
	case "":
		return nil, errors.New("provider: empty spec; want provider:model (e.g. anthropic:claude-opus-5-5)")
	default:
		if k, ok := localKindNamed(name); ok {
			// A local server on the loopback host needs no API key. See [localKind] for
			// how the kinds differ.
			return resolveLocal(ctx, src, k, model)
		}
		if eps := endpointsOf(src); eps.Providers != nil {
			if _, ok := eps.Providers[name]; ok {
				return resolveEndpoint(ctx, src, eps, name, model)
			}
		}
		return nil, fmt.Errorf("provider: unknown provider %q (want one of %s)", name, strings.Join(append(Providers(), EndpointNames(src)...), ", "))
	}
}

// DefaultModelID returns the model id a bare provider name resolves to, and whether the
// provider is known. A keyless local server (llamacpp, ollama, lmstudio) has no default id.
func DefaultModelID(name string) (string, bool) {
	switch name {
	case "anthropic":
		return anthropic.DefaultModel, true
	case "openai":
		return openai.DefaultModel, true
	case "deepseek":
		return deepseekDefaultModel, true
	case "gemini":
		return geminiDefaultModel, true
	default:
		if _, ok := localKindNamed(name); ok {
			return "", true
		}
		return "", false
	}
}

// CanonicalSpec expands a bare provider name to "<provider>:<default model>" and leaves a
// full "provider:model" spec, or anything it does not recognise such as a local model id,
// unchanged, so a caller can record and display the model it uses.
func CanonicalSpec(spec string) string {
	name, model, _ := strings.Cut(spec, ":")
	if model != "" {
		return spec
	}
	if def, ok := DefaultModelID(name); ok && def != "" {
		return name + ":" + def
	}
	return spec
}

// openAICompatible resolves a provider that speaks the OpenAI Chat Completions format at
// its own endpoint through the OpenAI adapter, so every OpenAI-shaped backend shares one
// tested mapping. The provider's *_BASE_URL override wins over defaultBaseURL, so a proxy
// or self-hosted gateway works. extra carries per-model capabilities such as vision.
func openAICompatible(ctx context.Context, src secret.Source, model, keyRef, baseRef, defaultBaseURL, defaultModel string, extra ...openai.Option) (llm.Model, error) {
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
	opts := append([]openai.Option{openai.WithModel(model), openai.WithBaseURL(baseURL)}, extra...)
	return openai.New(key, opts...), nil
}

// truthy reports whether an environment value turns a flag on: 1, true, on or yes.
// Anything else, empty included, leaves it off.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

// credentials resolves a provider's API key (required) and base-URL override (optional)
// from the Source, refusing a base URL that would send the key in the clear. An absent
// base URL is the empty string, so the backend's default is used.
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

// Providers lists the built-in provider names: the four hosted APIs and the keyless
// local servers. Names an operator adds in the providers file are not included; see
// [EndpointNames].
func Providers() []string {
	names := []string{"anthropic", "openai", "deepseek", "gemini"}
	for _, k := range localKinds {
		names = append(names, k.name)
	}
	return names
}

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

// CredentialEnvVars are the environment variables the default Resolve reads a provider
// API key from. A binary can unset these once a model is resolved, so the process stops
// carrying the raw key even though the sandbox already withholds it from commands.
func CredentialEnvVars() []string {
	return []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "DEEPSEEK_API_KEY", "GEMINI_API_KEY"}
}

// EmbedProviders lists the provider names that resolve to an embedding client. Anthropic
// publishes no embeddings API and DeepSeek's endpoint serves chat only, so listing them
// would let recall fail a session later.
func EmbedProviders() []string { return []string{"openai", "llamacpp"} }

// ResolveEmbedderWith turns a "provider:model" spec into an embedding client, resolving
// the key through src as [ResolveWith] does, so a stored key serves recall too. A bare
// provider name uses its default embedding model. It is separate from the chat model
// because the two are chosen separately, and switching chat model must not force a
// corpus to be embedded again.
func ResolveEmbedderWith(ctx context.Context, spec string, src secret.Source) (*embed.Client, error) {
	name, model, _ := strings.Cut(spec, ":")
	switch name {
	case "openai":
		key, baseURL, err := credentials(ctx, src, "OPENAI_API_KEY", "OPENAI_BASE_URL")
		if err != nil {
			return nil, err
		}
		return embed.New(key, embed.WithModel(model), embed.WithBaseURL(baseURL)), nil
	case "llamacpp":
		// A local server is keyless on the loopback host. A server serving one model
		// ignores the id.
		baseURL := localLlamaCPPBaseURL
		if u, err := src.Lookup(ctx, "LLAMACPP_BASE_URL"); err == nil && u.Expose() != "" {
			baseURL = u.Expose()
		}
		if !llm.SafeBaseURL(baseURL) {
			return nil, errors.New("provider: LLAMACPP_BASE_URL must be https or http to localhost")
		}
		return embed.New(secret.Text{}, embed.WithModel(model), embed.WithBaseURL(baseURL)), nil
	case "":
		return nil, errors.New("provider: empty embedding spec; want provider:model (e.g. openai:text-embedding-3-small)")
	default:
		return nil, fmt.Errorf("provider: %q serves no embeddings API (want one of %s)", name, strings.Join(EmbedProviders(), ", "))
	}
}
