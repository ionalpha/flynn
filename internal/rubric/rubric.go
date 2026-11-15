// Package rubric scores work that has no ground-truth check ("does this settings page
// look finished") along several named axes, returning a score and reason per axis.
// internal/grade answers the checkable question and this package the judged one; both
// record their verdict on the spine and can be re-graded when the grader improves.
//
// A judged grade is as trustworthy as three things this package makes explicit. The
// axes and weights are data, so raising design and originality to push a generator
// toward aesthetic risk is a config change. Every axis has calibration bands, worked
// examples of what a score means, so a 3 of 5 means the same thing across runs. The
// grader confesses every issue before it scores (see [ModelGrader]), because an
// unguided judge notices a real problem and approves the work anyway.
//
// Grading is a model call; the scoring arithmetic is pure. [Rubric.Assemble] turns
// raw per-axis judgments into a verdict identically on every machine, so a recorded
// assessment re-assembles under replay and scoring is tested without a model. What the
// grader can see is the caller's concern: a judge handed a text digest cannot rule on
// anything visual, however good the rubric.
package rubric

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// DefaultMax is the top of the per-axis scale when a rubric leaves Max unset. Scores
// run 0 to 5, the range the calibration bands are written against.
const DefaultMax = 5

// Axis is one dimension a rubric judges: a name, a Guide shown to the grader, and a
// weight. A zero weight counts as 1 and a negative one as 0, as in internal/grade, so
// an axis with its weight unset is never worth nothing.
type Axis struct {
	Name   string
	Guide  string
	Weight float64
}

// Band is one calibration example pinning what a score on an axis means. Shown to the
// grader as worked examples, bands hold the scale steady; without them a model
// re-anchors its scale every call and scores drift.
type Band struct {
	Axis   string
	Score  float64
	Reason string
}

// Rubric is a judged grade's definition: the axes and weights, the calibration bands,
// the top of the per-axis scale (Max, default [DefaultMax]), and the Threshold an
// overall score must reach to pass. It holds no prompt text, so one rubric drives every
// grader backend.
type Rubric struct {
	Name      string
	Axes      []Axis
	Bands     []Band
	Max       float64
	Threshold float64
}

// RawScore is one axis's judgment as the grader gave it, before weighting: the score
// on the rubric's 0 to Max scale and the reason.
type RawScore struct {
	Score  float64
	Reason string
}

// Issue is a defect the grader named before scoring. Confessing first puts a flaw the
// grader would wave away on the record, so the axis scores must account for it. Axis
// is the dimension it bears on, empty when none.
type Issue struct {
	Axis     string
	Severity string
	Detail   string
}

// AxisScore is one axis's graded outcome: the raw score clamped to the scale, the
// grader's reason, and Scored, false when the grader returned no score. An unscored
// axis counts as zero credit against its weight, so a grader silent on an axis lowers
// the grade.
type AxisScore struct {
	Axis   string
	Weight float64
	Score  float64
	Reason string
	Scored bool
}

// Assessment is a judged grade: the confessed issues, the per-axis scores in rubric
// order, the weighted Overall in [0,1], and whether it clears the threshold. Rubric and
// Fingerprint name the grader that produced the verdict, so a re-grade can supersede
// it with a newer grader's.
type Assessment struct {
	Rubric      string
	Fingerprint string
	Issues      []Issue
	Axes        []AxisScore
	Overall     float64
	Passed      bool
}

func (r Rubric) scaleMax() float64 {
	if r.Max > 0 {
		return r.Max
	}
	return DefaultMax
}

// effectiveWeight defaults a zero weight to 1 and floors a negative one at 0, as
// internal/grade does, so the two graders weight axes alike.
func effectiveWeight(w float64) float64 {
	switch {
	case w == 0:
		return 1
	case w < 0:
		return 0
	default:
		return w
	}
}

// clamp holds a score inside [0, scale], so a 7 on a 0 to 5 axis, or a negative,
// cannot skew the weighted total.
func clamp(v, scale float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > scale:
		return scale
	default:
		return v
	}
}

// Assemble builds an Assessment from the grader's raw judgment: scores per axis name
// and the confessed issues. It is pure, so a recorded judgment re-assembles under
// replay. Axes come out in rubric order, and an unscored axis counts as zero credit.
// Overall is the weighted mean of each axis's fraction of the scale, in [0,1], and
// Passed reports whether it reaches Threshold.
func (r Rubric) Assemble(scores map[string]RawScore, issues []Issue) Assessment {
	scale := r.scaleMax()
	a := Assessment{
		Rubric:      r.Name,
		Fingerprint: r.Fingerprint(),
		Issues:      issues,
		Axes:        make([]AxisScore, 0, len(r.Axes)),
	}
	var num, den float64
	for _, ax := range r.Axes {
		w := effectiveWeight(ax.Weight)
		den += w
		as := AxisScore{Axis: ax.Name, Weight: w}
		if raw, ok := scores[ax.Name]; ok {
			as.Score = clamp(raw.Score, scale)
			as.Reason = raw.Reason
			as.Scored = true
			num += w * (as.Score / scale)
		}
		a.Axes = append(a.Axes, as)
	}
	if den > 0 {
		a.Overall = num / den
	}
	a.Passed = a.Overall >= r.Threshold
	return a
}

// Fingerprint is a stable short id for the rubric's content: name, scale, threshold,
// axes with weights and guides, and calibration bands. Any change to them changes it,
// so a stamped verdict names its exact grader and a re-grade under a changed rubric is
// distinguishable from the verdict it supersedes.
func (r Rubric) Fingerprint() string {
	// The struct fixes the field order, so the encoding is canonical without sorting.
	blob, err := json.Marshal(struct {
		Name      string  `json:"name"`
		Max       float64 `json:"max"`
		Threshold float64 `json:"threshold"`
		Axes      []Axis  `json:"axes"`
		Bands     []Band  `json:"bands"`
	}{r.Name, r.scaleMax(), r.Threshold, r.Axes, r.Bands})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:6])
}
