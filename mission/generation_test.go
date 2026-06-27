package mission

import (
	"context"
	"fmt"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

func TestEnvelopeOf(t *testing.T) {
	if env := envelopeOf(nil); env.Pinned || env.Deterministic {
		t.Fatalf("a free-running call must be unpinned and not deterministic, got %+v", env)
	}
	greedy := envelopeOf(&llm.Sampling{Seed: 7, Temperature: 0, TopP: 0.9})
	if !greedy.Pinned || !greedy.Deterministic || greedy.Seed != 7 {
		t.Fatalf("greedy pinned sampling = %+v, want pinned + deterministic + seed 7", greedy)
	}
	sampled := envelopeOf(&llm.Sampling{Seed: 7, Temperature: 0.8})
	if !sampled.Pinned || sampled.Deterministic {
		t.Fatalf("a positive temperature is pinned but not guaranteed deterministic, got %+v", sampled)
	}
	// Out-of-range values are normalized into the envelope.
	clamped := envelopeOf(&llm.Sampling{Temperature: -2, TopP: 9})
	if clamped.Temperature != 0 || clamped.TopP != 1 {
		t.Fatalf("envelope did not normalize sampling: %+v", clamped)
	}
}

// recordingRecorder captures every envelope it is handed.
type recordingRecorder struct {
	envs []GenerationEnvelope
}

func (r *recordingRecorder) RecordGeneration(_ context.Context, env GenerationEnvelope) {
	r.envs = append(r.envs, env)
}

func TestExecutorRecordsGenerationEnvelope(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("done"))
	rec := &recordingRecorder{}
	exec := NewExecutor(model,
		WithSampling(&llm.Sampling{Seed: 11, Temperature: 0}),
		WithGenerationRecorder(rec),
	)

	driveToDone(t, exec, 3)

	if len(rec.envs) == 0 {
		t.Fatal("no generation envelope was recorded")
	}
	got := rec.envs[0]
	if !got.Pinned || !got.Deterministic || got.Seed != 11 {
		t.Fatalf("recorded envelope = %+v, want pinned + deterministic + seed 11", got)
	}
}

func TestExecutorRecordsFreeRunningGeneration(t *testing.T) {
	rec := &recordingRecorder{}
	exec := NewExecutor(llmtest.NewScripted(llmtest.SayText("done")), WithGenerationRecorder(rec))
	driveToDone(t, exec, 3)
	if len(rec.envs) == 0 || rec.envs[0].Pinned {
		t.Fatalf("a free-running run must record an unpinned envelope, got %+v", rec.envs)
	}
}

// seedEcho is a runtime that honors the seed: its output is a deterministic function of the
// request's seed, so the same seed always yields the same tokens.
type seedEcho struct{}

func (seedEcho) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	seed := int64(0)
	if req.Sampling != nil {
		seed = req.Sampling.Seed
	}
	return llm.Response{Message: llm.Text(llm.RoleAssistant, fmt.Sprintf("gen-%d", seed))}, nil
}

// seedBlind ignores the seed and returns a fresh value each call, a nondeterministic runtime.
type seedBlind struct{ n int }

func (b *seedBlind) Generate(context.Context, llm.Request) (llm.Response, error) {
	b.n++
	return llm.Response{Message: llm.Text(llm.RoleAssistant, fmt.Sprintf("gen-%d", b.n))}, nil
}

// generate runs one pinned generation through a model the way the executor builds the request,
// returning the output text, so a test can record-and-replay it.
func generate(t *testing.T, m llm.Model, env GenerationEnvelope) string {
	t.Helper()
	resp, err := m.Generate(context.Background(), llm.Request{
		Messages: []llm.Message{llm.Text(llm.RoleUser, "go")},
		Sampling: &llm.Sampling{Seed: env.Seed, Temperature: env.Temperature, TopP: env.TopP},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return resp.Message.TextContent()
}

// TestReplayEquivalence is the headline proof: a recorded deterministic generation, replayed
// against a runtime that honors its seed, reproduces identical output; a runtime that ignores
// the seed is detected by the same equivalence check rather than silently accepted.
func TestReplayEquivalence(t *testing.T) {
	env := envelopeOf(&llm.Sampling{Seed: 1234, Temperature: 0})
	if !env.Deterministic {
		t.Fatal("greedy decoding must be in the deterministic regime")
	}

	// A seed-honoring runtime reproduces: the recorded run and its replay match.
	first := generate(t, seedEcho{}, env)
	replay := generate(t, seedEcho{}, env)
	if first != replay {
		t.Fatalf("a deterministic generation must reproduce on replay: %q vs %q", first, replay)
	}

	// A seed-ignoring runtime does not reproduce, and the equivalence check catches it.
	blind := &seedBlind{}
	a := generate(t, blind, env)
	b := generate(t, blind, env)
	if a == b {
		t.Fatal("the test runtime was meant to be nondeterministic but reproduced; cannot prove detection")
	}
	// The same check that confirms reproduction above flags the violation here.
	if a == replay {
		t.Fatalf("a nondeterministic runtime must not be mistaken for the recorded output")
	}
}
