package reliability

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm"
)

// reactiveModel answers each probe the way the grader wants, so the battery scores it perfectly.
// It inspects the request rather than replaying a fixed script, since the battery's probes are not
// sent in a guaranteed order and each needs a tailored answer.
type reactiveModel struct {
	lastSampling *llm.Sampling
}

func (m *reactiveModel) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	m.lastSampling = req.Sampling
	prompt := req.Messages[len(req.Messages)-1].TextContent()
	// A tool is offered and nothing forbids it: emit a well-formed, schema-valid call.
	if len(req.Tools) > 0 && !strings.Contains(prompt, "Do not use") {
		t := req.Tools[0]
		return toolCall(t.Name, validArgs(t.Name)), nil
	}
	switch {
	case strings.Contains(prompt, "single word READY"):
		return sayText("READY"), nil
	case strings.Contains(prompt, "Do not use"):
		return sayText("The capital of France is Paris."), nil
	case strings.Contains(prompt, "one word"):
		return sayText("Blue"), nil
	}
	return sayText("ok"), nil
}

// malformedModel is the quantization-damaged case: it answers tool probes with a call whose
// arguments are not valid JSON, and instruction probes with off-target text, so every dimension
// should score zero.
type malformedModel struct{}

func (malformedModel) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	if len(req.Tools) > 0 && !strings.Contains(req.Messages[len(req.Messages)-1].TextContent(), "Do not use") {
		// Right intent, broken output: a call with unparseable arguments.
		return llm.Response{
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{
				Kind:    llm.KindToolUse,
				ToolUse: &llm.ToolUse{ID: "x", Name: req.Tools[0].Name, Input: json.RawMessage(`{"location": `)},
			}}},
			StopReason: llm.StopToolUse,
		}, nil
	}
	return sayText("Sure, I can help with that whenever you like."), nil
}

func toolCall(name string, input json.RawMessage) llm.Response {
	return llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{
			Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "1", Name: name, Input: input},
		}}},
		StopReason: llm.StopToolUse,
	}
}

func sayText(s string) llm.Response {
	return llm.Response{Message: llm.Text(llm.RoleAssistant, s), StopReason: llm.StopEndTurn}
}

func validArgs(tool string) json.RawMessage {
	switch tool {
	case "get_weather":
		return json.RawMessage(`{"location":"Paris"}`)
	case "read_file":
		return json.RawMessage(`{"path":"config.yaml"}`)
	case "create_event":
		return json.RawMessage(`{"title":"Launch","allDay":true}`)
	}
	return json.RawMessage(`{}`)
}

// TestReliableModelScoresHigh proves a model that answers every probe correctly earns a profile
// the harness reads as fully reliable.
func TestReliableModelScoresHigh(t *testing.T) {
	rep, err := Score(t.Context(), &reactiveModel{})
	if err != nil {
		t.Fatal(err)
	}
	p := rep.Profile()
	if p.ToolCallReliability != 1 || p.StructuredOutput != 1 || p.InstructionFollowing != 1 {
		t.Fatalf("a fully-correct model must score 1 on every dimension, got %+v", p)
	}
	if rep.Version != BatteryVersion {
		t.Fatalf("report version = %q, want %q", rep.Version, BatteryVersion)
	}
}

// TestMalformedModelScoresLow proves the battery catches the failure that actually burns agent
// turns: a model emitting malformed calls and off-target text scores zero across the board, so the
// harness will scaffold it hard.
func TestMalformedModelScoresLow(t *testing.T) {
	rep, err := Score(t.Context(), malformedModel{})
	if err != nil {
		t.Fatal(err)
	}
	p := rep.Profile()
	if p.ToolCallReliability != 0 || p.StructuredOutput != 0 || p.InstructionFollowing != 0 {
		t.Fatalf("a malformed model must score 0 on every dimension, got %+v", p)
	}
}

// TestScorePinsDecoding proves each probe is sent with pinned decoding, so the measurement is
// reproducible on a runtime that honors the seed.
func TestScorePinsDecoding(t *testing.T) {
	m := &reactiveModel{}
	if _, err := Score(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if m.lastSampling == nil || m.lastSampling.Seed == 0 {
		t.Fatalf("probes must pin a seed for reproducibility, got %+v", m.lastSampling)
	}
}

// erroringModel always fails the call, standing in for an unavailable or broken runtime.
type erroringModel struct{}

func (erroringModel) Generate(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("backend down")
}

// TestErroringModelCountsAsFailure proves an erroring call is scored as a failure, not skipped: a
// model that cannot answer reliably must not earn a clean profile by default.
func TestErroringModelCountsAsFailure(t *testing.T) {
	rep, err := Score(t.Context(), erroringModel{})
	if err != nil {
		t.Fatal(err)
	}
	if p := rep.Profile(); p.ToolCallReliability != 0 || p.StructuredOutput != 0 || p.InstructionFollowing != 0 {
		t.Fatalf("an erroring model must score 0, got %+v", p)
	}
	// Every probe was still attempted, so the denominator is the full battery.
	for dim, tally := range rep.Dims {
		if tally.Attempted == 0 {
			t.Fatalf("dimension %d recorded no attempts", dim)
		}
	}
}

// TestScoreCancelled proves a cancelled context aborts the measurement with its error rather than
// returning a partial score.
func TestScoreCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Score(ctx, &reactiveModel{}); err == nil {
		t.Fatal("a cancelled measurement must return an error")
	}
}
