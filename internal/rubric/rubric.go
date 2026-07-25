// Package rubric is Flynn's subjective grader: it scores work that has no ground-truth
// check — "does this settings page look finished", not "do the tests pass" — along
// several named axes at once, and returns a per-axis score with the reason for each
// rather than a single bit. The deterministic ladder in internal/grade answers the
// checkable question; this answers the judged one, and the two are siblings: both
// record their verdict on the spine, and both can be re-graded when the grader improves.
//
// A judged grade is only as trustworthy as three things this package makes explicit.
// First, the axes and their weights are data, not prose buried in a prompt, so raising
// the weight on design and originality — which pushes a generator toward aesthetic risk —
// is a config change, not a prompt rewrite. Second, every axis carries calibration
// bands, worked examples of what a given score means, so a 3-out-of-5 means the same
// thing across runs instead of the model re-anchoring its scale on every call. Third,
// the grader is made to confess every issue it can find before it scores (see
// grader.go), because the failure mode of an out-of-the-box judge is to notice a real
// problem and then talk itself into approving the work anyway.
//
// Grading against a rubric is a model call and so is not pure; the scoring arithmetic
// here is. Assemble turns raw per-axis judgments into a verdict identically on every
// machine, so a recorded assessment re-assembles under replay and the scoring is tested
// without a model. What the grader can actually see is the caller's problem, not the
// rubric's: a judge handed a text digest cannot rule on anything visual no matter how
// good the rubric, which is the live-verification concern, upstream of calibration.
package rubric

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// DefaultMax is the top of the per-axis scale when a rubric leaves Max unset: scores
// run 0..5, the range the calibration bands are written against.
const DefaultMax = 5

// Axis is one dimension a rubric judges: a name, what it measures (Guide, shown to the
// grader), and how much it counts toward the overall score. Weight defaults to 1 when
// zero and a negative weight is treated as 0, matching internal/grade, so an axis is
// never accidentally worth nothing by leaving its weight unset.
type Axis struct {
	Name   string
	Guide  string
	Weight float64
}

// Band is one calibration example pinning what a score on an axis means: on craft, a 2
// means this. Shown to the grader as a worked example, bands are what hold the scale
// steady across iterations — without them a model re-anchors its own scale every call
// and scores drift.
type Band struct {
	Axis   string
	Score  float64
	Reason string
}

// Rubric is a judged grade's standing definition: the axes and their weights, the
// calibration bands, the top of the per-axis scale (Max, default DefaultMax), and the
// Threshold an overall score must reach to count as a pass. It carries no prompt text of
// its own; grader.go renders it into one, so the same rubric drives every grader backend.
type Rubric struct {
	Name      string
	Axes      []Axis
	Bands     []Band
	Max       float64
	Threshold float64
}

// RawScore is one axis's judgment as the grader gave it, before weighting and
// normalization: the score on the rubric's 0..Max scale and the reason for it.
type RawScore struct {
	Score  float64
	Reason string
}

// Issue is a defect the grader named before scoring. The confession pattern (grader.go)
// forces every problem it notices into this list first, so a flaw it would otherwise
// wave away as "not a big deal" is on the record and the axis scores have to account for
// it. Axis is the dimension the issue bears on (empty when it is not axis-specific).
type Issue struct {
	Axis     string
	Severity string
	Detail   string
}

// AxisScore is one axis's graded outcome: the raw score clamped to the rubric's scale,
// the reason the grader gave, and Scored, which is false when the grader returned no
// score for the axis. An unscored axis counts as zero credit against its weight rather
// than being skipped, so a grader that stays silent on an axis lowers the grade instead
// of quietly passing it.
type AxisScore struct {
	Axis   string
	Weight float64
	Score  float64
	Reason string
	Scored bool
}

// Assessment is a judged grade: the issues the grader confessed, the per-axis scores in
// rubric order, the weighted Overall in [0,1], and whether that clears the rubric's
// threshold. Rubric and Fingerprint stamp which grader produced the verdict, so a
// recorded assessment says not just how it scored but what scored it — the provenance a
// re-grade needs to supersede an older verdict with a newer grader's.
type Assessment struct {
	Rubric      string
	Fingerprint string
	Issues      []Issue
	Axes        []AxisScore
	Overall     float64
	Passed      bool
}

// scaleMax is the effective top of the per-axis scale.
func (r Rubric) scaleMax() float64 {
	if r.Max > 0 {
		return r.Max
	}
	return DefaultMax
}

// effectiveWeight defaults a zero weight to 1 and floors a negative one at 0, matching
// internal/grade so the two graders weight axes the same way.
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

// clamp holds a score inside [0, scale]: a grader that returns a 7 on a 0..5 axis, or a
// negative, is corrected rather than allowed to skew the weighted total.
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

// Assemble builds an Assessment from the grader's raw judgment: scores holds the model's
// score and reason per axis (keyed by axis name), issues the defects it confessed. It is
// the pure half of grading — the same inputs assemble identically on every machine — so
// a recorded judgment re-assembles under replay and the scoring is tested without a
// model. Axes are emitted in rubric order; one the grader left unscored counts as zero
// credit against its weight. Overall is the weighted mean of each axis's fraction of the
// scale, in [0,1], and Passed reports whether it reaches Threshold.
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

// Fingerprint is a stable short identifier for the rubric's content: its name, scale,
// threshold, axes (with weights and guides) and calibration bands. Change any of them —
// reweight an axis, reword a guide, add a band — and the fingerprint changes, so a
// verdict stamped with it names the exact grader that produced it and a re-grade under a
// changed rubric is distinguishable from the verdict it supersedes.
func (r Rubric) Fingerprint() string {
	// Marshal the rubric's grading-relevant content in declaration order. The field
	// order is fixed by the struct, so the encoding is canonical without sorting.
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
