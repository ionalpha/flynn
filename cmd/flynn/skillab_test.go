package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/skill/skillab"
	"github.com/ionalpha/flynn/state"
)

// writeExerciseSet lays out one skill's exercise set the way an author keeps it: outside the
// pack, one directory per skill.
func writeExerciseSet(t *testing.T, slug, exercises string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, slug)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillab.ExercisesFile), []byte(exercises), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// The command refuses before it spends anything. A skill with no exercise set, and a
// call with no skill named, both stop at the argument check: resolving a model and
// opening a store first would charge for a run that was never going to happen.
func TestSkillABRefusesBeforeItSpends(t *testing.T) {
	var out bytes.Buffer
	if err := runSkillAB(nil, "", t.TempDir(), &out); err == nil {
		t.Fatal("a call naming no skill was accepted")
	}
	root := writeExerciseSet(t, "present", "do the thing | exit 0\n")
	err := runSkillAB([]string{"--exercises", root, "absent"}, "", t.TempDir(), &out)
	if err == nil {
		t.Fatal("a skill with no exercise set was accepted")
	}
	if !strings.Contains(err.Error(), "no exercise set") {
		t.Errorf("err = %v, want it to name the missing exercise set", err)
	}
}

// A trial is one arm of one pair, and the verifier is the only thing that decides
// it. The model here says it is done without doing anything, so the exercise's own
// command is what separates the pass from the failure.
func TestSkillABTrialIsDecidedByTheVerifier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for name, tc := range map[string]struct {
		verify string
		want   bool
	}{
		"the verifier passes": {"exit 0", true},
		"the verifier fails":  {"exit 1", false},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			model := llmtest.NewScripted(llmtest.SayText("done"))
			exercise := skillab.Exercise{Objective: "do the thing", Verify: tc.verify}
			got, err := runTrial(ctx, &out, model, harness.Plan{}, skillab.Set{Skill: "any-skill"}, exercise, 1, true)
			if err != nil {
				t.Fatalf("trial: %v", err)
			}
			if got != tc.want {
				t.Errorf("trial reported %v, want %v: the verifier is what decides a run", got, tc.want)
			}
			if !strings.Contains(out.String(), "[with, repeat 1]") {
				t.Errorf("the trial did not say which arm it ran:\n%s", out.String())
			}
		})
	}
}

// The arm without the skill has to actually be without the skill. Measuring the
// absence of something the library never held would compare two identical
// conditions and report "no measurable difference" about a skill that was never
// installed, which is the most misleading result this harness could produce.
func TestSkillABRefusesToMeasureTheAbsenceOfAnAbsentSkill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	model := llmtest.NewScripted(llmtest.SayText("done"))
	exercise := skillab.Exercise{Objective: "do the thing", Verify: "exit 0"}

	_, err := runTrial(ctx, &out, model, harness.Plan{}, skillab.Set{Skill: "never-shipped"}, exercise, 1, false)
	if err == nil {
		t.Fatal("the arm without a skill the library does not hold ran anyway")
	}
	if !strings.Contains(err.Error(), "nothing to measure the absence of") {
		t.Errorf("err = %v, want it to say the skill is not installed", err)
	}
}

// The report a reader sees carries the verdict, the tallies behind it, and the
// held-out half on its own line. The last of those is the point: a skill that helps
// on its author's exercises and does nothing on the exercises someone else wrote has been
// fitted to its own eval, and one averaged verdict would hide it.
func TestSkillABReportsTheHoldoutSeparately(t *testing.T) {
	set := skillab.Set{Skill: "systematic-debugging"}
	for i := range 6 {
		set.Exercises = append(set.Exercises, skillab.Exercise{Objective: fmt.Sprintf("open exercise %d", i), Verify: "exit 0"})
	}
	set.Exercises = append(set.Exercises, skillab.Exercise{Objective: "held out", Verify: "exit 0", Holdout: true})

	var out bytes.Buffer
	err := measureSkill(context.Background(), &out, set, 1, func(_ context.Context, tk skillab.Exercise, _ int, withSkill bool) (bool, error) {
		if tk.Holdout {
			return true, nil // the skill changes nothing on the held-out exercise
		}
		return withSkill, nil
	})
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"measuring systematic-debugging over 7 exercise(s) x 1 repeat(s), both conditions: 14 runs",
		"systematic-debugging: helped",
		"held-out exercises alone: no measurable difference",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report does not say %q:\n%s", want, got)
		}
	}
}

