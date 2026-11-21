package skillab

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ionalpha/flynn/usage"
)

// Alpha is the significance level for a verdict. It is a constant so the threshold
// cannot be tuned after seeing the numbers.
const Alpha = 0.05

// Attempt runs one exercise and reports whether its verifier passed. withSkill must
// be the only difference between conditions. An error means the harness failed and
// stops the measurement; a failed run returns false.
type Attempt func(ctx context.Context, t Exercise, repeat int, withSkill bool) (passed bool, err error)

// Arms names the two sides of a comparison. The candidate is the version under
// test and the baseline is what it is judged against: the same skill removed, or
// an earlier version of it.
type Arms struct {
	Candidate string
	Baseline  string
}

// withAndWithout is the comparison [Measure] makes: a skill against none.
var withAndWithout = Arms{Candidate: "with the skill", Baseline: "without the skill"}

// Cost is what one attempt, or a sum of them, spent. A zero Cost means nothing was
// metered, which is how a measurement made through [Measure] reports it.
type Cost struct {
	// Tokens carries the four token figures with the meanings [usage.Tokens] gives
	// them, so a sum goes through [usage.Tokens.Add] and a total through
	// [usage.Tokens.Total] and a cache read is never counted twice.
	Tokens usage.Tokens
	// Turns is the number of model calls that returned.
	Turns int
	// Wall is the time the agent ran, excluding setup and the checks.
	Wall time.Duration
	// Money is what a price table charged for the priced turns, in Currency. It is the
	// whole spend only when Unpriced is zero: see [Cost.Known].
	Money    usage.Nanos
	Currency string
	// Unpriced counts the turns no price was found for. A provider reports tokens and
	// not a price, so a model the table does not list leaves every turn here.
	Unpriced int
}

// Known reports whether Money is the whole spend: something was metered and every turn
// was priced. A cost that is not known is never read as zero.
func (c Cost) Known() bool { return c.Turns > 0 && c.Unpriced == 0 && c.Currency != "" }

// Add returns the sum of c and o. Two currencies cannot be summed, so the turns of the
// second are counted as unpriced.
func (c Cost) Add(o Cost) Cost {
	currency, mixed := c.Currency, 0
	switch {
	case currency == "":
		currency = o.Currency
	case o.Currency != "" && o.Currency != currency:
		mixed = o.Turns
	}
	return Cost{
		Money:    c.Money + o.Money,
		Currency: currency,
		Unpriced: c.Unpriced + o.Unpriced + mixed,
		Tokens:   c.Tokens.Add(o.Tokens),
		Turns:    c.Turns + o.Turns,
		Wall:     c.Wall + o.Wall,
	}
}

// Ending says how an attempt's run stopped when it did not finish by itself. A run
// stopped short still fails its exercise; the report keeps the reason apart from an
// ordinary failure, because a stalled arm and a wrong answer call for different fixes.
type Ending string

const (
	// Finished is a run that ended by itself, right or wrong.
	Finished Ending = ""
	// Stalled is a run stopped for making no progress.
	Stalled Ending = "stalled"
	// BudgetExhausted is a run stopped by a step, token or spend limit.
	BudgetExhausted Ending = "budget exhausted"
	// Aborted is any other early stop, such as a model error or a cancelled run.
	Aborted Ending = "aborted"
)

// Outcome is what one attempt produced: the primary check, any named secondary
// checks, how the run ended and what the attempt cost.
type Outcome struct {
	// Ended is how the run stopped. Passed and Checks still hold the verifier's word
	// on whatever the run left behind.
	Ended Ending
	// Passed is the result of the exercise's verify command.
	Passed bool
	// Checks maps each of the exercise's [Exercise.Checks] to its result. A declared
	// check with no entry is a harness failure, and an undeclared entry is ignored.
	Checks map[string]bool
	Cost   Cost
}

