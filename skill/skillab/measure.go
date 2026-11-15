package skillab

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Alpha is the significance level for a verdict. It is a constant so the threshold
// cannot be tuned after seeing the numbers.
const Alpha = 0.05

// Attempt runs one exercise and reports whether its verifier passed. withSkill must
// be the only difference between conditions. An error means the harness failed and
// stops the measurement; a failed run returns false.
type Attempt func(ctx context.Context, t Exercise, repeat int, withSkill bool) (passed bool, err error)

// Pair is one exercise attempted once in each condition. Run-to-run variance exceeds
// a skill's effect, so unpaired samples would need far more runs.
type Pair struct {
	Exercise Exercise
	Repeat   int
	With     bool
	Without  bool
}

// Discordant reports whether the conditions disagreed; only such pairs inform the test.
func (p Pair) Discordant() bool { return p.With != p.Without }

// Measure runs every exercise repeats times (at least once) in both conditions. Both
// arms of a pair run before the next pair, so drift in the model, machine or network
// affects both arms.
func Measure(ctx context.Context, set Set, repeats int, attempt Attempt) (Report, error) {
	if repeats < 1 {
		repeats = 1
	}
	rep := Report{Skill: set.Skill, Repeats: repeats}
	for _, exercise := range set.Exercises {
		for r := 1; r <= repeats; r++ {
			with, err := attempt(ctx, exercise, r, true)
			if err != nil {
				return Report{}, fmt.Errorf("skillab: %s repeat %d with the skill: %w", exercise.Name(), r, err)
			}
			without, err := attempt(ctx, exercise, r, false)
			if err != nil {
				return Report{}, fmt.Errorf("skillab: %s repeat %d without the skill: %w", exercise.Name(), r, err)
			}
			rep.Pairs = append(rep.Pairs, Pair{Exercise: exercise, Repeat: r, With: with, Without: without})
		}
	}
	return rep.scored(), nil
}

// Verdict is what the harness concludes about a skill.
type Verdict string

const (
	// Helped means the skill's arm passed more often, beyond chance.
	Helped Verdict = "helped"
	// NoDifference calls for deleting the skill, never for rerunning until the
	// verdict changes.
	NoDifference Verdict = "no measurable difference"
	// Hurt means the skill's arm passed less often, as when a procedure spends
	// attention the exercise did not need.
	Hurt Verdict = "hurt"
)

// Report is every pair, its tallies and the verdict.
type Report struct {
	Skill   string
	Repeats int
	Pairs   []Pair

	// Gain is the pass-rate difference in percentage points, the effect size.
	WithPasses    int
	WithoutPasses int
	Gain          float64

	// HelpedOnly and HurtOnly count the discordant pairs, the only ones the test reads.
	HelpedOnly int
	HurtOnly   int

	// P is the two-sided exact McNemar probability.
	P float64

	Verdict Verdict
}

func (r Report) scored() Report {
	for _, p := range r.Pairs {
		if p.With {
			r.WithPasses++
		}
		if p.Without {
			r.WithoutPasses++
		}
		switch {
		case p.With && !p.Without:
			r.HelpedOnly++
		case !p.With && p.Without:
			r.HurtOnly++
		}
	}
	if n := len(r.Pairs); n > 0 {
		r.Gain = 100 * (float64(r.WithPasses) - float64(r.WithoutPasses)) / float64(n)
	}
	r.P = mcnemar(r.HelpedOnly, r.HurtOnly)
	switch {
	case r.P > Alpha || r.HelpedOnly == r.HurtOnly:
		r.Verdict = NoDifference
	case r.HelpedOnly > r.HurtOnly:
		r.Verdict = Helped
	default:
		r.Verdict = Hurt
	}
	return r
}

// Uninformative reports that every pair agreed, so the exercises, and not the skill,
// need fixing.
func (r Report) Uninformative() bool {
	return len(r.Pairs) > 0 && r.HelpedOnly == 0 && r.HurtOnly == 0
}

// AllPass reports an uninformative set that is too easy: every run passed.
func (r Report) AllPass() bool {
	return len(r.Pairs) > 0 && r.WithPasses == len(r.Pairs) && r.WithoutPasses == len(r.Pairs)
}

// AllFail reports an uninformative set that is too hard: no run passed.
func (r Report) AllFail() bool {
	return len(r.Pairs) > 0 && r.WithPasses == 0 && r.WithoutPasses == 0
}

