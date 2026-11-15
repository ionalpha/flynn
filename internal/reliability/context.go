package reliability

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ionalpha/flynn/llm"
)

// charsPerToken is the characters-per-token ratio the first prompt is sized with. It is
// a guess (this filler runs nearer 3.5 on some tokenizers), so
// [MeasureEffectiveContext] sizes later rungs from the ratio the server reports.
const charsPerToken = 4.0

// replyHeadroom is the room left in the served context for the answer and the chat
// template, so a rung sized to the whole context is not refused for a few tokens.
const replyHeadroom = 256

// needle is the fact hidden in the filler. It is distinctive enough that a correct
// answer cannot come by chance.
const (
	needleValue  = "BLUE-OTTER-4127"
	needlePrompt = "Hidden in the text above is a pass phrase. Reply with only the pass phrase, exactly as written."
)

// MeasureEffectiveContext finds the largest candidate context length, in tokens, at
// which the model still recalls a fact buried in a prompt of that size: its usable
// window, often shorter than advertised and shorter still under quantization.
// Candidates run in ascending order and the search stops at the first failure,
// because recall falls off with length and does not recover. Zero means even the
// smallest candidate failed. Non-positive candidates are ignored.
//
// A refusal as too long ([llm.ErrContextOverflow]) marks where the server's window
// ends, so it ends the search like a failed recall. Any other error is returned,
// because a dropped connection says nothing about the window. The result is capped at
// the token count the server reports, so it never exceeds what was sent.
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

// recall is one rung's outcome: whether the needle came back, the characters sent, and
// the token count the server reported for them (zero when none).
type recall struct {
	recalled bool
	chars    int
	sent     int
}

// recallsAt builds a prompt of about tokens tokens with the needle in the middle, the
// hardest place to retain, and reports whether the model retrieved it. cpt is the
// characters-per-token ratio to size the prompt with.
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

// buildHaystack returns a prompt of about tokens tokens at cpt characters per token:
// filler with the needle near the middle, then the question. The filler is
// deterministic, so a target always gives the same prompt.
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

// ascending returns a sorted copy of candidates, leaving the caller's slice untouched.
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

// ContextCandidates returns the token lengths to probe: powers of two up to and
// including the advertised window, and none when advertised is not positive.
//
// served is the context the server was started with, or zero when unknown. A server
// is often launched with less than the model supports, and a prompt past it is
// refused, so the ladder ends at served less [replyHeadroom].
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

// DescribeContext renders an effective-context measurement against the advertised
// window for a report.
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
