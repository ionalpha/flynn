package skillab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/ionalpha/flynn/usage"
)

// Board is a scoreboard: the same exercises run by several agents, each attempt graded
// by the exercise's own verifier and nothing else. It is the machine-readable record;
// [Board.Markdown] renders the tables a person reads. An arm that could not run, and
// an attempt that could not be evaluated, are listed with the reason and never counted
// as failures: a missing CLI or an empty account says nothing about the agent.
type Board struct {
	Generated time.Time `json:"generated"`
	Prices    string    `json:"prices,omitempty"`
	// Stopped says why the board ended before every planned attempt ran, such as a
	// spend limit; empty means it ran them all.
	Stopped string      `json:"stopped,omitempty"`
	Planned int         `json:"planned"`
	Arms    []ArmStatus `json:"arms"`
	Runs    []BoardRun  `json:"runs"`
}

// BoardEntry is one exercise a board runs, with the set that can materialise it.
type BoardEntry struct {
	Set      Set
	Exercise Exercise
}

// Key names the entry as skill/fixture, or skill/line-N for an exercise with no fixture.
func (e BoardEntry) Key() (skill, fixture string) {
	if e.Exercise.Fixture != "" {
		return e.Set.Skill, e.Exercise.Fixture
	}
	return e.Set.Skill, fmt.Sprintf("line-%d", e.Exercise.Line)
}

// BoardTrial runs one arm once on one entry. It returns the attempt's row and what it
// spent; an error ends the board, so a trial returns one only when no further attempt
// could be trusted, such as a cancelled context.
type BoardTrial func(ctx context.Context, entry BoardEntry, arm string, repeat int) (BoardRun, Cost, error)

// RunBoard runs every entry repeats times (at least once) in every arm. All arms run an
// entry's repeat before the next repeat starts, so drift in the model, the machine or the
// network reaches each arm alike. An attempt done reports true for is not run again; a
// nil done runs them all. limit is checked before every attempt, so the board can pass it
// by at most one attempt; the reason it stopped is returned with the rows that ran.
func RunBoard(ctx context.Context, entries []BoardEntry, arms []string, repeats int, limit Limit, done func(BoardEntry, string, int) bool, trial BoardTrial) ([]BoardRun, string, error) {
	if repeats < 1 {
		repeats = 1
	}
	var runs []BoardRun
	var spent Cost
	for _, e := range entries {
		for r := 1; r <= repeats; r++ {
			for _, arm := range arms {
				if done != nil && done(e, arm, r) {
					continue
				}
				if why := limit.reached(spent); why != "" {
					return runs, why, nil
				}
				if err := ctx.Err(); err != nil {
					return runs, "", err
				}
				run, cost, err := trial(ctx, e, arm, r)
				if err != nil {
					return runs, "", err
				}
				spent = spent.Add(cost)
				runs = append(runs, run)
			}
		}
	}
	return runs, "", nil
}

// ArmStatus is one competitor and whether it ran. Note says how its figures were
// obtained when they are less direct than another arm's, so a reader can weigh them.
type ArmStatus struct {
	Name    string `json:"name"`
	Model   string `json:"model,omitempty"`
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Note    string `json:"note,omitempty"`
}

// BoardRun is one attempt. Turns counts model calls that returned, and FirstUseful is
// the turn whose response first wrote or edited a file, zero when none did. USD is
// priced from the reported tokens by the board's table and is meaningful only when
// Priced is true; ReportedUSD is what the agent said it cost, kept as a cross-check
// and never used in a total. Unevaluated says why the attempt measured nothing about
// the agent, such as a provider that refused to serve it; such a row is neither solved
// nor unsolved, though what it spent still counts.
type BoardRun struct {
	Skill       string       `json:"skill"`
	Fixture     string       `json:"fixture"`
	Arm         string       `json:"arm"`
	Repeat      int          `json:"repeat"`
	Solved      bool         `json:"solved"`
	Unevaluated string       `json:"unevaluated,omitempty"`
	Ended       Ending       `json:"ended,omitempty"`
	Error       string       `json:"error,omitempty"`
	Tokens      usage.Tokens `json:"tokens"`
	USD         usage.Nanos  `json:"usd"`
	Priced      bool         `json:"priced"`
	ReportedUSD string       `json:"reportedUsd,omitempty"`
	WallSeconds float64      `json:"wallSeconds"`
	Turns       int          `json:"turns"`
	FirstUseful int          `json:"firstUseful"`
}

// Exercise names the attempt's exercise as skill/fixture, the form a board is
// filtered by.
func (r BoardRun) Exercise() string { return r.Skill + "/" + r.Fixture }

