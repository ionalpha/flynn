package rubric

import (
	"context"

	"github.com/ionalpha/flynn/spine"
)

// RegradeResult summarizes re-grading one recorded verdict with the current grader.
// Held reports that pass or fail is unchanged; Regraded reports that the grader's
// fingerprint differs from the one that produced Prior, so a Held verdict from a
// changed grader is stronger evidence than one no grader touched. ScoreDelta is how
// far the overall score moved.
type RegradeResult struct {
	Prior      Assessment
	Current    Assessment
	Held       bool
	Regraded   bool
	ScoreDelta float64
}

// Regrade re-checks whether a past verdict still holds by re-assessing the same
// subject with the current grader and reconciling the result against prior, as
// learn.Regrade does for a captured skill. Graders improve (a reworded rubric,
// reweighted axes, a stronger model), so a verdict is re-checked instead of trusted
// forever.
//
// When log is non-nil the fresh verdict is recorded with the current grader's
// fingerprint, so both verdicts and their graders stay on the record. A nil grader is
// a no-op.
func Regrade(ctx context.Context, log spine.Log, stream string, g Grader, prior Assessment, s Subject) (RegradeResult, error) {
	if g == nil {
		return RegradeResult{}, nil
	}
	current, err := g.Assess(ctx, s)
	if err != nil {
		return RegradeResult{}, err
	}
	if log != nil {
		if err := Record(ctx, log, stream, current); err != nil {
			return RegradeResult{}, err
		}
	}
	return RegradeResult{
		Prior:      prior,
		Current:    current,
		Held:       prior.Passed == current.Passed,
		Regraded:   prior.Fingerprint != current.Fingerprint,
		ScoreDelta: current.Overall - prior.Overall,
	}, nil
}
