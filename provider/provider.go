// Package provider resolves a "provider:model" string to a concrete llm.Model. It
// is the small boundary where the agent's configured model name becomes a backend,
// resolving the provider's API key through a secret.Source (the vault interface) and
// an optional base-URL override for compatible endpoints. The key is carried as a
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
	"github.com/ionalpha/flynn/llm/embed"
	"github.com/ionalpha/flynn/llm/openai"
	"github.com/ionalpha/flynn/secret"
)

// ErrCredentialNotSet reports that a provider's API key was found in neither the
// vault nor the environment. It is wrapped (not replaced) so the precise variable
// name still shows in the message, and a caller can detect the case with errors.Is
// to start an interactive setup prompt instead of failing outright.
var ErrCredentialNotSet = errors.New("provider: credential not set")

// Default model ids for the OpenAI-compatible providers, the same values
// ResolveWith hands each adapter. They live here so a bare provider name can be
// expanded to the concrete model it resolves to without drifting from ResolveWith.
const (
	deepseekDefaultModel = "deepseek-chat"
	geminiDefaultModel   = "gemini-2.5-flash"
	// localLlamaCPPBaseURL is where a local model server listens unless the operator
	// moved it. The same default the chat path uses, stated once.
	localLlamaCPPBaseURL = "http://localhost:8080/v1"
)

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
		return openai.New(key, openai.WithModel(model), openai.WithBaseURL(baseURL), openai.WithVision()), nil
	case "deepseek":
		// DeepSeek speaks the OpenAI Chat Completions format, so the same adapter
		// reaches it by pointing at its endpoint; its default model is the general
		// chat model.
		return openAICompatible(ctx, src, model, "DEEPSEEK_API_KEY", "DEEPSEEK_BASE_URL", "https://api.deepseek.com", deepseekDefaultModel)
	case "gemini":
		// Gemini exposes an OpenAI-compatible endpoint, so the same adapter reaches it
		// by pointing at that base URL; the default model is the fast, low-cost tier.
		return openAICompatible(ctx, src, model, "GEMINI_API_KEY", "GEMINI_BASE_URL", "https://generativelanguage.googleapis.com/v1beta/openai", geminiDefaultModel, openai.WithVision())
	case "llamacpp":
		// A local model server speaking the OpenAI Chat Completions format. It runs on
		// the loopback host, so no API key is required and the base URL defaults to the
		// server's usual address. Tool calls are grammar-constrained at decode time, so
		// even a small local model can only emit a structurally valid call; the
		// constraint is local-only because it rides the server's grammar request field.
		return localOpenAI(ctx, src, model, "LLAMACPP_BASE_URL", "LLAMACPP_VISION", localLlamaCPPBaseURL)
	case "":
		return nil, errors.New("provider: empty spec; want provider:model (e.g. anthropic:claude-opus-4-8)")
	default:
		return nil, fmt.Errorf("provider: unknown provider %q (want one of %s)", name, strings.Join(Providers(), ", "))
	}
}

// DefaultModelID returns the model id a bare provider name resolves to, and whether
// the provider is known. A keyless local server (llamacpp) has no default id. The
// values match the defaults ResolveWith hands each adapter.
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
	case "llamacpp":
		return "", true
	default:
		return "", false
	}
}

// CanonicalSpec expands a bare provider name to "<provider>:<default model>" and
// leaves a full "provider:model" spec (or anything it does not recognise, such as a
// local model id) unchanged. A caller that resolves a bare provider can record and
// display the concrete model it actually uses instead of the provider name alone.
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

// openAICompatible resolves a provider that speaks the OpenAI Chat Completions
// format but lives at its own endpoint, reusing the OpenAI adapter with a default
// base URL and model. A configured base-URL override (the provider's *_BASE_URL
// reference) wins over the default, so a proxy or a self-hosted gateway still works;
// otherwise the provider's standard endpoint is used. Routing these through one
// adapter keeps a single, tested mapping for every OpenAI-shaped backend. Extra
// options carry a provider's per-model capabilities (for example vision, which
// Gemini has and DeepSeek's chat model does not).
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

// localOpenAI resolves a local OpenAI-compatible model server: no API key is
// required (the traffic never leaves the machine), the base URL defaults to the
// server's usual loopback address and may be overridden by baseRef, and tool calls
// are grammar-constrained so the model can only emit a structurally valid call. A
// configured base URL that is not safe to send to (a plaintext non-loopback host)
// is refused, so the local-only assumption cannot be silently broken. Vision is off
// unless visionRef is set truthy: a local server can serve any model, so an image is
// refused rather than sent to one that cannot see it, but an operator running a
// vision model (llava-class) opts in to enable image input.
func localOpenAI(ctx context.Context, src secret.Source, model, baseRef, visionRef, defaultBaseURL string) (llm.Model, error) {
	baseURL := defaultBaseURL
	if u, err := src.Lookup(ctx, baseRef); err == nil && u.Expose() != "" {
		baseURL = u.Expose()
	}
	if !llm.SafeBaseURL(baseURL) {
		return nil, fmt.Errorf("provider: %s must be https or http to localhost", baseRef)
	}
	opts := []openai.Option{openai.WithModel(model), openai.WithBaseURL(baseURL), openai.WithToolGrammar()}
	if u, err := src.Lookup(ctx, visionRef); err == nil && truthy(u.Expose()) {
		opts = append(opts, openai.WithVision())
	}
	return openai.New(secret.Text{}, opts...), nil
}

// truthy reports whether an environment value asks to turn a flag on. It accepts
// the usual affirmatives so an operator can write LLAMACPP_VISION=1, =true, =on,
// or =yes; anything else (including empty or unset) leaves the flag off.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
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
func Providers() []string { return []string{"anthropic", "openai", "deepseek", "gemini", "llamacpp"} }

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

// EmbedProviders lists the provider names that resolve to an embedding client. It
// is shorter than Providers because embeddings are a separate API, not a mode of
// the chat one: Anthropic publishes none, and DeepSeek's endpoint serves chat
// only. Naming the two that work beats resolving five and letting three fail at
// the first recall, which is a wrong ranking discovered a session later.
func EmbedProviders() []string { return []string{"openai", "llamacpp"} }

// ResolveEmbedderWith turns a "provider:model" spec into an embedding client,
// resolving the provider's key through src exactly as ResolveWith does, so an
// operator who has already stored a key does not store a second one for recall.
// A bare provider name uses that provider's default embedding model.
//
// It is a separate call rather than a method on the resolved model because the two
// are separately chosen: the model that reasons and the model that embeds a corpus
// have different costs, and re-embedding a corpus every time somebody switches
// chat model would make the choice of one hostage to the other.
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
		// A local model server, keyless because it is on the loopback host. The model
		// id is whatever the operator loaded; a server serving one model ignores it.
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
