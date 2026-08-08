package distil

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/memory/consolidate"
	"github.com/ionalpha/flynn/state"
)

// recordingSink collects the dispatch events a distillation produces.
type recordingSink struct{ events []dispatch.Event }

func (s *recordingSink) Append(_ context.Context, e dispatch.Event) error {
	s.events = append(s.events, e)
	return nil
}

// refuse is an Admitter that declines every action.
type refuse struct{ err error }

func (r refuse) Admit(context.Context, dispatch.Action) error { return r.err }

// meter is a dispatch.Hook that records what each governed action reported.
type meter struct {
	calls  int
	tokens int
}

func (*meter) Before(context.Context, dispatch.Action) error { return nil }

func (m *meter) After(_ context.Context, _ dispatch.Action, mt dispatch.Metering, _ error) {
	m.calls++
	m.tokens = mt.Tokens
}

func TestGovernedDistilRecordsTheActionOnTheSpine(t *testing.T) {
	sink := &recordingSink{}
	m := llmtest.NewScripted(llmtest.SayText("the migration has to run second"))
	in := series(3)
	in.Scope = state.Scope{Project: "flynn"}

	got, err := NewGoverned(New(m), dispatch.WithEventSink(sink)).Distil(t.Context(), in)
	if err != nil {
		t.Fatalf("Distil: %v", err)
	}
	if got.Content != "the migration has to run second" {
		t.Fatalf("lesson = %q, want the inner distiller's", got.Content)
	}
	if len(sink.events) == 0 {
		t.Fatal("no events recorded: an offline model call must not be a side channel")
	}
	for _, e := range sink.events {
		if e.Action != Action {
			t.Errorf("event action = %q, want %q", e.Action, Action)
		}
		if e.Scope != in.Scope {
			t.Errorf("event scope = %+v, want the series' %+v", e.Scope, in.Scope)
		}
	}
}

func TestGovernedDistilChargesTheModelCall(t *testing.T) {
	// A sweep that spends tokens has to be charged for them, which means the
	// metering has to carry the usage the inner call reported.
	mt := &meter{}
	m := llmtest.NewScripted(llm.Response{
		Message:    llm.Text(llm.RoleAssistant, "a lesson"),
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 900, OutputTokens: 40, CacheReadTokens: 50, CacheWriteTokens: 10},
	})
	if _, err := NewGoverned(New(m), dispatch.WithHook(mt)).Distil(t.Context(), series(3)); err != nil {
		t.Fatalf("Distil: %v", err)
	}
	if mt.tokens != 1000 {
		t.Fatalf("metered tokens = %d, want 1000 (input, output and both cache sides)", mt.tokens)
	}
}

func TestGovernedDistilMetersZeroForAnotherDistiller(t *testing.T) {
	// NewGoverned takes any Distiller. One that is not this package's meters
	// nothing rather than being refused, so a host's own summarizer still gets
	// the governance and the spine record.
	mt := &meter{}
	inner := consolidate.DistillerFunc(func(context.Context, consolidate.Series) (consolidate.Lesson, error) {
		return consolidate.Lesson{Content: "a host's lesson"}, nil
	})
	got, err := NewGoverned(inner, dispatch.WithHook(mt)).Distil(t.Context(), series(3))
	if err != nil || got.Content != "a host's lesson" {
		t.Fatalf("Distil = (%q, %v), want the host distiller's lesson", got.Content, err)
	}
	if mt.calls != 1 || mt.tokens != 0 {
		t.Fatalf("meter calls = %d, tokens = %d, want one call metering 0", mt.calls, mt.tokens)
	}
}

func TestGovernedDistilDeclinesWhenAdmissionRefuses(t *testing.T) {
	// Governance opting out is not a failure. The series is left exactly as it
	// was found, which is what a decline means to the pass.
	m := llmtest.NewScripted(llmtest.SayText("a lesson"))
	g := NewGoverned(New(m), dispatch.WithAdmitter(refuse{err: errors.New("not admitted")}))
	got, err := g.Distil(t.Context(), series(3))
	if err != nil {
		t.Fatalf("Distil: %v, want a decline rather than a failure", err)
	}
	if got.Content != "" {
		t.Fatalf("lesson = %q, want a decline", got.Content)
	}
	if m.Calls() != 0 {
		t.Fatalf("model calls = %d, want 0: a refused distillation must not reach the model", m.Calls())
	}
}

func TestGovernedDistilSurfacesAnInnerFailure(t *testing.T) {
	// A broken distiller has to stay visible. Reporting it as a decline would
	// leave every series unconsolidated with nothing saying why.
	want := errors.New("the model is down")
	inner := consolidate.DistillerFunc(func(context.Context, consolidate.Series) (consolidate.Lesson, error) {
		return consolidate.Lesson{}, want
	})
	if _, err := NewGoverned(inner).Distil(t.Context(), series(3)); !errors.Is(err, want) {
		t.Fatalf("Distil = %v, want the inner failure", err)
	}
}

func TestGovernedDistilReportsCancellationRatherThanADecline(t *testing.T) {
	// A distillation that did not run has two very different causes, and they
	// must not look alike: governance declining leaves a series for the next run,
	// a torn-down sweep has to stop. Both arrive here as "the work never ran".
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	m := llmtest.NewScripted(llmtest.SayText("a lesson"))
	g := NewGoverned(New(m), dispatch.WithAdmitter(refuse{err: errors.New("not admitted")}))
	if _, err := g.Distil(ctx, series(3)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Distil = %v, want context.Canceled", err)
	}
}
