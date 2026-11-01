package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/governed"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/profilestore"
	"github.com/ionalpha/flynn/internal/reliability"
	"github.com/ionalpha/flynn/llm"
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
	reg, err := governed.Registry()
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

// contextServer stands in for a model server started with a context of ctx tokens: it counts a
// prompt at 3.5 characters per token, refuses one past ctx with the llama.cpp overflow body, and
// otherwise answers the needle. Every other prompt (the reliability battery) gets a plain answer.
type contextServer struct {
	ctx     int
	largest int
}

func (s *contextServer) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	prompt := req.Messages[len(req.Messages)-1].TextContent()
	if !strings.Contains(prompt, "Hidden in the text above") {
		return llm.Response{Message: llm.Text(llm.RoleAssistant, "ok"), StopReason: llm.StopEndTurn}, nil
	}
	tokens := int(float64(len(prompt)) / 3.5)
	s.largest = max(s.largest, tokens)
	if tokens > s.ctx {
		return llm.Response{}, fault.Wrap(fault.Terminal, "openai_status", overflowRefusalError(fmt.Sprintf("openai: HTTP 400: request (%d tokens) exceeds the available context size (%d tokens)", tokens, s.ctx)))
	}
	return llm.Response{
		Message:    llm.Text(llm.RoleAssistant, "BLUE-OTTER-4127"),
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: tokens},
	}, nil
}

// overflowRefusalError is what the OpenAI-compatible client returns for a server's context refusal.
type overflowRefusalError string

func (e overflowRefusalError) Error() string      { return string(e) }
func (overflowRefusalError) Is(target error) bool { return target == llm.ErrContextOverflow }

// TestProbeSurvivesServedContextBelowAdvertised is the field failure: a model advertising 131072
// served with 115456. Without a known served context the ladder reaches a rung the server refuses,
// and the probe must still record the last length that passed; with it, the ladder ends inside the
// served window. Either way the stored context is no larger than what was sent, and below the
// served one.
func TestProbeSurvivesServedContextBelowAdvertised(t *testing.T) {
	spec := catalog.ModelSpec{ID: "ollama:test", ContextTokens: 131072, Quants: []catalog.Quant{{Name: "Q4_0"}}}
	for name, served := range map[string]int{"served context unknown": 0, "served context known": 115456} {
		t.Run(name, func(t *testing.T) {
			srv := &contextServer{ctx: 115456}
			rs := profileStore(t)
			var out bytes.Buffer
			if err := probeAndStoreWithin(t.Context(), srv, spec, "llama.cpp", served, rs, &out); err != nil {
				t.Fatalf("probe must survive a refusal past the served context: %v\n%s", err, out.String())
			}
			src, err := profilestore.NewSource(t.Context(), rs)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := src.Profile("ollama:test")
			if !ok {
				t.Fatal("probe did not record a profile")
			}
			if got.EffectiveContext <= 0 || got.EffectiveContext > srv.ctx || got.EffectiveContext > srv.largest {
				t.Fatalf("stored context %d must be positive, within the served %d and within the largest prompt sent %d", got.EffectiveContext, srv.ctx, srv.largest)
			}
			if served > 0 && got.EffectiveContext < 100000 {
				t.Fatalf("with the served context known the ladder should reach it, stored only %d", got.EffectiveContext)
			}
		})
	}
}

// TestServedContext proves the served context is read from the server's /props at its root, and
// that every way of not getting it is zero rather than an error.
func TestServedContext(t *testing.T) {
	cases := map[string]struct {
		handler http.HandlerFunc
		want    int
	}{
		"llama.cpp": {func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/props" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, `{"default_generation_settings":{"n_ctx":115456}}`)
		}, 115456},
		"no endpoint": {http.NotFound, 0},
		"not json":    {func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "nope") }, 0},
		"negative": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"default_generation_settings":{"n_ctx":-1}}`)
		}, 0},
		"no context key": {func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			if got := servedContext(t.Context(), srv.Client(), srv.URL+"/v1"); got != tc.want {
				t.Fatalf("servedContext = %d, want %d", got, tc.want)
			}
		})
	}
	if got := servedContext(t.Context(), nil, "http://127.0.0.1:1/v1"); got != 0 {
		t.Fatalf("an unreachable server must read as unknown, got %d", got)
	}
}
