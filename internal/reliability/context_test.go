package reliability

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm"
)

// windowModel recalls the needle only when the prompt is shorter than maxChars, standing in for a
// model whose usable window is narrower than the lengths it is asked to handle.
type windowModel struct{ maxChars int }

func (m windowModel) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	prompt := req.Messages[len(req.Messages)-1].TextContent()
	text := "I do not see a pass phrase."
	if len(prompt) < m.maxChars {
		text = "The pass phrase is " + needleValue
	}
	return llm.Response{Message: llm.Text(llm.RoleAssistant, text), StopReason: llm.StopEndTurn}, nil
}

// TestMeasureEffectiveContextFindsBoundary proves the probe reports the largest length the model
// still recalls at, not its advertised one: a model that loses the needle past a threshold is
// measured at the last candidate below it.
func TestMeasureEffectiveContextFindsBoundary(t *testing.T) {
	// buildHaystack(n) is about n*charsPerToken chars; 20000 sits between the 4096 and 8192 builds.
	got, err := MeasureEffectiveContext(t.Context(), windowModel{maxChars: 20000}, []int{2048, 4096, 8192})
	if err != nil {
		t.Fatal(err)
	}
	if got != 4096 {
		t.Fatalf("effective context = %d, want 4096 (the last length recalled)", got)
	}
}

// TestMeasureEffectiveContextFullWindow proves a model that recalls at every candidate is measured
// at the largest one.
func TestMeasureEffectiveContextFullWindow(t *testing.T) {
	got, err := MeasureEffectiveContext(t.Context(), windowModel{maxChars: 1 << 30}, []int{2048, 4096, 8192})
	if err != nil {
		t.Fatal(err)
	}
	if got != 8192 {
		t.Fatalf("effective context = %d, want 8192", got)
	}
}

// TestMeasureEffectiveContextNone proves a model that fails even the smallest candidate measures as
// zero, the unknown the harness treats most conservatively.
func TestMeasureEffectiveContextNone(t *testing.T) {
	got, err := MeasureEffectiveContext(t.Context(), windowModel{maxChars: 0}, []int{2048, 4096})
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("effective context = %d, want 0", got)
	}
}

// TestMeasureEffectiveContextCancelled proves a cancelled context aborts with its error.
func TestMeasureEffectiveContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := MeasureEffectiveContext(ctx, windowModel{maxChars: 1 << 30}, []int{2048}); err == nil {
		t.Fatal("a cancelled measurement must error")
	}
}

// TestContextCandidates proves the ladder spans powers of two up to and including the advertised
// window, and is empty for an unknown window.
func TestContextCandidates(t *testing.T) {
	if got := ContextCandidates(0, 0); got != nil {
		t.Fatalf("an unknown window must yield no candidates, got %v", got)
	}
	got := ContextCandidates(8192, 0)
	if want := []int{2048, 4096, 8192}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	// An advertised window between powers of two still includes the exact advertised value last.
	if got := ContextCandidates(5000, 0); got[len(got)-1] != 5000 {
		t.Fatalf("candidates must end at the advertised window, got %v", got)
	}
}

// TestHaystackContainsNeedle proves the built prompt actually carries the needle and the retrieval
// prompt, so a failure to recall reflects the model and not a missing fact, and that the prompt
// grows with the target so longer candidates are genuinely longer.
func TestHaystackContainsNeedle(t *testing.T) {
	h := buildHaystack(4096, charsPerToken)
	if !strings.Contains(h, needleValue) || !strings.Contains(h, needlePrompt) {
		t.Fatal("haystack must contain both the needle and the retrieval prompt")
	}
	if len(buildHaystack(8192, charsPerToken)) <= len(h) {
		t.Fatal("a larger target must produce a longer prompt")
	}
}

// tokenizingModel stands in for a server that counts tokens at charsPerTok characters per token,
// reports that count as the prompt's usage, refuses a prompt past ctx tokens the way a server does
// (an error that matches [llm.ErrContextOverflow]), and otherwise recalls the needle. It records the
// largest prompt it was asked to take, in tokens.
type tokenizingModel struct {
	charsPerTok float64
	ctx         int
	failWith    error // returned instead of answering, for a prompt past ctx, when set
	largest     int
}

func (m *tokenizingModel) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	tokens := int(float64(len(req.Messages[len(req.Messages)-1].TextContent())) / m.charsPerTok)
	m.largest = max(m.largest, tokens)
	if tokens > m.ctx {
		if m.failWith != nil {
			return llm.Response{}, m.failWith
		}
		return llm.Response{}, fmt.Errorf("openai_status: HTTP 400: request (%d tokens) exceeds the available context size (%d tokens): %w", tokens, m.ctx, llm.ErrContextOverflow)
	}
	return llm.Response{
		Message:    llm.Text(llm.RoleAssistant, "The pass phrase is "+needleValue),
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: tokens},
	}, nil
}

