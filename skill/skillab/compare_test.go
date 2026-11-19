package skillab_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/skill/skillab"
)

// TestCompareRunsBothNamedArmsOfEveryPair pins the order of a comparison between two
// versions: the candidate runs first and the baseline second, for every pair, and the
// report names both.
func TestCompareRunsBothNamedArmsOfEveryPair(t *testing.T) {
	var order []string
	trial := func(_ context.Context, e skillab.Exercise, repeat int, candidate bool) (skillab.Outcome, error) {
		order = append(order, fmt.Sprintf("%s/%d/%v", e.Objective, repeat, candidate))
		return skillab.Outcome{Passed: true}, nil
	}
	arms := skillab.Arms{Candidate: "v2", Baseline: "v1"}
	rep, err := skillab.Compare(context.Background(), set(2), 2, arms, trial)
	if err != nil {
		t.Fatal(err)
	}
	want := "exercise 0/1/true,exercise 0/1/false,exercise 0/2/true,exercise 0/2/false," +
		"exercise 1/1/true,exercise 1/1/false,exercise 1/2/true,exercise 1/2/false"
	if got := strings.Join(order, ","); got != want {
		t.Errorf("trial order:\n got %s\nwant %s", got, want)
	}
	if rep.Arms != arms {
		t.Errorf("report arms = %+v, want %+v", rep.Arms, arms)
	}
	if out := rep.String(); !strings.Contains(out, "v2") || !strings.Contains(out, "v1") {
		t.Errorf("the report does not name both versions:\n%s", out)
	}
}

// A failed trial names the version that could not run, so the repair goes to the right one.
func TestCompareNamesTheArmThatFailed(t *testing.T) {
	arms := skillab.Arms{Candidate: "v2", Baseline: "v1"}
	boom := errors.New("would not start")
	_, err := skillab.Compare(context.Background(), set(1), 1, arms,
		func(_ context.Context, _ skillab.Exercise, _ int, candidate bool) (skillab.Outcome, error) {
			if !candidate {
				return skillab.Outcome{}, boom
			}
			return skillab.Outcome{Passed: true}, nil
		})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "v1") {
		t.Fatalf("err = %v, want it to wrap the trial's error and name v1", err)
	}
}

func TestHolmAdjustsStepDown(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []float64
		m    int
		want []float64
	}{
		{"three checks", []float64{0.01, 0.04, 0.03}, 3, []float64{0.03, 0.06, 0.06}},
		{"one check is unchanged", []float64{0.04}, 1, []float64{0.04}},
		{"capped at one", []float64{0.6, 0.9}, 2, []float64{1, 1}},
		{"fewer tested than declared", []float64{0.02}, 3, []float64{0.06}},
		{"monotone through a later small value", []float64{0.001, 0.2, 0.2}, 3, []float64{0.003, 0.4, 0.4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := skillab.Holm(tc.in, tc.m)
			for i := range tc.want {
				if math.Abs(got[i]-tc.want[i]) > 1e-9 {
					t.Errorf("Holm(%v, %d) = %v, want %v", tc.in, tc.m, got, tc.want)
					break
				}
			}
		})
	}
}

// checked builds a set whose exercises each declare the named secondary checks.
func checked(n int, names ...string) skillab.Set {
	s := set(n)
	for i := range s.Exercises {
		for _, name := range names {
			s.Exercises[i].Checks = append(s.Exercises[i].Checks, skillab.Check{Name: name, Command: "exit 0"})
		}
	}
	return s
}

// A fixture where one arm fails only the held-out check gets a verdict on that check
// while the primary verdict stays where it was: the run passed verify in both arms and
// one of them broke something verify does not look at.
func TestOneCheckCanSeparateTheArmsWhileVerifyDoesNot(t *testing.T) {
	trial := func(_ context.Context, _ skillab.Exercise, _ int, candidate bool) (skillab.Outcome, error) {
		return skillab.Outcome{Passed: true, Checks: map[string]bool{"heldout": !candidate, "scope": true}}, nil
	}
	rep, err := skillab.Compare(context.Background(), checked(8, "heldout", "scope"), 1, skillab.Arms{Candidate: "v2", Baseline: "v1"}, trial)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != skillab.NoDifference {
		t.Errorf("primary verdict = %q, want it unchanged by the secondary checks", rep.Verdict)
	}
	byName := map[string]skillab.CheckResult{}
	for _, c := range rep.Checks {
		byName[c.Name] = c
	}
	if got := byName["heldout"]; got.Verdict != skillab.Hurt {
		t.Errorf("heldout verdict = %q (adjusted p=%.4f), want %q", got.Verdict, got.Adjusted, skillab.Hurt)
	}
	if got := byName["scope"]; got.Verdict != skillab.NoDifference {
		t.Errorf("scope verdict = %q, want %q", got.Verdict, skillab.NoDifference)
	}
	if !strings.Contains(rep.String(), "heldout") {
		t.Errorf("the report does not show the check:\n%s", rep)
	}
}