// Trial runs one exercise in one arm. candidate selects the arm and must be the
// only difference between the two. An error means the harness failed and stops the
// comparison; a run that did badly returns an [Outcome] saying so.
type Trial func(ctx context.Context, e Exercise, repeat int, candidate bool) (Outcome, error)

// Pair is one exercise attempted once in each condition. Run-to-run variance exceeds
// a skill's effect, so unpaired samples would need far more runs. With is the
// candidate arm and Without the baseline.
type Pair struct {
	Exercise Exercise
	Repeat   int
	With     bool
	Without  bool
	// WithChecks and WithoutChecks hold the results of the exercise's declared
	// secondary checks, and WithCost and WithoutCost what each arm spent.
	WithChecks    map[string]bool
	WithoutChecks map[string]bool
	WithCost      Cost
	WithoutCost   Cost
	// WithEnded and WithoutEnded say how each arm's run stopped.
	WithEnded    Ending
	WithoutEnded Ending
}

// Discordant reports whether the conditions disagreed; only such pairs inform the test.
func (p Pair) Discordant() bool { return p.With != p.Without }

// Measure compares a skill against its absence. Each attempt reports only whether
// the verifier passed, so the report carries no checks and no cost.
func Measure(ctx context.Context, set Set, repeats int, attempt Attempt) (Report, error) {
	return Compare(ctx, set, repeats, withAndWithout, func(ctx context.Context, e Exercise, repeat int, candidate bool) (Outcome, error) {
		passed, err := attempt(ctx, e, repeat, candidate)
		return Outcome{Passed: passed}, err
	})
}

// Compare runs every exercise repeats times (at least once) in both arms. Both arms
// of a pair run before the next pair, so drift in the model, machine or network
// affects both arms.
func Compare(ctx context.Context, set Set, repeats int, arms Arms, trial Trial) (Report, error) {
	return CompareWithin(ctx, set, repeats, arms, trial, Limit{})
}

// Limit bounds what a comparison may spend. The zero value is no limit.
type Limit struct {
	// Money stops the comparison from starting a pair once the pairs already run have
	// cost this much. It is checked between pairs, because half a pair observes
	// nothing, so a run can pass it by up to one pair. A spend that cannot be read (a
	// turn no price was found for) stops the comparison too: a limit that cannot be
	// checked must not be assumed to hold.
	Money usage.Nanos
	// Currency is the currency of Money. A spend in another currency is unreadable.
	Currency string
}

// reached returns why no further pair may start, or "" when one may.
func (l Limit) reached(spent Cost) string {
	if l.Money <= 0 {
		return ""
	}
	if spent.Unpriced > 0 || (spent.Currency != "" && spent.Currency != l.Currency) {
		return fmt.Sprintf("spend limit %s %s could not be checked: %d turn(s) had no price", l.Money, l.Currency, spent.Unpriced)
	}
	if spent.Money >= l.Money {
		return fmt.Sprintf("spend limit %s %s reached (%s spent)", l.Money, l.Currency, spent.Money)
	}
	return ""
}

// CompareWithin is [Compare] under a [Limit]. When the limit stops it, the report holds
// the pairs that ran, and Stopped says why.
func CompareWithin(ctx context.Context, set Set, repeats int, arms Arms, trial Trial, limit Limit) (Report, error) {
	if repeats < 1 {
		repeats = 1
	}
	rep := Report{Skill: set.Skill, Repeats: repeats, Arms: arms, Planned: len(set.Exercises) * repeats}
	var spent Cost
	for _, exercise := range set.Exercises {
		for r := 1; r <= repeats; r++ {
			if reason := limit.reached(spent); reason != "" {
				rep.Stopped = reason
				return rep.scored(), nil
			}
			with, err := runArm(ctx, trial, exercise, r, true, arms.Candidate)
			if err != nil {
				return Report{}, err
			}
			without, err := runArm(ctx, trial, exercise, r, false, arms.Baseline)
			if err != nil {
				return Report{}, err
			}
			rep.Pairs = append(rep.Pairs, Pair{
				Exercise: exercise, Repeat: r,
				With: with.Passed, Without: without.Passed,
				WithChecks: with.Checks, WithoutChecks: without.Checks,
				WithCost: with.Cost, WithoutCost: without.Cost,
				WithEnded: with.Ended, WithoutEnded: without.Ended,
			})
			spent = spent.Add(with.Cost).Add(without.Cost)
		}
	}
	return rep.scored(), nil
}