// WriteJSON writes b indented, so a diff of two boards reads line by line.
func (b Board) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(b)
}

// ReadBoard reads what [Board.WriteJSON] wrote.
func ReadBoard(r io.Reader) (Board, error) {
	var b Board
	if err := json.NewDecoder(r).Decode(&b); err != nil {
		return Board{}, fmt.Errorf("skillab: read board: %w", err)
	}
	return b, nil
}

// Spread is the median and range of one figure over a group of attempts. N is how
// many attempts had the figure; zero means none did and the other fields are unset.
type Spread struct {
	N              int
	Median, Lo, Hi float64
}

func spreadOf(xs []float64) Spread {
	if len(xs) == 0 {
		return Spread{}
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	med := s[len(s)/2]
	if len(s)%2 == 0 {
		med = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return Spread{N: len(s), Median: med, Lo: s[0], Hi: s[len(s)-1]}
}

// format prints the median with the range beside it once the attempts disagree.
func (s Spread) format(unit string, decimals int) string {
	if s.N == 0 {
		return "n/a"
	}
	f := func(x float64) string { return fmt.Sprintf("%.*f", decimals, x) }
	if s.Lo == s.Hi {
		return f(s.Median) + unit
	}
	return fmt.Sprintf("%s%s (%s to %s)", f(s.Median), unit, f(s.Lo), f(s.Hi))
}

// Tally is what a group of attempts came to: one arm overall, or one arm on one
// exercise. USD sums only priced attempts, and Unpriced counts the rest, so a total
// with any unpriced attempt is shown as a lower bound and never as the whole cost.
// Runs counts the attempts that were evaluated; Unevaluated counts the others, whose
// spend is in USD and which are in no other figure. Why is the first one's reason.
type Tally struct {
	Arm, Exercise string
	Runs, Solved  int
	Unevaluated   int
	Why           string
	USD           usage.Nanos
	Unpriced      int
	Tokens        usage.Tokens
	Wall          Spread
	Turns         Spread
	FirstUseful   Spread
	RunUSD        Spread
}

// USDPerSolved is the priced spend divided by the attempts solved, and false when
// nothing was solved, or some attempt was unpriced or not evaluated.
func (t Tally) USDPerSolved() (float64, bool) {
	if t.Solved == 0 || t.Unpriced > 0 || t.Unevaluated > 0 {
		return 0, false
	}
	return dollars(t.USD) / float64(t.Solved), true
}

func dollars(n usage.Nanos) float64 { return float64(n) / 1e9 }

func tally(arm, exercise string, runs []BoardRun) Tally {
	t := Tally{Arm: arm, Exercise: exercise}
	var wall, turns, first, cost []float64
	for _, r := range runs {
		if r.Priced {
			t.USD += r.USD
		} else {
			t.Unpriced++
		}
		if r.Unevaluated != "" {
			if t.Unevaluated == 0 {
				t.Why = r.Unevaluated
			}
			t.Unevaluated++
			continue
		}
		t.Runs++
		if r.Solved {
			t.Solved++
		}
		if r.Priced {
			cost = append(cost, dollars(r.USD))
		}
		t.Tokens = t.Tokens.Add(r.Tokens)
		wall = append(wall, r.WallSeconds)
		turns = append(turns, float64(r.Turns))
		if r.FirstUseful > 0 {
			first = append(first, float64(r.FirstUseful))
		}
	}
	t.Wall, t.Turns, t.FirstUseful, t.RunUSD = spreadOf(wall), spreadOf(turns), spreadOf(first), spreadOf(cost)
	return t
}

// ByArm tallies every attempt of each arm that ran, in the board's arm order.
func (b Board) ByArm() []Tally {
	var out []Tally
	for _, a := range b.Arms {
		if a.Skipped {
			continue
		}
		var runs []BoardRun
		for _, r := range b.Runs {
			if r.Arm == a.Name {
				runs = append(runs, r)
			}
		}
		out = append(out, tally(a.Name, "", runs))
	}
	return out
}

// ByExercise tallies each exercise for each arm that ran, exercises in the order the
// board first met them, so the table reads in the order the runs were made.
func (b Board) ByExercise() []Tally {
	var order []string
	seen := map[string]bool{}
	for _, r := range b.Runs {
		if !seen[r.Exercise()] {
			seen[r.Exercise()] = true
			order = append(order, r.Exercise())
		}
	}
	var out []Tally
	for _, ex := range order {
		for _, a := range b.Arms {
			if a.Skipped {
				continue
			}
			var runs []BoardRun
			for _, r := range b.Runs {
				if r.Arm == a.Name && r.Exercise() == ex {
					runs = append(runs, r)
				}
			}
			if len(runs) > 0 {
				out = append(out, tally(a.Name, ex, runs))
			}
		}
	}
	return out
}

// usdCell prints a priced total, marked as a lower bound when an attempt in it had no price.
func usdCell(t Tally) string {
	s := fmt.Sprintf("%.4f", dollars(t.USD))
	if t.Unpriced > 0 {
		s = fmt.Sprintf(">= %s (%d unpriced)", s, t.Unpriced)
	}
	return s
}

// Markdown writes the board as the tables docs/scoreboard.md carries: the arms and why
// any were skipped, a summary per arm, then each exercise per arm.
func (b Board) Markdown(w io.Writer) error {
	var s strings.Builder
	fmt.Fprintf(&s, "Generated %s.", b.Generated.UTC().Format(time.DateOnly))
	if b.Prices != "" {
		fmt.Fprintf(&s, " Cost is priced from reported tokens by `%s`.", b.Prices)
	}
	fmt.Fprintf(&s, " %d of %d planned attempts ran.", len(b.Runs), b.Planned)
	unevaluated := map[string]Tally{}
	total := 0
	for _, t := range b.ByArm() {
		if t.Unevaluated > 0 {
			unevaluated[t.Arm] = t
			total += t.Unevaluated
		}
	}
	if total > 0 {
		fmt.Fprintf(&s, " %d of those could not be evaluated.", total)
	}
	if b.Stopped != "" {
		fmt.Fprintf(&s, " The board stopped early: %s.", b.Stopped)
	}
	s.WriteString("\n\n| Arm | Model | Ran | Notes |\n|---|---|---|---|\n")
	for _, a := range b.Arms {
		ran, note := "yes", a.Note
		if a.Skipped {
			ran, note = "skipped", a.Reason
		}
		if t, ok := unevaluated[a.Name]; ok {
			ran = "not measured"
			note = fmt.Sprintf("%d attempt(s) could not be evaluated: %s", t.Unevaluated, t.Why)
		}
		fmt.Fprintf(&s, "| %s | %s | %s | %s |\n", a.Name, cell(a.Model), ran, cell(note))
	}

	s.WriteString("\n### By arm\n\n")
	s.WriteString("| Arm | Solved | USD total | USD per solve | Wall s | Turns | Turns to first edit | Tokens in / out / cache read / cache write |\n")
	s.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, t := range b.ByArm() {
		// An arm with a hole in it is not compared: its totals would cover different
		// exercises from every other arm's.
		if t.Unevaluated > 0 {
			fmt.Fprintf(&s, "| %s | not measured | %s | n/a | n/a | n/a | n/a | n/a |\n", t.Arm, usdCell(t))
			continue
		}
		per := "n/a"
		if v, ok := t.USDPerSolved(); ok {
			per = fmt.Sprintf("%.4f", v)
		}
		fmt.Fprintf(&s, "| %s | %d/%d | %s | %s | %s | %s | %s | %d / %d / %d / %d |\n",
			t.Arm, t.Solved, t.Runs, usdCell(t), per,
			t.Wall.format("", 0), t.Turns.format("", 0), t.FirstUseful.format("", 0),
			t.Tokens.Input.N, t.Tokens.Output.N, t.Tokens.CacheRead.N, t.Tokens.CacheWrite.N)
	}

	s.WriteString("\n### By exercise\n\n")
	s.WriteString("| Exercise | Arm | Solved | USD per run | Wall s | Turns | Turns to first edit |\n")
	s.WriteString("|---|---|---|---|---|---|---|\n")
	for _, t := range b.ByExercise() {
		solved := fmt.Sprintf("%d/%d", t.Solved, t.Runs)
		switch {
		case t.Runs == 0:
			solved = "could not evaluate"
		case t.Unevaluated > 0:
			solved += fmt.Sprintf(" (%d could not be evaluated)", t.Unevaluated)
		}
		fmt.Fprintf(&s, "| %s | %s | %s | %s | %s | %s | %s |\n",
			t.Exercise, t.Arm, solved, t.RunUSD.format("", 4),
			t.Wall.format("", 0), t.Turns.format("", 0), t.FirstUseful.format("", 0))
	}
	s.WriteString("\nMedians, with the range in brackets when the repeats disagree. Turns to first edit counts only the runs that edited a file. An attempt that could not be evaluated is in no figure but the spend.\n")
	_, err := io.WriteString(w, s.String())
	return err
}

// cell keeps a value from breaking the table it sits in.
func cell(s string) string {
	if s == "" {
		return "-"
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", "/"), "\n", " ")
}
