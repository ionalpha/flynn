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
	case "":
		return nil, errors.New("provider: empty spec; want provider:model (e.g. anthropic:claude-opus-4-8)")
	default:
		return nil, fmt.Errorf("provider: unknown provider %q (want one of %s)", name, strings.Join(Providers(), ", "))
	}
}

// credentials resolves a provider's API key (required) and base-URL override
// (optional) from the Source, refusing a base URL that would send the key in the
// clear. The base URL is a configuration value, not a secret, so an absent one is
// the empty string and the backend's default is used.
func credentials(ctx context.Context, src secret.Source, keyRef, baseRef string) (secret.Text, string, error) {
	key, err := src.Lookup(ctx, keyRef)
	if errors.Is(err, secret.ErrNotFound) {
		return secret.Text{}, "", fmt.Errorf("provider: %s is not set", keyRef)
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
func Providers() []string { return []string{"anthropic", "openai"} }

// KeyRef returns the reference a provider's API key is stored under, the same name
// in the environment and in the vault, and whether the provider is known. The auth
// command uses it to seal a key under the name Resolve will look it up by.
func KeyRef(name string) (string, bool) {
	switch name {
	case "anthropic":
		return "ANTHROPIC_API_KEY", true
	case "openai":
		return "OPENAI_API_KEY", true
	default:
		return "", false
	}
}

// CredentialEnvVars are the environment variables the default Resolve reads a
// provider API key from. A binary can unset these once a model is resolved so the
// process stops carrying the raw key in its environment, defense in depth on top
// of the sandbox already withholding the parent environment from commands.
func CredentialEnvVars() []string { return []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"} }
