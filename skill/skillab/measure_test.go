package skillab_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/skill/skillab"
)

// set builds a set of n open exercises with a deterministic verifier apiece.
func set(n int, holdout ...string) skillab.Set {
	s := skillab.Set{Skill: "systematic-debugging"}
	for i := range n {
		s.Exercises = append(s.Exercises, skillab.Exercise{
			Objective: fmt.Sprintf("exercise %d", i),
			Verify:    "exit 0",
			Source:    skillab.ExercisesFile,
			Line:      i + 1,
		})
	}
	for i, obj := range holdout {
		s.Exercises = append(s.Exercises, skillab.Exercise{
			Objective: obj, Verify: "exit 0", Holdout: true, Source: skillab.HoldoutFile, Line: i + 1,
		})
	}
	return s
}

// scripted answers each attempt from a list of outcomes, in the order Measure asks:
// with the skill, then without, per repeat, per exercise.
func scripted(outcomes ...bool) (skillab.Attempt, *int) {
	i := 0
	return func(context.Context, skillab.Exercise, int, bool) (bool, error) {
		v := outcomes[i%len(outcomes)]
		i++
		return v, nil
	}, &i
}

// TestMeasureRunsBothArmsOfEveryPair pins the shape of the measurement: each exercise is
// attempted once per repeat in each condition, and both arms of a pair run before
// the next pair, so a model or a machine that drifts over a long measurement drifts
// through both arms rather than through one.
func TestMeasureRunsBothArmsOfEveryPair(t *testing.T) {
	var order []string
	attempt := func(_ context.Context, tk skillab.Exercise, repeat int, withSkill bool) (bool, error) {
		order = append(order, fmt.Sprintf("%s/%d/%v", tk.Objective, repeat, withSkill))
		return true, nil
	}
	rep, err := skillab.Measure(context.Background(), set(2), 2, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Pairs) != 4 {
		t.Fatalf("%d pairs, want 2 exercises x 2 repeats", len(rep.Pairs))
	}
	want := []string{
		"exercise 0/1/true", "exercise 0/1/false",
		"exercise 0/2/true", "exercise 0/2/false",
		"exercise 1/1/true", "exercise 1/1/false",
		"exercise 1/2/true", "exercise 1/2/false",
	}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("attempt order:\n got %v\nwant %v", order, want)
	}
}

// A skill that wins every pair it separates is called helped once there are enough
// of those pairs, and not before. Six one-sided disagreements is the point where a
// fair coin explains the split less than five per cent of the time; five is not, and
// reporting the smaller one as a result would be the overclaim this harness exists
// to stop.
func TestVerdictNeedsEnoughDisagreementToCallIt(t *testing.T) {
	for _, tc := range []struct {
		discordant int
		want       skillab.Verdict
	}{
		{4, skillab.NoDifference},
		{5, skillab.NoDifference},
		{6, skillab.Helped},
		{10, skillab.Helped},
	} {
		t.Run(strconv.Itoa(tc.discordant), func(t *testing.T) {
			// Every pair: passes with the skill, fails without it.
			attempt, _ := scripted(true, false)
			rep, err := skillab.Measure(context.Background(), set(tc.discordant), 1, attempt)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Verdict != tc.want {
				t.Errorf("%d one-sided pairs gave %q (p=%.4f), want %q", tc.discordant, rep.Verdict, rep.P, tc.want)
			}
			if rep.HelpedOnly != tc.discordant || rep.HurtOnly != 0 {
				t.Errorf("discordant tally = %d/%d", rep.HelpedOnly, rep.HurtOnly)
			}
			if rep.Gain != 100 {
				t.Errorf("gain = %.1f points, want 100", rep.Gain)
			}
		})
	}
}

