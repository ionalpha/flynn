package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ionalpha/flynn/memory/hybrid"
	"github.com/ionalpha/flynn/provider"
)

// embedModelEnv names the embedding model recall ranks by, as a provider:model spec
// (`openai:text-embedding-3-small`, `llamacpp:nomic-embed-text`). Unset is the
// default and means recall is ranked by words alone.
//
// It is an environment variable rather than a flag because it is a property of the
// install, not of a run: the corpus is embedded under one model, and switching it
// per invocation would rank half a corpus against vectors from another model. The
// same reasoning is why there is no per-command override.
const embedModelEnv = "FLYNN_EMBED_MODEL"

// noticeLine is where a command with no notice channel of its own puts an aside, in
// the same shape the run's memory notices take.
func noticeLine(w io.Writer) func(string) {
	return func(line string) { _, _ = fmt.Fprintf(w, "  (memory: %s)\n", line) }
}

// configuredEmbedder resolves the embedding model this install ranks recall by, or
// returns nil when none is configured, which leaves recall exactly as it was.
//
// A spec that does not resolve is reported and then dropped. Memory is an advantage
// and not a precondition: an install whose embedding endpoint is misconfigured
// should lose ranking quality for the session, never its memory. The report is what
// keeps that from being silent, because a recall that quietly stopped ranking by
// meaning looks exactly like one that never did.
func configuredEmbedder(ctx context.Context, dataDir string, notify func(string)) hybrid.Embedder {
	spec := strings.TrimSpace(os.Getenv(embedModelEnv))
	if spec == "" {
		return nil
	}
	emb, err := provider.ResolveEmbedderWith(ctx, spec, credentialSource(dataDir))
	if err != nil {
		if notify != nil {
			notify(embedModelEnv + "=" + spec + " did not resolve, so recall is ranked by words alone: " + err.Error())
		}
		return nil
	}
	return emb
}
