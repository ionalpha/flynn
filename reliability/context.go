package reliability

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/ionalpha/flynn/llm"
)

// charsPerToken is the rough characters-per-token ratio used to size a prompt to a target token
// count. It does not need to be exact: the goal is to grow the prompt across a meaningful range, so
// a coarse, model-independent estimate is enough and keeps the probe free of a tokenizer.
const charsPerToken = 4

// needle is the fact hidden inside the filler. It is distinctive enough that a model cannot answer
// by chance, so a correct answer means the model actually retained it across the surrounding text.
const (
	needleValue  = "BLUE-OTTER-4127"
	needlePrompt = "Hidden in the text above is a pass phrase. Reply with only the pass phrase, exactly as written."
)

// MeasureEffectiveContext finds the largest candidate context length, in tokens, at which the
// model still recalls a fact buried in a prompt of that size. That is the model's usable window,
// which is often shorter than its advertised one, and shrinks under quantization. Candidates are
// tried in ascending order and the search stops at the first failure, since recall falls off with
// length and does not recover, so the result is the last length that passed. Zero means the model
// failed even the smallest candidate, so no usable window was measured. Each probe pins decoding,
// so the measurement is reproducible. Candidates that are not positive are ignored.
func MeasureEffectiveContext(ctx context.Context, model llm.Model, candidates []int) (int, error) {
	best := 0
	for _, tokens := range ascending(candidates) {
		if tokens <= 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		ok, err := recallsAt(ctx, model, tokens)
		if err != nil {
			return 0, err
		}
		if !ok {
			break // recall is monotone in length; the previous pass is the effective window
		}
		best = tokens
	}
	return best, nil
}

// recallsAt builds a prompt of about tokens tokens with the needle buried in the middle, asks the
// model to retrieve it, and reports whether it did. The needle sits in the middle because that is
// the hardest place to retain, so the measurement reflects whole-window coherence rather than just
// the recent tail.
func recallsAt(ctx context.Context, model llm.Model, tokens int) (bool, error) {
	prompt := buildHaystack(tokens)
	resp, err := model.Generate(ctx, llm.Request{
		Messages: []llm.Message{llm.Text(llm.RoleUser, prompt)},
		Sampling: probeSeed,
	})
	if err != nil {
		return false, err
	}
	return strings.Contains(resp.Message.TextContent(), needleValue), nil
}

// buildHaystack returns a prompt of roughly the target token count: filler sentences with the
// needle inserted near the middle, followed by the retrieval question. The filler is deterministic,
// so the same target always produces the same prompt and the measurement reproduces.
func buildHaystack(tokens int) string {
	targetChars := tokens * charsPerToken
	const filler = "The quarterly logistics report notes routine throughput across all regional depots. "
	var b strings.Builder
	half := targetChars / 2
	for b.Len() < half {
		b.WriteString(filler)
	}
	b.WriteString("The pass phrase is ")
	b.WriteString(needleValue)
	b.WriteString(". ")
	for b.Len() < targetChars {
		b.WriteString(filler)
	}
	b.WriteString("\n\n")
	b.WriteString(needlePrompt)
	return b.String()
}

// ascending returns a sorted copy of candidates (ascending), so the search visits lengths from
// shortest to longest regardless of input order, without mutating the caller's slice.
func ascending(candidates []int) []int {
	out := make([]int, len(candidates))
	copy(out, candidates)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// ContextCandidates returns the token lengths to probe for a model whose advertised window is
// advertised: a fixed ladder of powers of two up to and including the advertised window, so the
// search spans the realistic range without overshooting the claim. An advertised window of zero or
// less yields no candidates, since there is nothing to measure against.
func ContextCandidates(advertised int) []int {
	if advertised <= 0 {
		return nil
	}
	var out []int
	for n := 2048; n < advertised; n *= 2 {
		out = append(out, n)
	}
	return append(out, advertised)
}

// DescribeContext renders an effective-context measurement against the advertised window, for a
// human-facing report.
func DescribeContext(effective, advertised int) string {
	switch {
	case effective <= 0:
		return "not measured"
	case advertised > 0 && effective < advertised:
		return fmt.Sprintf("%d of %d advertised", effective, advertised)
	default:
		return strconv.Itoa(effective)
	}
}