// Six one-sided pairs clear alpha for one check. With three declared checks the same
// count no longer does: the correction is for how many were looked at, and a result
// that survives only because the others were ignored is the overclaim it exists to stop.
func TestSecondaryChecksAreCorrectedForHowManyWereDeclared(t *testing.T) {
	trial := func(_ context.Context, _ skillab.Exercise, _ int, candidate bool) (skillab.Outcome, error) {
		return skillab.Outcome{Passed: true, Checks: map[string]bool{"a": candidate, "b": true, "c": true}}, nil
	}
	arms := skillab.Arms{Candidate: "v2", Baseline: "v1"}
	one, err := skillab.Compare(context.Background(), checked(6, "a"), 1, arms, trial)
	if err != nil {
		t.Fatal(err)
	}
	if v := one.Checks[0].Verdict; v != skillab.Helped {
		t.Fatalf("a alone: verdict %q (adjusted p=%.4f), want helped", v, one.Checks[0].Adjusted)
	}
	three, err := skillab.Compare(context.Background(), checked(6, "a", "b", "c"), 1, arms, trial)
	if err != nil {
		t.Fatal(err)
	}
	a := three.Checks[0]
	if a.Name != "a" || a.P >= skillab.Alpha {
		t.Fatalf("raw p for a = %.4f, want it under alpha", a.P)
	}
	if a.Verdict != skillab.NoDifference || a.Adjusted < skillab.Alpha {
		t.Errorf("a among three: verdict %q adjusted p=%.4f, want no difference once corrected", a.Verdict, a.Adjusted)
	}
}

// Declaring secondary checks cannot change the verdict on the primary check, which is
// tested at the full level on its own.
func TestSecondaryChecksDoNotMoveThePrimaryVerdict(t *testing.T) {
	arms := skillab.Arms{Candidate: "v2", Baseline: "v1"}
	mk := func(s skillab.Set) skillab.Report {
		rep, err := skillab.Compare(context.Background(), s, 1, arms,
			func(_ context.Context, _ skillab.Exercise, _ int, candidate bool) (skillab.Outcome, error) {
				return skillab.Outcome{Passed: candidate, Checks: map[string]bool{"a": true, "b": true, "c": true}}, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	bare, withChecks := mk(set(6)), mk(checked(6, "a", "b", "c"))
	if bare.Verdict != skillab.Helped || withChecks.Verdict != bare.Verdict || withChecks.P != bare.P {
		t.Errorf("verdict %q p=%.4f with checks, %q p=%.4f without", withChecks.Verdict, withChecks.P, bare.Verdict, bare.P)
	}
}

// A check an exercise does not declare says nothing about it, so a pair from that
// exercise is not counted for the check.
func TestACheckIsTestedOnlyOnTheExercisesThatDeclareIt(t *testing.T) {
	s := set(3)
	s.Exercises[0].Checks = []skillab.Check{{Name: "scope", Command: "exit 0"}}
	rep, err := skillab.Compare(context.Background(), s, 2, skillab.Arms{Candidate: "v2", Baseline: "v1"},
		func(_ context.Context, _ skillab.Exercise, _ int, _ bool) (skillab.Outcome, error) {
			return skillab.Outcome{Passed: true, Checks: map[string]bool{"scope": true}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Checks) != 1 || rep.Checks[0].Pairs != 2 {
		t.Errorf("checks = %+v, want scope over the 2 pairs of the one exercise that declares it", rep.Checks)
	}
}

// Cost is summed per arm and reported per attempt, with no verdict attached: what a
// difference in cost is worth is a judgement the harness does not make.
func TestCostIsTotalledPerArm(t *testing.T) {
	trial := func(_ context.Context, _ skillab.Exercise, _ int, candidate bool) (skillab.Outcome, error) {
		c := skillab.Cost{InputTokens: 100, OutputTokens: 10, Turns: 2, Wall: time.Second}
		if candidate {
			c = skillab.Cost{InputTokens: 300, OutputTokens: 30, Turns: 4, Wall: 3 * time.Second}
		}
		return skillab.Outcome{Passed: true, Cost: c}, nil
	}
	rep, err := skillab.Compare(context.Background(), set(2), 2, skillab.Arms{Candidate: "v2", Baseline: "v1"}, trial)
	if err != nil {
		t.Fatal(err)
	}
	if rep.CandidateCost.InputTokens != 1200 || rep.BaselineCost.InputTokens != 400 {
		t.Errorf("input tokens %d/%d, want 1200/400", rep.CandidateCost.InputTokens, rep.BaselineCost.InputTokens)
	}
	if rep.CandidateCost.Turns != 16 || rep.BaselineCost.Wall != 4*time.Second {
		t.Errorf("turns %d wall %v", rep.CandidateCost.Turns, rep.BaselineCost.Wall)
	}
	out := rep.String()
	for _, want := range []string{"cost per attempt", "300", "100"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}

// A run that reported no cost prints no cost line, rather than a column of zeros that
// reads as a free skill.
func TestNoCostLineWhenNothingWasMetered(t *testing.T) {
	attempt, _ := scripted(true)
	rep, err := skillab.Measure(context.Background(), set(2), 1, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rep.String(), "cost per attempt") {
		t.Errorf("a measurement without cost printed a cost line:\n%s", rep)
	}
}

// The holdout rescoring carries the checks and the cost of its own pairs.
func TestHoldoutKeepsItsOwnChecksAndCost(t *testing.T) {
	s := checked(2, "scope")
	s.Exercises = append(s.Exercises, skillab.Exercise{
		Objective: "held", Verify: "exit 0", Holdout: true,
		Checks: []skillab.Check{{Name: "scope", Command: "exit 0"}},
	})
	rep, err := skillab.Compare(context.Background(), s, 1, skillab.Arms{Candidate: "v2", Baseline: "v1"},
		func(_ context.Context, _ skillab.Exercise, _ int, _ bool) (skillab.Outcome, error) {
			return skillab.Outcome{Passed: true, Checks: map[string]bool{"scope": true}, Cost: skillab.Cost{Turns: 1}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	h := rep.Holdout()
	if len(h.Checks) != 1 || h.Checks[0].Pairs != 1 || h.CandidateCost.Turns != 1 {
		t.Errorf("holdout checks %+v cost %+v, want one pair", h.Checks, h.CandidateCost)
	}
}