// runArm runs one trial and keeps only the checks the exercise declares, so a
// trial that forgot one is caught here, before a missing result reads as a failure.
func runArm(ctx context.Context, trial Trial, e Exercise, repeat int, candidate bool, arm string) (Outcome, error) {
	out, err := trial(ctx, e, repeat, candidate)
	if err != nil {
		return Outcome{}, fmt.Errorf("skillab: %s repeat %d %s: %w", e.Name(), repeat, arm, err)
	}
	kept := make(map[string]bool, len(e.Checks))
	for _, c := range e.Checks {
		v, ok := out.Checks[c.Name]
		if !ok {
			return Outcome{}, fmt.Errorf("skillab: %s repeat %d %s: no result for check %q", e.Name(), repeat, arm, c.Name)
		}
		kept[c.Name] = v
	}
	out.Checks = kept
	return out, nil
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
	// Arms names the candidate and the baseline. The zero value reads as a skill
	// against its absence.
	Arms  Arms
	Pairs []Pair

	// Gain is the pass-rate difference in percentage points, the effect size.
	WithPasses    int
	WithoutPasses int
	Gain          float64

	// HelpedOnly and HurtOnly count the discordant pairs, the only ones the test reads.
	HelpedOnly int
	HurtOnly   int

	// P is the two-sided exact McNemar probability.
	P float64

	// Verdict is about the primary check alone, at the full [Alpha], so declaring
	// secondary checks cannot move it.
	Verdict Verdict

	// Checks is one result per declared secondary check, by name.
	Checks []CheckResult

	// CandidateCost and BaselineCost total what each arm spent over every pair. The
	// report states them and gives no verdict on them: what a difference in cost is
	// worth is a judgement for whoever reads it.
	CandidateCost Cost
	BaselineCost  Cost

	// CandidateEnds and BaselineEnds count each arm's attempts that stopped without
	// finishing, by how they stopped.
	CandidateEnds map[Ending]int
	BaselineEnds  map[Ending]int

	// Planned is the number of pairs the comparison set out to run. Stopped is empty
	// when all of them ran, and otherwise says why it ended early; the verdict then
	// reads only the pairs that ran.
	Planned int
	Stopped string
}

// CheckResult is the paired comparison of one named secondary check over the pairs
// whose exercise declares it.
type CheckResult struct {
	Name          string
	Pairs         int
	WithPasses    int
	WithoutPasses int
	HelpedOnly    int
	HurtOnly      int
	// P is the exact McNemar probability for this check alone.
	P float64
	// Adjusted is P after the Holm step-down correction over every declared
	// secondary check, which is what the verdict reads. Each check is a chance to
	// find a difference by luck, and the correction pays for the number of chances.
	Adjusted float64
	Verdict  Verdict
}

// Metered reports whether any attempt recorded a cost.
func (r Report) Metered() bool {
	return r.CandidateCost.Turns > 0 || r.BaselineCost.Turns > 0
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
		r.CandidateCost = r.CandidateCost.Add(p.WithCost)
		r.BaselineCost = r.BaselineCost.Add(p.WithoutCost)
		r.CandidateEnds = countEnd(r.CandidateEnds, p.WithEnded)
		r.BaselineEnds = countEnd(r.BaselineEnds, p.WithoutEnded)
	}
	if n := len(r.Pairs); n > 0 {
		r.Gain = 100 * (float64(r.WithPasses) - float64(r.WithoutPasses)) / float64(n)
	}
	r.P = mcnemar(r.HelpedOnly, r.HurtOnly)
	r.Verdict = verdict(r.P, r.HelpedOnly, r.HurtOnly)
	r.Checks = scoreChecks(r.Pairs)
	return r
}

