package grade_test

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/grade"
)

// TestProp_CumulativeScoreIsLeadingPassFraction checks the defining invariant of a
// cumulative ladder: with unit weights, the score equals the fraction of leading
// rungs that pass, the count of reached rungs equals that leading run, and the score
// is always in [0,1]. It also confirms no rung past the first failure is reached.
func TestProp_CumulativeScoreIsLeadingPassFraction(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		outcomes := rapid.SliceOfN(rapid.Bool(), 1, 12).Draw(rt, "outcomes")

		milestones := make([]grade.Milestone, len(outcomes))
		for i, ok := range outcomes {
			milestones[i] = grade.Milestone{Name: "m", Check: fixed(ok)}
		}
		l := grade.Ladder{Name: "prop", Mode: grade.Cumulative, Milestones: milestones}

		g, err := l.Grade(context.Background(), grade.MapEvidence{})
		if err != nil {
			rt.Fatalf("grade: %v", err)
		}

		leading := 0
		for _, ok := range outcomes {
			if !ok {
				break
			}
			leading++
		}

		wantScore := float64(leading) / float64(len(outcomes))
		if !approxEqual(g.Score, wantScore) {
			rt.Fatalf("score=%v, want %v (leading=%d of %d)", g.Score, wantScore, leading, len(outcomes))
		}
		if g.Reached != leading {
			rt.Fatalf("reached=%d, want %d", g.Reached, leading)
		}
		if g.Score < 0 || g.Score > 1 {
			rt.Fatalf("score %v out of [0,1]", g.Score)
		}
		for i := leading + 1; i < len(g.Rungs); i++ {
			if g.Rungs[i].Reached {
				rt.Fatalf("rung %d past the first failure was reached", i)
			}
		}
	})
}

func fixed(ok bool) grade.Check {
	return func(context.Context, grade.Evidence) (bool, string, error) { return ok, "", nil }
}
