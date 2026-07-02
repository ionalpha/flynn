package reliability

import (
	"context"
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
	if got := ContextCandidates(0); got != nil {
		t.Fatalf("an unknown window must yield no candidates, got %v", got)
	}
	got := ContextCandidates(8192)
	if want := []int{2048, 4096, 8192}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	// An advertised window between powers of two still includes the exact advertised value last.
	if got := ContextCandidates(5000); got[len(got)-1] != 5000 {
		t.Fatalf("candidates must end at the advertised window, got %v", got)
	}
}

// TestHaystackContainsNeedle proves the built prompt actually carries the needle and the retrieval
// prompt, so a failure to recall reflects the model and not a missing fact, and that the prompt
// grows with the target so longer candidates are genuinely longer.
func TestHaystackContainsNeedle(t *testing.T) {
	h := buildHaystack(4096)
	if !strings.Contains(h, needleValue) || !strings.Contains(h, needlePrompt) {
		t.Fatal("haystack must contain both the needle and the retrieval prompt")
	}
	if len(buildHaystack(8192)) <= len(h) {
		t.Fatal("a larger target must produce a longer prompt")
	}
}
