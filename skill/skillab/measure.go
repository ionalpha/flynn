package skillab

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Alpha is the significance level a verdict is called at. Five per cent, the
// convention, and stated as a constant because a harness that tunes its threshold
// after seeing the numbers is not measuring anything.
const Alpha = 0.05

// Attempt runs one task under one condition and reports whether the task's verifier
// passed. withSkill is the only thing that differs between the two conditions: the
// same objective, the same model, the same tools, the same fresh working directory,
// and the skill under test either offered and readable or pruned from the library.
//
// An error is the harness failing rather than the run failing, and it stops the
// measurement. A run that does not accomplish the objective is a false verdict, not
// an error; those are the observations being counted.
type Attempt func(ctx context.Context, t Task, repeat int, withSkill bool) (passed bool, err error)

// Pair is one task attempted once in each condition. Pairing is what makes the
// comparison affordable: the variance between two runs of the same objective is
// larger than the effect a skill has, so unpaired samples would need far more runs
// to say anything.
type Pair struct {
	Task    Task
	Repeat  int
	With    bool
	Without bool
}

// Discordant reports whether the two conditions disagreed, which is the only kind of
// pair that carries information about the difference between them.
func (p Pair) Discordant() bool { return p.With != p.Without }

// Measure runs every task in the set repeats times under both conditions and returns
// the report. Repeats below one are treated as one.
//
// The order is deliberate: both conditions of a pair run before the next pair
// starts, so a model, a machine or a network that drifts over the course of a long
// measurement drifts through both arms rather than through one.
func Measure(ctx context.Context, set Set, repeats int, attempt Attempt) (Report, error) {
	if repeats < 1 {
		repeats = 1
	}
	rep := Report{Skill: set.Skill, Repeats: repeats}
	for _, task := range set.Tasks {
		for r := 1; r <= repeats; r++ {
			with, err := attempt(ctx, task, r, true)
			if err != nil {
				return Report{}, fmt.Errorf("skillab: %s repeat %d with the skill: %w", task.Name(), r, err)
			}
			without, err := attempt(ctx, task, r, false)
			if err != nil {
				return Report{}, fmt.Errorf("skillab: %s repeat %d without the skill: %w", task.Name(), r, err)
			}
			rep.Pairs = append(rep.Pairs, Pair{Task: task, Repeat: r, With: with, Without: without})
		}
	}
	return rep.scored(), nil
}

// Verdict is what the harness concludes about a skill.
type Verdict string

const (
	// Helped is the skill's arm passing more often than its absence, by more than
	// chance accounts for.
	Helped Verdict = "helped"
	// NoDifference is nothing separating the two arms. It is a shippable verdict and
	// the action it calls for is deleting the skill, not running the harness again
	// until it says something else.
	NoDifference Verdict = "no measurable difference"
	// Hurt is the skill's arm passing less often. A skill can make a run worse by
	// spending the model's attention on a procedure the task did not need.
	Hurt Verdict = "hurt"
)

// Report is the measurement: every pair, the tallies drawn from them, and the
// verdict those tallies support.
type Report struct {
	Skill   string
	Repeats int
	Pairs   []Pair

	// WithPasses and WithoutPasses are the raw counts, and Gain is the difference in
	// pass rate in percentage points: the effect size, which significance says
	// nothing about on its own.
	WithPasses    int
	WithoutPasses int
	Gain          float64

	// HelpedOnly and HurtOnly are the discordant pairs, the only ones the test reads:
	// pairs where both arms agreed carry no information about the difference between
	// them, however many there are.
	HelpedOnly int
	HurtOnly   int

	// P is the two-sided exact probability of a split at least this lopsided if the
	// skill made no difference. Exact rather than approximate because the discordant
	// count is usually small, and a chi-squared approximation over a handful of pairs
	// reports confidence nobody has.
	P float64

	Verdict Verdict
}

// scored fills the tallies and the verdict from the pairs.
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

// Uninformative reports that the task set could not have told the two conditions
// apart: every pair agreed, so no arrangement of the skill would have changed the
// numbers. It is the answer to "every skill passes on the first run", and it means
// the tasks are wrong rather than the skill being fine.
func (r Report) Uninformative() bool {
	return len(r.Pairs) > 0 && r.HelpedOnly == 0 && r.HurtOnly == 0
}

// AllPass and AllFail say which way an uninformative set failed: too easy for the
// difference to show, or too hard for either arm to finish.
func (r Report) AllPass() bool {
	return len(r.Pairs) > 0 && r.WithPasses == len(r.Pairs) && r.WithoutPasses == len(r.Pairs)
}

// AllFail reports the other degenerate case: neither arm ever passed.
func (r Report) AllFail() bool {
	return len(r.Pairs) > 0 && r.WithPasses == 0 && r.WithoutPasses == 0
}