// A harness failure comes back as an error rather than a verdict. The report must
// never be printed over a measurement that did not finish: a partial tally reads
// exactly like a complete one.
func TestSkillABReportsNothingWhenTheMeasurementFails(t *testing.T) {
	set := skillab.Set{Skill: "x", Exercises: []skillab.Exercise{{Objective: "do the thing", Verify: "exit 0"}}}
	var out bytes.Buffer
	err := measureSkill(context.Background(), &out, set, 0, func(context.Context, skillab.Exercise, int, bool) (bool, error) {
		return false, errors.New("the sandbox would not start")
	})
	if err == nil {
		t.Fatal("a failed measurement returned no error")
	}
	if strings.Contains(out.String(), "no measurable difference") {
		t.Errorf("a verdict was printed over a measurement that did not finish:\n%s", out.String())
	}
}

// An unknown flag stops the command where every other refusal does: before a model
// is resolved and before a store is opened.
func TestSkillABRefusesAnUnknownFlag(t *testing.T) {
	var out bytes.Buffer
	if _, _, err := skillABArgs([]string{"--nope", "systematic-debugging"}, &out); err == nil {
		t.Fatal("an unknown flag was accepted")
	}
}

// A set with no holdout is measured and said out loud. Refusing it would stop an
// author measuring anything until a second person had written exercises; staying quiet
// would let a skill be reported as helping on the only exercises its author chose.
func TestSkillABSaysWhenNothingIsHeldBack(t *testing.T) {
	root := writeExerciseSet(t, "open-only", "do the thing | exit 0\n")
	var out bytes.Buffer
	set, repeats, err := skillABArgs([]string{"--exercises", root, "--repeats", "2", "open-only"}, &out)
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	if repeats != 2 || len(set.Exercises) != 1 || set.Skill != "open-only" {
		t.Fatalf("parsed %d repeats over %d exercises for %q", repeats, len(set.Exercises), set.Skill)
	}
	if !strings.Contains(out.String(), "nothing here is held back") {
		t.Errorf("a set with no holdout was measured without saying so:\n%s", out.String())
	}

	// With a holdout, nothing is said: the note is a warning, not a running commentary.
	if err := os.WriteFile(filepath.Join(root, "open-only", skillab.HoldoutFile), []byte("held | exit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var quiet bytes.Buffer
	set, _, err = skillABArgs([]string{"--exercises", root, "open-only"}, &quiet)
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	if set.Holdout() != 1 {
		t.Errorf("%d held-out exercises, want 1", set.Holdout())
	}
	if strings.Contains(quiet.String(), "held back") {
		t.Errorf("a set with a holdout was warned about anyway:\n%s", quiet.String())
	}
}

// pruneSkill takes the record it was asked for and leaves the rest of the library
// standing, which is what makes the two arms differ by one skill rather than by the
// whole pack.
func TestPruneSkillRemovesOnlyTheSkillUnderTest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := openDataStore(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	for _, slug := range []string{"under-test", "the-rest"} {
		if _, err := store.Skills().Upsert(ctx, state.Skill{Slug: slug, Name: slug, Body: "a procedure"}); err != nil {
			t.Fatal(err)
		}
	}

	if err := pruneSkill(ctx, store, "under-test"); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := store.Skills().Get(ctx, "under-test"); err == nil {
		t.Error("the skill under test survived the arm that is meant to be without it")
	}
	if _, err := store.Skills().Get(ctx, "the-rest"); err != nil {
		t.Errorf("pruning one skill took another with it: %v", err)
	}
}

// The usage line puts the skill's name before the flags, and that spelling has to
// work. Go's flag package stops at the first argument that is not a flag, so a
// command that parses straight through refuses the invocation it just printed.
func TestSkillABAcceptsFlagsOnEitherSideOfTheSkillName(t *testing.T) {
	dir := t.TempDir()
	set := filepath.Join(dir, "systematic-debugging")
	if err := os.MkdirAll(set, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(set, skillab.ExercisesFile), []byte("fix it | exit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, args := range map[string][]string{
		"flags after the name":  {"systematic-debugging", "--repeats", "2", "--exercises", dir},
		"flags before the name": {"--repeats", "2", "--exercises", dir, "systematic-debugging"},
		"flags on both sides":   {"--repeats", "2", "systematic-debugging", "--exercises", dir},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			got, repeats, err := skillABArgs(args, &out)
			if err != nil {
				t.Fatalf("skillABArgs: %v", err)
			}
			if got.Skill != "systematic-debugging" {
				t.Errorf("skill = %q, want systematic-debugging", got.Skill)
			}
			if repeats != 2 {
				t.Errorf("repeats = %d, want 2: the flag was read as a positional argument", repeats)
			}
		})
	}
}

// Two names is not a typo the command should guess at. It means the caller meant
// something the command cannot do, and running one of the two would measure a skill
// nobody asked about.
func TestSkillABRefusesTwoSkillNames(t *testing.T) {
	var out bytes.Buffer
	if _, _, err := skillABArgs([]string{"one", "two"}, &out); err == nil {
		t.Fatal("two skill names were accepted")
	}
}
