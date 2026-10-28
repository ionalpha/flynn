package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// countingEmbedder is an embedder that answers every text with the same vector.
type countingEmbedder struct {
	calls int
	model string
}

func (c *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	c.calls++
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func (c *countingEmbedder) Model() string { return c.model }

func TestConfiguredEmbedderIsOffUntilNamed(t *testing.T) {
	t.Setenv(embedModelEnv, "")
	said := ""
	if e := configuredEmbedder(context.Background(), t.TempDir(), func(s string) { said = s }); e != nil {
		t.Fatalf("configuredEmbedder = %v, want none until an install names one", e)
	}
	if said != "" {
		t.Fatalf("said %q, want silence: not configuring embeddings is the default, not a problem", said)
	}
}

func TestConfiguredEmbedderResolvesWhatWasNamed(t *testing.T) {
	t.Setenv(embedModelEnv, "openai:text-embedding-3-large")
	t.Setenv("OPENAI_API_KEY", "o-key")

	e := configuredEmbedder(context.Background(), t.TempDir(), nil)
	if e == nil {
		t.Fatal("configuredEmbedder = nil, want the named model")
	}
	named, ok := e.(interface{ Model() string })
	if !ok || named.Model() != "text-embedding-3-large" {
		t.Fatalf("embedder = %#v, want the model in the spec", e)
	}
}

// A spec that does not resolve costs the session its ranking and not its memory, and
// it is said out loud. A recall that quietly stopped ranking by meaning looks exactly
// like one that never did.
func TestConfiguredEmbedderReportsASpecThatDoesNotResolve(t *testing.T) {
	t.Setenv(embedModelEnv, "anthropic:claude-opus-4-8")
	said := ""
	if e := configuredEmbedder(context.Background(), t.TempDir(), func(s string) { said = s }); e != nil {
		t.Fatalf("configuredEmbedder = %v, want nothing from a provider with no embeddings API", e)
	}
	if !strings.Contains(said, embedModelEnv) || !strings.Contains(said, "words alone") {
		t.Fatalf("said %q, want the variable named and the consequence stated", said)
	}

	// The same failure with nowhere to report it is still not fatal.
	if e := configuredEmbedder(context.Background(), t.TempDir(), nil); e != nil {
		t.Fatalf("configuredEmbedder = %v, want nothing", e)
	}
}

// The run's option carries the embedder to the memory the run is built on, which is
// the only thing the drive config does with it.
func TestWithMemoryEmbedderCarriesToTheRunsMemory(t *testing.T) {
	emb := &countingEmbedder{model: "text-embedding-3-small"}
	var cfg driveConfig
	withMemoryEmbedder(emb)(&cfg)
	if cfg.emb != emb {
		t.Fatalf("cfg.emb = %v, want the embedder the caller resolved", cfg.emb)
	}
}

// A command with nowhere else to put an aside prints it, in the shape the run's own
// memory notices take.
func TestNoticeLineIsShapedLikeTheRunsOtherAsides(t *testing.T) {
	var out bytes.Buffer
	noticeLine(&out)("the endpoint refused")
	if got := out.String(); got != "  (memory: the endpoint refused)\n" {
		t.Fatalf("noticeLine wrote %q", got)
	}
}
