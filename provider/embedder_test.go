package provider_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm/embed"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
)

func TestResolveEmbedderReadsTheSameCredential(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "o-key")
	ctx := context.Background()

	c, err := provider.ResolveEmbedderWith(ctx, "openai:text-embedding-3-large", secret.EnvSource{})
	if err != nil {
		t.Fatalf("ResolveEmbedderWith: %v", err)
	}
	if c.Model() != "text-embedding-3-large" {
		t.Errorf("Model() = %q, want the model in the spec", c.Model())
	}
	// A bare provider name is the common case: an operator who wants recall ranked by
	// meaning should not have to know an embedding model id to get one.
	c, err = provider.ResolveEmbedderWith(ctx, "openai", secret.EnvSource{})
	if err != nil {
		t.Fatalf("ResolveEmbedderWith(bare): %v", err)
	}
	if c.Model() != embed.DefaultModel {
		t.Errorf("Model() = %q, want the default", c.Model())
	}
}

// The local server needs no key, which is the whole point of it being an option:
// hybrid recall on a machine with no account and no host.
func TestResolveEmbedderLocalNeedsNoKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := provider.ResolveEmbedderWith(context.Background(), "llamacpp:nomic-embed-text", secret.EnvSource{}); err != nil {
		t.Fatalf("ResolveEmbedderWith(llamacpp): %v", err)
	}
}

// A provider whose API has no embeddings half is refused at resolution, with the
// ones that work named. Resolving it anyway would turn a typo into a recall that
// falls back on every read, which reads as a corpus with no meaning in it.
func TestResolveEmbedderRefusesWhatCannotEmbed(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "a-key")
	ctx := context.Background()

	_, err := provider.ResolveEmbedderWith(ctx, "anthropic:claude-opus-4-8", secret.EnvSource{})
	if err == nil {
		t.Fatal("anthropic resolved an embedder, want it refused")
	}
	for _, want := range provider.EmbedProviders() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q, which does work", err, want)
		}
	}
	if _, err := provider.ResolveEmbedderWith(ctx, "", secret.EnvSource{}); err == nil {
		t.Error("an empty spec resolved, want the usage error")
	}
}

func TestResolveEmbedderNeedsTheKeyAndASafeEndpoint(t *testing.T) {
	ctx := context.Background()

	t.Setenv("OPENAI_API_KEY", "")
	if _, err := provider.ResolveEmbedderWith(ctx, "openai", secret.EnvSource{}); !errors.Is(err, provider.ErrCredentialNotSet) {
		t.Fatalf("err = %v, want ErrCredentialNotSet so a caller can tell it apart from a bad spec", err)
	}

	// The local override is refused the same way the chat path refuses it: an
	// endpoint that is neither loopback nor https is not the local server, whatever
	// the variable says.
	t.Setenv("LLAMACPP_BASE_URL", "http://embeddings.example.com/v1")
	if _, err := provider.ResolveEmbedderWith(ctx, "llamacpp", secret.EnvSource{}); err == nil {
		t.Fatal("a plaintext remote base URL resolved, want it refused")
	}
	t.Setenv("LLAMACPP_BASE_URL", "http://127.0.0.1:9090/v1")
	if _, err := provider.ResolveEmbedderWith(ctx, "llamacpp", secret.EnvSource{}); err != nil {
		t.Fatalf("a loopback override was refused: %v", err)
	}
}