// Holdout rescores the held-out exercises alone, so a skill fitted to its author's
// exercises shows a different verdict.
func (r Report) Holdout() Report {
	out := Report{Skill: r.Skill, Repeats: r.Repeats}
	for _, p := range r.Pairs {
		if p.Exercise.Holdout {
			out.Pairs = append(out.Pairs, p)
		}
	}
	return out.scored()
}

// PerExercise tallies each exercise, sorted by objective, which shows an exercise
// that never passes and needs rewriting.
func (r Report) PerExercise() []ExerciseResult {
	index := map[string]int{}
	var out []ExerciseResult
	for _, p := range r.Pairs {
		i, ok := index[p.Exercise.Objective]
		if !ok {
			i = len(out)
			index[p.Exercise.Objective] = i
			out = append(out, ExerciseResult{Exercise: p.Exercise})
		}
		out[i].Attempts++
		if p.With {
			out[i].WithPasses++
		}
		if p.Without {
			out[i].WithoutPasses++
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Exercise.Objective < out[j].Exercise.Objective })
	return out
}

// ExerciseResult is one exercise's tally across its repeats.
type ExerciseResult struct {
	Exercise      Exercise
	Attempts      int
	WithPasses    int
	WithoutPasses int
}

// Decided reports whether this exercise ever separated the two arms.
func (t ExerciseResult) Decided() bool { return t.WithPasses != t.WithoutPasses }

// String renders the report for an author deciding whether to keep the skill.
func (r Report) String() string {
	var b strings.Builder
	n := len(r.Pairs)
	fmt.Fprintf(&b, "%s: %s\n", r.Skill, r.Verdict)
	fmt.Fprintf(&b, "  %d paired runs (%d exercises x %d repeats)\n", n, exerciseCount(r.Pairs), r.Repeats)
	fmt.Fprintf(&b, "  passed with the skill %d/%d, without it %d/%d (%+.1f points)\n",
		r.WithPasses, n, r.WithoutPasses, n, r.Gain)
	fmt.Fprintf(&b, "  disagreed on %d pairs: %d only with, %d only without (p=%.3f)\n",
		r.HelpedOnly+r.HurtOnly, r.HelpedOnly, r.HurtOnly, r.P)
	switch {
	case r.AllPass():
		b.WriteString("  every run passed in both conditions: the exercises are too easy to measure anything\n")
	case r.AllFail():
		b.WriteString("  no run passed in either condition: the exercises are out of reach, so nothing was measured\n")
	case r.Uninformative():
		b.WriteString("  the two conditions never disagreed: this exercise set cannot tell them apart\n")
	}
	for _, t := range r.PerExercise() {
		mark := " "
		if !t.Decided() {
			mark = "="
		}
		held := ""
		if t.Exercise.Holdout {
			held = " [holdout]"
		}
		fmt.Fprintf(&b, "  %s %d/%d vs %d/%d%s  %s\n", mark, t.WithPasses, t.Attempts, t.WithoutPasses, t.Attempts, held, t.Exercise.Objective)
	}
	return b.String()
}

func exerciseCount(pairs []Pair) int {
	seen := map[string]bool{}
	for _, p := range pairs {
		seen[p.Exercise.Objective] = true
	}
	return len(seen)
}

// mcnemar is the exact two-sided binomial test with p=0.5 on the discordant split.
// It is exact because the discordant count is usually under ten, where the
// chi-squared approximation is unreliable. With no discordant pairs it is 1.
func mcnemar(helped, hurt int) float64 {
	n := helped + hurt
	if n == 0 {
		return 1
	}
	k := helped
	if hurt < helped {
		k = hurt
	}
	// P(X <= k) + P(X >= n-k), equal by symmetry.
	tail := 0.0
	for i := 0; i <= k; i++ {
		tail += binomTerm(n, i)
	}
	if p := 2 * tail; p < 1 {
		return p
	}
	return 1
}

// binomTerm is C(n,i)/2^n via log-gamma, so a large n does not overflow.
func binomTerm(n, i int) float64 {
	logC, _ := math.Lgamma(float64(n) + 1)
	li, _ := math.Lgamma(float64(i) + 1)
	lni, _ := math.Lgamma(float64(n-i) + 1)
	return math.Exp(logC - li - lni - float64(n)*math.Ln2)
}
