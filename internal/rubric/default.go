package rubric

// Default is the rubric for judging built work — a page, a feature, a generated
// artifact — where "done" is a matter of quality, not a passing test. Its four axes are
// the ones that make subjective work gradable: design quality, originality, craft, and
// functionality. Every axis ships with calibration bands so a score means the same thing
// across runs rather than drifting toward the middle.
//
// The weights are a tuning knob, not a neutral average. Design and originality are
// weighted above craft and functionality on purpose: leaning the grade toward the
// aesthetic axes pushes a generator being graded against this rubric to take visual and
// conceptual risk rather than settle for something merely working and tidy. A caller
// that wants the opposite — correctness above flair — reweights the axes without touching
// a prompt, which is the whole point of keeping the weights as data.
func Default() Rubric {
	return Rubric{
		Name:      "built-work",
		Max:       DefaultMax,
		Threshold: 0.7,
		Axes: []Axis{
			{
				Name:   "design",
				Weight: 1.5,
				Guide:  "Visual and structural quality: layout, hierarchy, spacing, restraint. Does it look considered, or assembled?",
			},
			{
				Name:   "originality",
				Weight: 1.5,
				Guide:  "Did it take a real idea and commit to it, or reach for the obvious template? Reward a distinct point of view over a safe default.",
			},
			{
				Name:   "craft",
				Weight: 1.0,
				Guide:  "Finish and rigor: edge cases handled, states covered, details right, nothing left half-done or placeholder.",
			},
			{
				Name:   "functionality",
				Weight: 1.0,
				Guide:  "Does it actually do what the objective asked, end to end, on the evidence shown?",
			},
		},
		Bands: []Band{
			{Axis: "design", Score: 1, Reason: "Default styling, misaligned elements, no visual hierarchy. Looks like a wireframe."},
			{Axis: "design", Score: 3, Reason: "Clean and consistent but unremarkable: sensible spacing and hierarchy, no distinctive choices."},
			{Axis: "design", Score: 5, Reason: "A deliberate visual system — every choice reinforces the others, and it reads as designed, not defaulted."},
			{Axis: "originality", Score: 1, Reason: "The obvious template with nothing added; indistinguishable from a hundred others."},
			{Axis: "originality", Score: 3, Reason: "One or two genuine ideas over a conventional base."},
			{Axis: "originality", Score: 5, Reason: "A committed, distinct point of view that reframes the problem, and it pays off."},
			{Axis: "craft", Score: 1, Reason: "Happy path only: empty, loading, and error states missing; placeholder content left in."},
			{Axis: "craft", Score: 3, Reason: "Main cases handled, a few rough edges or unhandled states remain."},
			{Axis: "craft", Score: 5, Reason: "Every state covered, edge cases handled, details right; nothing half-done."},
			{Axis: "functionality", Score: 1, Reason: "Does not do what was asked, or breaks on the evidence shown."},
			{Axis: "functionality", Score: 3, Reason: "Does the core of what was asked; a secondary requirement is missing or partial."},
			{Axis: "functionality", Score: 5, Reason: "Does everything the objective asked, end to end, demonstrably on the evidence."},
		},
	}
}
