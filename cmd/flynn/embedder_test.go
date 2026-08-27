package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/state"
)

// countingEmbedder is an embedder that answers every text with the same vector and
// counts the calls, which is all a wiring test needs: whether the read path reaches
// an embedder at all is the thing this file can get wrong, and how the vectors rank
// is memory/hybrid's own tested concern.
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

// An embedder reaches the read path through the curated store, which is the wiring
// that could have been got wrong: hybrid has to sit against the durable store, not
// over the write policy, or the digest and the ride-along would read the lexical
// order while a single command saw the fused one.
func TestMemoryStackRanksThroughTheCuratedStore(t *testing.T) {
	ctx := context.Background()
	emb := &countingEmbedder{model: "text-embedding-3-small"}
	mem := newMemoryStack(state.NewMemory().Memory(), nil, withEmbedder(emb))

	if _, err := mem.store.Write(ctx, state.MemoryItem{Kind: "fact", Subject: "deploy", Content: "the migration runs first"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := mem.store.Recall(ctx, state.RecallQuery{Query: "how does the deploy go"}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	if emb.calls == 0 {
		t.Fatal("the embedder was never reached, so recall is still ranked by words alone")
	}

	var out bytes.Buffer
	mem.describeRecall(&out)
	if got := out.String(); !strings.Contains(got, "meaning") || !strings.Contains(got, emb.model) {
		t.Fatalf("describeRecall = %q, want it to name meaning and the model", got)
	}
}

// With no embedder the stack is what it always was, and says so. The line is the
// staged capability's visibility: an operator who has not turned ranking by meaning
// on can see that they have not.
func TestMemoryStackWithoutAnEmbedderSaysSo(t *testing.T) {
	var out bytes.Buffer
	newMemoryStack(state.NewMemory().Memory(), nil).describeRecall(&out)
	if got := out.String(); got != "ranked by words\n" {
		t.Fatalf("describeRecall = %q, want the lexical line", got)
	}

	// A nil stack is a session whose memory was never built. It answers the same way
	// rather than panicking on a command that only reports.
	out.Reset()
	var nilStack *memoryStack
	nilStack.describeRecall(&out)
	if got := out.String(); got != "ranked by words\n" {
		t.Fatalf("describeRecall on a nil stack = %q, want the lexical line", got)
	}
}

// An embedder that cannot say which model it is still ranks by meaning, and the line
// says that much rather than inventing a name for it.
func TestMemoryStackNamesAnUnnamedEmbedder(t *testing.T) {
	var out bytes.Buffer
	newMemoryStack(state.NewMemory().Memory(), nil, withEmbedder(&countingEmbedder{})).describeRecall(&out)
	if got := out.String(); got != "ranked by words and meaning\n" {
		t.Fatalf("describeRecall = %q, want meaning with no model named", got)
	}
}

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
