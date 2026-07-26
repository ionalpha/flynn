package rubric

import (
	"context"

	"github.com/ionalpha/flynn/spine"
)

// RegradeResult summarizes re-grading one recorded verdict with the current grader. Held
// reports that the pass/fail verdict is unchanged; Regraded reports that the grader has
// actually moved (its fingerprint differs from the one that produced Prior), so a Held
// verdict from a changed grader is a stronger signal than one no grader touched.
// ScoreDelta is how far the overall score moved.
type RegradeResult struct {
	Prior      Assessment
	Current    Assessment
	Held       bool
	Regraded   bool
	ScoreDelta float64
}

// Regrade re-checks whether a past verdict still holds. It re-assesses the same subject
// with the current grader and reconciles the result against prior: this is the subjective
// analogue of learn.Regrade, which re-runs a captured skill's check against current
// ground truth and re-confirms or retires it. A judged verdict is a run outcome, and a
// grader improves over time — a reworded rubric, reweighted axes, a stronger model — so
// the verdict it produced once is re-checked rather than trusted forever.
//
// The fresh verdict is recorded on the spine (when log is non-nil), stamped with the
// current grader's fingerprint, so both verdicts and which grader produced each stay
// durable: a re-grade is provenance-stamped, not a silent overwrite. This rides the
// spine the same way grade.Record and learn.Regrade do rather than forking a parallel
// calibration loop. Sourcing prior and the subject from a full spine replay, instead of
// the caller supplying them, is the replay-to-regrade follow-on; the reconciliation here
// is the same either way. A nil grader is a no-op.
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