// TestMeasureEffectiveContextOverflowEndsLadder proves a server's refusal of a prompt longer than
// its context ends the search at the last length it took, and is not returned as a failure. The
// ladder is offered rungs past the served window, as it is when the model advertises more than the
// server was launched with.
func TestMeasureEffectiveContextOverflowEndsLadder(t *testing.T) {
	m := &tokenizingModel{charsPerTok: charsPerToken, ctx: 6000}
	got, err := MeasureEffectiveContext(t.Context(), m, []int{2048, 4096, 8192, 16384})
	if err != nil {
		t.Fatalf("an overflow refusal must end the ladder, not fail the probe: %v", err)
	}
	if got != 4096 {
		t.Fatalf("effective context = %d, want 4096 (the last rung the server took)", got)
	}
}

// TestMeasureEffectiveContextOverflowOnFirstRung proves a server too small for even the smallest
// rung measures as zero rather than as an error.
func TestMeasureEffectiveContextOverflowOnFirstRung(t *testing.T) {
	m := &tokenizingModel{charsPerTok: charsPerToken, ctx: 100}
	got, err := MeasureEffectiveContext(t.Context(), m, []int{2048, 4096})
	if err != nil || got != 0 {
		t.Fatalf("got (%d, %v), want (0, nil)", got, err)
	}
}

// TestMeasureEffectiveContextOtherErrorsFail proves only the typed overflow refusal ends the
// ladder: an error that merely reads like one, or any other failure, is returned.
func TestMeasureEffectiveContextOtherErrorsFail(t *testing.T) {
	for name, failure := range map[string]error{
		"network":      errors.New("openai_http: connection reset"),
		"untyped text": errors.New("HTTP 400: request exceeds the available context size"),
	} {
		t.Run(name, func(t *testing.T) {
			m := &tokenizingModel{charsPerTok: charsPerToken, ctx: 100, failWith: failure}
			if _, err := MeasureEffectiveContext(t.Context(), m, []int{2048, 4096}); !errors.Is(err, failure) {
				t.Fatalf("err = %v, want the failure returned", err)
			}
		})
	}
}

// TestMeasureEffectiveContextNeverExceedsWhatWasSent proves the result is bounded by what the
// server counted. The filler tokenizes at 3.5 characters per token here, denser than the 4 the
// first rung is sized with, so the first rung sends more than it names; the stored value must not
// claim the name, and the rungs after it must be sized from the observed ratio rather than overshoot
// the way the first did.
func TestMeasureEffectiveContextNeverExceedsWhatWasSent(t *testing.T) {
	m := &tokenizingModel{charsPerTok: 3.5, ctx: 1 << 30}
	got, err := MeasureEffectiveContext(t.Context(), m, []int{2048, 4096, 8192})
	if err != nil {
		t.Fatal(err)
	}
	if got > m.largest {
		t.Fatalf("effective context %d is larger than the largest prompt sent, %d tokens", got, m.largest)
	}
	if got != 8192 {
		t.Fatalf("effective context = %d, want the last rung 8192", got)
	}
	// The last rung was sized from the first rung's report, so it sent about 8192 tokens, not the
	// 9362 the nominal ratio would have.
	if m.largest > 8192+8192/50 {
		t.Fatalf("the last rung sent %d tokens for a rung of 8192: later rungs must be sized from the server's count", m.largest)
	}
}

// TestMeasureEffectiveContextFirstRungOverstatedIsCapped proves a single rung whose prompt came
// out shorter than named reports the shorter figure: a tokenizer looser than the assumed ratio.
func TestMeasureEffectiveContextFirstRungOverstatedIsCapped(t *testing.T) {
	m := &tokenizingModel{charsPerTok: 5, ctx: 1 << 30}
	got, err := MeasureEffectiveContext(t.Context(), m, []int{2048})
	if err != nil {
		t.Fatal(err)
	}
	if want := m.largest; got != want || got >= 2048 {
		t.Fatalf("effective context = %d, want the %d tokens actually sent, below the rung of 2048", got, want)
	}
}

// TestContextCandidatesStopAtServedContext proves the ladder ends below the context the server was
// launched with, less room for the answer, when that is smaller than the advertised window, and is
// unchanged when the served context is unknown or larger.
func TestContextCandidatesStopAtServedContext(t *testing.T) {
	got := ContextCandidates(131072, 115456)
	if last := got[len(got)-1]; last != 115456-replyHeadroom {
		t.Fatalf("ladder ends at %d, want the served context less headroom, %d", last, 115456-replyHeadroom)
	}
	if want := []int{2048, 4096, 8192, 16384, 32768, 65536}; !slices.Equal(got[:len(got)-1], want) {
		t.Fatalf("rungs below the cap = %v, want %v", got[:len(got)-1], want)
	}
	if got := ContextCandidates(8192, 0); got[len(got)-1] != 8192 {
		t.Fatalf("an unknown served context must leave the ladder at the advertised window, got %v", got)
	}
	if got := ContextCandidates(8192, 1<<20); got[len(got)-1] != 8192 {
		t.Fatalf("a served context past the advertised window must not extend the ladder, got %v", got)
	}
}
