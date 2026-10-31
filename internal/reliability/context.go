package reliability

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ionalpha/flynn/llm"
)

// charsPerToken is the characters-per-token ratio a prompt is first sized with, before any server
// has said how many tokens a prompt really was. It is a guess: this filler runs nearer 3.5 on some
// tokenizers, so [MeasureEffectiveContext] replaces it with the ratio the server reports after the
// first rung and sizes every later rung from that.
const charsPerToken = 4.0

// replyHeadroom is the room left in the served context for the model's answer and the chat
// template around the prompt, so a rung sized to the whole context is not refused for a few tokens.
const replyHeadroom = 256

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
// failed even the smallest candidate. Each probe pins decoding, so the measurement reproduces.
// Candidates that are not positive are ignored.
//
// A refusal as too long ([llm.ErrContextOverflow]) shows where the server's window ends, which is
// the same answer as a failed recall, so it ends the search too. Any other error is returned: a
// dropped connection says nothing about the window. The result never exceeds what was sent: it is
// capped at the token count the server reports, and later rungs are sized from the ratio that
// count implies, because [charsPerToken] is only the first guess.
func MeasureEffectiveContext(ctx context.Context, model llm.Model, candidates []int) (int, error) {
	best := 0
	cpt := charsPerToken
	for _, tokens := range ascending(candidates) {
		if tokens <= 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		r, err := recallsAt(ctx, model, tokens, cpt)
		if errors.Is(err, llm.ErrContextOverflow) {
			break // the server's window ends below this rung; the previous pass is the answer
		}
		if err != nil {
			return 0, err
		}
		if r.sent > 0 {
			cpt = float64(r.chars) / float64(r.sent)
		}
		if !r.recalled {
			break // recall is monotone in length; the previous pass is the effective window
		}
		best = tokens
		if r.sent > 0 {
			best = min(tokens, r.sent)
		}
	}
	return best, nil
}

// recall is the outcome of one rung: whether the needle came back, how many characters were sent,
// and the token count the server reported for them (zero when it reports none).
type recall struct {
	recalled bool
	chars    int
	sent     int
}

// recallsAt builds a prompt of about tokens tokens with the needle buried in the middle, asks the
// model to retrieve it, and reports whether it did. The needle sits in the middle because that is
// the hardest place to retain, so the measurement reflects whole-window coherence rather than just
// the recent tail. cpt is the characters-per-token ratio to size the prompt with.
func recallsAt(ctx context.Context, model llm.Model, tokens int, cpt float64) (recall, error) {
	prompt := buildHaystack(tokens, cpt)
	resp, err := model.Generate(ctx, llm.Request{
		Messages: []llm.Message{llm.Text(llm.RoleUser, prompt)},
		Sampling: probeSeed,
	})
	if err != nil {
		return recall{}, err
	}
	return recall{
		recalled: strings.Contains(resp.Message.TextContent(), needleValue),
		chars:    len(prompt),
		sent:     resp.Usage.InputTokens,
	}, nil
}

// buildHaystack returns a prompt of roughly the target token count: filler sentences with the
// needle inserted near the middle, followed by the retrieval question, sized at cpt characters per
// token. The filler is deterministic, so the same target always produces the same prompt and the measurement reproduces.
func buildHaystack(tokens int, cpt float64) string {
	targetChars := int(float64(tokens) * cpt)
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
//
// served is the context the server was actually started with, when known (zero when not). The
// advertised window is what the model supports, but a server is often launched with less, and a
// prompt past it is refused rather than answered, so the ladder ends at what the server can hold
// less [replyHeadroom] for the answer.
func ContextCandidates(advertised, served int) []int {
	if advertised <= 0 {
		return nil
	}
	limit := advertised
	if served > 0 && served-replyHeadroom < limit {
		limit = served - replyHeadroom
	}
	var out []int
	for n := 2048; n < limit; n *= 2 {
		out = append(out, n)
	}
	if limit > 0 {
		out = append(out, limit)
	}
	return out
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