// Holdout returns the report restricted to the held-out tasks, rescored. A skill
// that helps on the tasks its author wrote and does nothing on the tasks someone
// else wrote has been fitted to its own eval, and the two verdicts side by side are
// what makes that visible.
func (r Report) Holdout() Report {
	out := Report{Skill: r.Skill, Repeats: r.Repeats}
	for _, p := range r.Pairs {
		if p.Task.Holdout {
			out.Pairs = append(out.Pairs, p)
		}
	}
	return out.scored()
}

// PerTask summarises each task: how often each arm passed. A task that never passes
// in either arm is measuring nothing and is worth rewriting, and the only place that
// shows is here.
func (r Report) PerTask() []TaskResult {
	index := map[string]int{}
	var out []TaskResult
	for _, p := range r.Pairs {
		i, ok := index[p.Task.Objective]
		if !ok {
			i = len(out)
			index[p.Task.Objective] = i
			out = append(out, TaskResult{Task: p.Task})
		}
		out[i].Attempts++
		if p.With {
			out[i].WithPasses++
		}
		if p.Without {
			out[i].WithoutPasses++
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Task.Objective < out[j].Task.Objective })
	return out
}

// TaskResult is one task's tally across its repeats.
type TaskResult struct {
	Task          Task
	Attempts      int
	WithPasses    int
	WithoutPasses int
}

// Decided reports whether this task ever separated the two arms.
func (t TaskResult) Decided() bool { return t.WithPasses != t.WithoutPasses }

// String renders the report as the paragraph an author reads before deciding
// whether to keep the skill.
func (r Report) String() string {
	var b strings.Builder
	n := len(r.Pairs)
	fmt.Fprintf(&b, "%s: %s\n", r.Skill, r.Verdict)
	fmt.Fprintf(&b, "  %d paired runs (%d tasks x %d repeats)\n", n, taskCount(r.Pairs), r.Repeats)
	fmt.Fprintf(&b, "  passed with the skill %d/%d, without it %d/%d (%+.1f points)\n",
		r.WithPasses, n, r.WithoutPasses, n, r.Gain)
	fmt.Fprintf(&b, "  disagreed on %d pairs: %d only with, %d only without (p=%.3f)\n",
		r.HelpedOnly+r.HurtOnly, r.HelpedOnly, r.HurtOnly, r.P)
	switch {
	case r.AllPass():
		b.WriteString("  every run passed in both conditions: the tasks are too easy to measure anything\n")
	case r.AllFail():
		b.WriteString("  no run passed in either condition: the tasks are out of reach, so nothing was measured\n")
	case r.Uninformative():
		b.WriteString("  the two conditions never disagreed: this task set cannot tell them apart\n")
	}
	for _, t := range r.PerTask() {
		mark := " "
		if !t.Decided() {
			mark = "="
		}
		held := ""
		if t.Task.Holdout {
			held = " [holdout]"
		}
		fmt.Fprintf(&b, "  %s %d/%d vs %d/%d%s  %s\n", mark, t.WithPasses, t.Attempts, t.WithoutPasses, t.Attempts, held, t.Task.Objective)
	}
	return b.String()
}

// taskCount counts the distinct tasks behind a set of pairs.
func taskCount(pairs []Pair) int {
	seen := map[string]bool{}
	for _, p := range pairs {
		seen[p.Task.Objective] = true
	}
	return len(seen)
}

// mcnemar returns the two-sided exact probability of a discordant split at least
// this lopsided when the skill makes no difference. Under that hypothesis each
// discordant pair is a fair coin, so the split is binomial with p=0.5 and the exact
// tail is the sum of the terms at least as extreme.
//
// Exact and not the chi-squared approximation, because the discordant count here is
// routinely under ten and the approximation is unreliable there. With no discordant
// pairs it is 1: nothing disagreed, so nothing is evidence of a difference.
func mcnemar(helped, hurt int) float64 {
	n := helped + hurt
	if n == 0 {
		return 1
	}
	k := helped
	if hurt < helped {
		k = hurt
	}
	// The lower tail doubled: P(X <= k) + P(X >= n-k), which are equal by symmetry.
	tail := 0.0
	for i := 0; i <= k; i++ {
		tail += binomTerm(n, i)
	}
	if p := 2 * tail; p < 1 {
		return p
	}
	return 1
}

// binomTerm is C(n,i)/2^n, computed through log-gamma so a large n does not overflow
// the binomial coefficient on the way to a small probability.
func binomTerm(n, i int) float64 {
	logC, _ := math.Lgamma(float64(n) + 1)
	li, _ := math.Lgamma(float64(i) + 1)
	lni, _ := math.Lgamma(float64(n-i) + 1)
	return math.Exp(logC - li - lni - float64(n)*math.Ln2)
}