// The verdict runs both ways. A skill can make a run worse by spending the model's
// attention on a procedure the exercise did not need, and a harness that can only report
// improvement is not measuring, it is confirming.
func TestASkillCanBeMeasuredAsHurting(t *testing.T) {
	attempt, _ := scripted(false, true)
	rep, err := skillab.Measure(context.Background(), set(8), 1, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != skillab.Hurt {
		t.Fatalf("verdict = %q (p=%.4f), want %q", rep.Verdict, rep.P, skillab.Hurt)
	}
	if rep.Gain != -100 {
		t.Errorf("gain = %.1f points, want -100", rep.Gain)
	}
}

// An exercise set where both arms always agree is reported as measuring nothing, whether
// it agreed by passing or by failing. This is the one result the harness must not
// dress up: a skill that "passed" a set of exercises it could not have failed has no
// evidence behind it at all.
func TestAnAgreeingExerciseSetIsReportedAsMeasuringNothing(t *testing.T) {
	for name, outcome := range map[string]bool{"every run passed": true, "no run passed": false} {
		t.Run(name, func(t *testing.T) {
			attempt, _ := scripted(outcome)
			rep, err := skillab.Measure(context.Background(), set(6), 2, attempt)
			if err != nil {
				t.Fatal(err)
			}
			if !rep.Uninformative() {
				t.Errorf("a set that never disagreed was not reported as uninformative")
			}
			if rep.Verdict != skillab.NoDifference {
				t.Errorf("verdict = %q, want %q", rep.Verdict, skillab.NoDifference)
			}
			if rep.AllPass() == rep.AllFail() {
				t.Errorf("the report does not say which way the set degenerated")
			}
			want := "too easy"
			if !outcome {
				want = "out of reach"
			}
			if !strings.Contains(rep.String(), want) {
				t.Errorf("the report does not say %q:\n%s", want, rep)
			}
		})
	}
}

// A skill that helps on its author's exercises and does nothing on the held-out ones has
// been fitted to its own eval. The two verdicts side by side are what makes that
// visible, so the holdout is rescored on its own rather than folded into the total.
func TestHoldoutIsScoredOnItsOwn(t *testing.T) {
	s := set(6, "held out one", "held out two")
	attempt := func(_ context.Context, tk skillab.Exercise, _ int, withSkill bool) (bool, error) {
		if tk.Holdout {
			// Both arms pass the held-out exercises: the skill changes nothing there.
			return true, nil
		}
		return withSkill, nil
	}
	rep, err := skillab.Measure(context.Background(), s, 1, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != skillab.Helped {
		t.Fatalf("overall verdict = %q, want %q", rep.Verdict, skillab.Helped)
	}
	held := rep.Holdout()
	if len(held.Pairs) != 2 {
		t.Fatalf("holdout has %d pairs, want 2", len(held.Pairs))
	}
	if held.Verdict != skillab.NoDifference || !held.Uninformative() {
		t.Errorf("holdout verdict = %q, want the skill to show nothing there", held.Verdict)
	}
}

// A harness failure stops the measurement rather than being counted as a failed run.
// The difference matters: a sandbox that would not start is not evidence about a
// skill, and averaging it in would quietly move the verdict.
func TestAnAttemptErrorStopsTheMeasurement(t *testing.T) {
	boom := errors.New("the sandbox would not start")
	attempt := func(_ context.Context, _ skillab.Exercise, _ int, withSkill bool) (bool, error) {
		if !withSkill {
			return false, boom
		}
		return true, nil
	}
	if _, err := skillab.Measure(context.Background(), set(3), 1, attempt); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the attempt's own error", err)
	}
}

// PerExercise is where an exercise that decides nothing shows up. A row that passes in both
// arms every time is not evidence about the skill, and an author reading only the
// total would keep it.
func TestPerExerciseNamesTheOnesThatDecidedNothing(t *testing.T) {
	s := set(2)
	attempt := func(_ context.Context, tk skillab.Exercise, _ int, withSkill bool) (bool, error) {
		if tk.Objective == "exercise 0" {
			return true, nil
		}
		return withSkill, nil
	}
	rep, err := skillab.Measure(context.Background(), s, 2, attempt)
	if err != nil {
		t.Fatal(err)
	}
	per := rep.PerExercise()
	if len(per) != 2 {
		t.Fatalf("%d exercise results, want 2", len(per))
	}
	if per[0].Decided() {
		t.Errorf("exercise 0 passed in both arms every time and was not flagged as deciding nothing")
	}
	if !per[1].Decided() {
		t.Errorf("exercise 1 separated the arms and was flagged as deciding nothing")
	}
	if per[1].WithPasses != 2 || per[1].WithoutPasses != 0 || per[1].Attempts != 2 {
		t.Errorf("exercise 1 tally = %+v", per[1])
	}
}

// Repeats below one are one run, not zero. A measurement that silently produced no
// pairs would report "no measurable difference" over nothing at all.
func TestRepeatsBelowOneStillRunOnce(t *testing.T) {
	attempt, calls := scripted(true)
	rep, err := skillab.Measure(context.Background(), set(2), 0, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Pairs) != 2 || *calls != 4 {
		t.Fatalf("%d pairs from %d attempts, want 2 pairs from 4 attempts", len(rep.Pairs), *calls)
	}
}

// A pair is discordant when the arms disagreed, and only those carry information
// about the difference between them. It is the report's own vocabulary, so a caller
// reading pairs back can ask the same question the verdict was built on.
func TestPairDiscordant(t *testing.T) {
	for _, tc := range []struct {
		with, without, want bool
	}{
		{true, false, true},
		{false, true, true},
		{true, true, false},
		{false, false, false},
	} {
		p := skillab.Pair{With: tc.with, Without: tc.without}
		if p.Discordant() != tc.want {
			t.Errorf("Pair{%v,%v}.Discordant() = %v, want %v", tc.with, tc.without, p.Discordant(), tc.want)
		}
	}
}

// A harness failure in the first arm stops the measurement too, and says which arm
// it was. The two messages differ because the repairs do: a run that cannot start
// with the skill is usually the skill's own resources, and one that cannot start
// without it is usually the pruning.
func TestAnErrorInEitherArmNamesTheArm(t *testing.T) {
	boom := errors.New("the sandbox would not start")
	_, err := skillab.Measure(context.Background(), set(2), 1, func(context.Context, skillab.Exercise, int, bool) (bool, error) {
		return false, boom
	})
	if err == nil || !strings.Contains(err.Error(), "with the skill") {
		t.Fatalf("err = %v, want it to name the arm that failed", err)
	}
}

// A set can agree on every pair without every run passing or every run failing: both
// arms pass some exercises, both fail others, and the skill made no difference to any of
// them. The report says so plainly, because a reader seeing a middling pass rate
// would otherwise take it for a measurement that worked.
func TestAMixedButAgreeingSetIsStillReportedAsMeasuringNothing(t *testing.T) {
	s := set(4, "held out")
	attempt := func(_ context.Context, tk skillab.Exercise, _ int, _ bool) (bool, error) {
		// The outcome depends on the exercise and never on the condition.
		return tk.Holdout || strings.HasSuffix(tk.Objective, "0") || strings.HasSuffix(tk.Objective, "2"), nil
	}
	rep, err := skillab.Measure(context.Background(), s, 1, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AllPass() || rep.AllFail() {
		t.Fatalf("the set is mixed, not degenerate one way: %d/%d passes", rep.WithPasses, rep.WithoutPasses)
	}
	out := rep.String()
	if !strings.Contains(out, "never disagreed") {
		t.Errorf("the report does not say the set could not tell the conditions apart:\n%s", out)
	}
	// The held-out rows are marked in the per-exercise listing, so a reader sees which
	// half an exercise came from without going back to the files.
	if !strings.Contains(out, "[holdout]") {
		t.Errorf("the per-exercise listing does not mark the held-out exercise:\n%s", out)
	}
}
