package reliability

import (
	"context"
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/llm"
)

// maskModel passes exactly the probes whose prompt is in pass, by delegating those to the
// reactiveModel that answers correctly and failing every other probe with multi-word, tool-free
// text that no grader accepts. It lets a property drive an arbitrary pass/fail pattern through the
// real scorer.
type maskModel struct {
	pass map[string]bool
}

func (m maskModel) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	prompt := req.Messages[len(req.Messages)-1].TextContent()
	if m.pass[prompt] {
		return (&reactiveModel{}).Generate(ctx, req)
	}
	// A deliberately failing answer: several words (fails one-word and exact-word), no tool call
	// (fails tool and schema probes), and nothing a content check looks for.
	return sayText("I would rather not respond to that request"), nil
}

// TestScoreAggregatesAnyPattern proves the scorer is a faithful tally: for any subset of probes a
// model passes, each dimension's score is exactly that dimension's pass fraction, and every score
// stays within [0,1]. This is the property the harness depends on, since a wrong aggregation would
// mis-scaffold a model.
func TestScoreAggregatesAnyPattern(t *testing.T) {
	probes := battery()
	rapid.Check(t, func(rt *rapid.T) {
		pass := map[string]bool{}
		want := map[dimension]Tally{}
		for i, p := range probes {
			prompt := p.req.Messages[len(p.req.Messages)-1].TextContent()
			ok := rapid.Bool().Draw(rt, fmt.Sprintf("pass-%d", i))
			pass[prompt] = ok
			tally := want[p.dim]
			tally.Attempted++
			if ok {
				tally.Passed++
			}
			want[p.dim] = tally
		}

		rep, err := Score(context.Background(), maskModel{pass: pass})
		if err != nil {
			rt.Fatal(err)
		}
		prof := rep.Profile()
		for dim, w := range want {
			got := rep.Dims[dim]
			if got != w {
				rt.Fatalf("dimension %d tally = %+v, want %+v", dim, got, w)
			}
		}
		for _, s := range []float64{prof.ToolCallReliability, prof.StructuredOutput, prof.InstructionFollowing} {
			if s < 0 || s > 1 {
				rt.Fatalf("score %v out of [0,1]", s)
			}
		}
	})
}
