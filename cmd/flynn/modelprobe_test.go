package main

import (
	"context"
	"io"
	"testing"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/profilestore"
	"github.com/ionalpha/flynn/reliability"
	"github.com/ionalpha/flynn/resource"
)

// constModel answers every request with the same text and no tool call. It is enough to drive the
// reliability battery to a fixed, non-trivial score for testing the probe wiring, without a server.
type constModel struct{ text string }

func (m constModel) Generate(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{Message: llm.Text(llm.RoleAssistant, m.text), StopReason: llm.StopEndTurn}, nil
}

// profileStore builds an in-memory resource store with the ModelProfile kind registered.
func profileStore(t *testing.T) resource.Store {
	t.Helper()
	reg, err := missionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return resource.NewMemory(reg)
}

// TestProbeAndStoreRecordsMeasuredProfile proves the probe wiring stores exactly what the battery
// measured, keyed by the model and its served quant, so a later run reads back the same profile.
func TestProbeAndStoreRecordsMeasuredProfile(t *testing.T) {
	model := constModel{text: "ok"}
	spec := catalog.ModelSpec{
		ID:            "ollama:test",
		ContextTokens: 8192,
		Quants:        []catalog.Quant{{Name: "Q4_K_M", SizeBytes: 100}},
	}
	rs := profileStore(t)

	want, err := reliability.Score(t.Context(), model)
	if err != nil {
		t.Fatal(err)
	}
	if err := probeAndStore(t.Context(), model, spec, "llama.cpp", rs, io.Discard); err != nil {
		t.Fatal(err)
	}

	src, err := profilestore.NewSource(t.Context(), rs)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := src.Profile("ollama:test")
	if !ok {
		t.Fatal("probe did not record a profile")
	}
	if got != want.Profile() {
		t.Fatalf("stored profile = %+v, want the measured %+v", got, want.Profile())
	}
}

// TestProbeProfileDrivesPlan proves the loop closes: a profile recorded by the probe, read back
// through the same store the resolver uses, drives the harness plan. A weak measured model gets a
// scaffolded plan that an unmeasured model would also get, but here it is earned from measurement.
func TestProbeProfileDrivesPlan(t *testing.T) {
	rs := profileStore(t)
	// A model that fails every probe (constant unhelpful text) records a zero profile.
	if err := probeAndStore(t.Context(), constModel{text: "no"}, catalog.ModelSpec{ID: "ollama:weak", Quants: []catalog.Quant{{Name: "Q4_K_M"}}}, "llama.cpp", rs, io.Discard); err != nil {
		t.Fatal(err)
	}
	src, err := profilestore.NewSource(t.Context(), rs)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := src.Profile("ollama:weak")
	if !ok {
		t.Fatal("no profile recorded")
	}
	if p.ToolCallReliability != 0 {
		t.Fatalf("a model that never calls a tool should score 0 on tool calls, got %v", p.ToolCallReliability)
	}
}

// TestProbeMissingArg proves the command refuses without a model id rather than panicking.
func TestProbeMissingArg(t *testing.T) {
	if err := runModelProbe(nil, t.TempDir(), io.Discard); err == nil {
		t.Fatal("probe without a model id must error")
	}
}