func countEnd(m map[Ending]int, e Ending) map[Ending]int {
	if e == Finished {
		return m
	}
	if m == nil {
		m = map[Ending]int{}
	}
	m[e]++
	return m
}

func verdict(p float64, helped, hurt int) Verdict {
	switch {
	case p > Alpha || helped == hurt:
		return NoDifference
	case helped > hurt:
		return Helped
	default:
		return Hurt
	}
}

// scoreChecks tallies each declared secondary check over the pairs that declare it.
// The correction counts the checks declared, not the ones that happened to
// disagree, so a check that never separated the arms still counts against the rest.
func scoreChecks(pairs []Pair) []CheckResult {
	index := map[string]int{}
	var out []CheckResult
	for _, p := range pairs {
		for _, c := range p.Exercise.Checks {
			i, ok := index[c.Name]
			if !ok {
				i = len(out)
				index[c.Name] = i
				out = append(out, CheckResult{Name: c.Name})
			}
			with, without := p.WithChecks[c.Name], p.WithoutChecks[c.Name]
			res := &out[i]
			res.Pairs++
			if with {
				res.WithPasses++
			}
			if without {
				res.WithoutPasses++
			}
			switch {
			case with && !without:
				res.HelpedOnly++
			case !with && without:
				res.HurtOnly++
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	raw := make([]float64, len(out))
	for i := range out {
		out[i].P = mcnemar(out[i].HelpedOnly, out[i].HurtOnly)
		raw[i] = out[i].P
	}
	adj := Holm(raw, len(out))
	for i := range out {
		out[i].Adjusted = adj[i]
		out[i].Verdict = verdict(adj[i], out[i].HelpedOnly, out[i].HurtOnly)
	}
	return out
}

// Holm applies the Holm step-down correction to ps, returned in the order given.
// With the probabilities sorted ascending, the i-th (from 1) is multiplied by
// m-i+1 and then raised to the largest value before it, so the adjusted values
// never fall as the raw ones rise, and each is capped at 1. m is the number of
// tests declared, which may exceed len(ps).
func Holm(ps []float64, m int) []float64 {
	order := make([]int, len(ps))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return ps[order[a]] < ps[order[b]] })
	out := make([]float64, len(ps))
	running := 0.0
	for rank, idx := range order {
		adj := math.Min(1, float64(m-rank)*ps[idx])
		running = math.Max(running, adj)
		out[idx] = running
	}
	return out
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
// exercises shows a different verdict. Its checks are corrected over the checks the
// held-out exercises declare, not over the whole set.
func (r Report) Holdout() Report {
	out := Report{Skill: r.Skill, Repeats: r.Repeats, Arms: r.Arms}
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
	arms := r.Arms
	if arms == (Arms{}) {
		arms = withAndWithout
	}
	fmt.Fprintf(&b, "%s: %s\n", r.Skill, r.Verdict)
	if arms != withAndWithout {
		fmt.Fprintf(&b, "  %s against %s\n", arms.Candidate, arms.Baseline)
	}
	fmt.Fprintf(&b, "  %d paired runs (%d exercises x %d repeats)\n", n, exerciseCount(r.Pairs), r.Repeats)
	fmt.Fprintf(&b, "  passed %s %d/%d, %s %d/%d (%+.1f points)\n",
		arms.Candidate, r.WithPasses, n, arms.Baseline, r.WithoutPasses, n, r.Gain)
	fmt.Fprintf(&b, "  disagreed on %d pairs: %d only %s, %d only %s (p=%.3f)\n",
		r.HelpedOnly+r.HurtOnly, r.HelpedOnly, arms.Candidate, r.HurtOnly, arms.Baseline, r.P)
	switch {
	case r.AllPass():
		b.WriteString("  every run passed in both conditions: the exercises are too easy to measure anything\n")
	case r.AllFail():
		b.WriteString("  no run passed in either condition: the exercises are out of reach, so nothing was measured\n")
	case r.Uninformative():
		b.WriteString("  the two conditions never disagreed: this exercise set cannot tell them apart\n")
	}
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "  check %s: %s, %d/%d vs %d/%d, disagreed on %d (p=%.3f, adjusted %.3f)\n",
			c.Name, c.Verdict, c.WithPasses, c.Pairs, c.WithoutPasses, c.Pairs, c.HelpedOnly+c.HurtOnly, c.P, c.Adjusted)
	}
	if r.Metered() && n > 0 {
		fmt.Fprintf(&b, "  cost per attempt, %s: %s\n", arms.Candidate, r.CandidateCost.perAttempt(n))
		fmt.Fprintf(&b, "  cost per attempt, %s: %s\n", arms.Baseline, r.BaselineCost.perAttempt(n))
		fmt.Fprintf(&b, "  cost in total, %s: %s\n", arms.Candidate, r.CandidateCost.total())
		fmt.Fprintf(&b, "  cost in total, %s: %s\n", arms.Baseline, r.BaselineCost.total())
		if d, ok := r.costDifference(); ok {
			fmt.Fprintf(&b, "  cost difference: %s\n", d)
		}
	}
	for _, e := range []struct {
		arm  string
		ends map[Ending]int
	}{{arms.Candidate, r.CandidateEnds}, {arms.Baseline, r.BaselineEnds}} {
		if s := endsLine(e.ends); s != "" {
			fmt.Fprintf(&b, "  runs that did not finish, %s: %s\n", e.arm, s)
		}
	}
	if r.Stopped != "" {
		fmt.Fprintf(&b, "  STOPPED EARLY after %d of %d pairs: %s; the verdict reads only the pairs that ran\n", n, r.Planned, r.Stopped)
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

// perAttempt renders c averaged over n attempts.
func (c Cost) perAttempt(n int) string {
	f := float64(n)
	return fmt.Sprintf("%.0f input + %.0f output tokens (%.0f read from cache), %.1f turns, %.1fs, %s",
		float64(c.Tokens.Input.N)/f, float64(c.Tokens.Output.N)/f, float64(c.Tokens.CacheRead.N)/f,
		float64(c.Turns)/f, c.Wall.Seconds()/f, c.money(n))
}

// String renders c as one attempt's cost: tokens, turns, time and money.
func (c Cost) String() string { return c.perAttempt(1) }

// total renders c as spent in all.
func (c Cost) total() string {
	return fmt.Sprintf("%d tokens, %d turns, %s", c.Tokens.Total(), c.Turns, c.money(1))
}

// money renders the spend averaged over n attempts, or says it is unknown. A price is
// only ever looked up, so a cost the table could not give is stated as unknown and
// never as zero.
func (c Cost) money(n int) string {
	if !c.Known() {
		if c.Unpriced > 0 {
			return fmt.Sprintf("cost unknown (%d of %d turns had no price)", c.Unpriced, c.Turns)
		}
		return "cost unknown (no price table)"
	}
	return fmt.Sprintf("%s %s", usage.Nanos(int64(c.Money)/int64(n)), c.Currency)
}

// costDifference is the candidate's spend minus the baseline's, when both are known.
func (r Report) costDifference() (string, bool) {
	c, b := r.CandidateCost, r.BaselineCost
	if !c.Known() || !b.Known() || c.Currency != b.Currency {
		return "", false
	}
	d := int64(c.Money) - int64(b.Money)
	sign := "+"
	if d < 0 {
		sign, d = "-", -d
	}
	return fmt.Sprintf("%s%s %s for the candidate (%+d tokens)", sign, usage.Nanos(d), c.Currency, c.Tokens.Total()-b.Tokens.Total()), true
}

func endsLine(ends map[Ending]int) string {
	var parts []string
	for _, e := range []Ending{Stalled, BudgetExhausted, Aborted} {
		if ends[e] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", ends[e], e))
		}
	}
	return strings.Join(parts, ", ")
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
