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

// writeTaskSet lays out one skill's task set the way an author keeps it: outside the
// pack, one directory per skill.
func writeTaskSet(t *testing.T, slug, tasks string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, slug)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillab.TasksFile), []byte(tasks), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// The command refuses before it spends anything. A skill with no task set, and a
// call with no skill named, both stop at the argument check: resolving a model and
// opening a store first would charge for a run that was never going to happen.
func TestSkillABRefusesBeforeItSpends(t *testing.T) {
	var out bytes.Buffer
	if err := runSkillAB(nil, "", t.TempDir(), &out); err == nil {
		t.Fatal("a call naming no skill was accepted")
	}
	root := writeTaskSet(t, "present", "do the thing | exit 0\n")
	err := runSkillAB([]string{"--tasks", root, "absent"}, "", t.TempDir(), &out)
	if err == nil {
		t.Fatal("a skill with no task set was accepted")
	}
	if !strings.Contains(err.Error(), "no task set") {
		t.Errorf("err = %v, want it to name the missing task set", err)
	}
}

// A trial is one arm of one pair, and the verifier is the only thing that decides
// it. The model here says it is done without doing anything, so the task's own
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
			task := skillab.Task{Objective: "do the thing", Verify: tc.verify}
			got, err := runTrial(ctx, &out, model, harness.Plan{}, "any-skill", task, 1, true)
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
	task := skillab.Task{Objective: "do the thing", Verify: "exit 0"}

	_, err := runTrial(ctx, &out, model, harness.Plan{}, "never-shipped", task, 1, false)
	if err == nil {
		t.Fatal("the arm without a skill the library does not hold ran anyway")
	}
	if !strings.Contains(err.Error(), "nothing to measure the absence of") {
		t.Errorf("err = %v, want it to say the skill is not installed", err)
	}
}

// The report a reader sees carries the verdict, the tallies behind it, and the
// held-out half on its own line. The last of those is the point: a skill that helps
// on its author's tasks and does nothing on the tasks someone else wrote has been
// fitted to its own eval, and one averaged verdict would hide it.
func TestSkillABReportsTheHoldoutSeparately(t *testing.T) {
	set := skillab.Set{Skill: "systematic-debugging"}
	for i := range 6 {
		set.Tasks = append(set.Tasks, skillab.Task{Objective: fmt.Sprintf("open task %d", i), Verify: "exit 0"})
	}
	set.Tasks = append(set.Tasks, skillab.Task{Objective: "held out", Verify: "exit 0", Holdout: true})

	var out bytes.Buffer
	err := measureSkill(context.Background(), &out, set, 1, func(_ context.Context, tk skillab.Task, _ int, withSkill bool) (bool, error) {
		if tk.Holdout {
			return true, nil // the skill changes nothing on the held-out task
		}
		return withSkill, nil
	})
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"measuring systematic-debugging over 7 task(s) x 1 repeat(s), both conditions: 14 runs",
		"systematic-debugging: helped",
		"held-out tasks alone: no measurable difference",
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
	set := skillab.Set{Skill: "x", Tasks: []skillab.Task{{Objective: "do the thing", Verify: "exit 0"}}}
	var out bytes.Buffer
	err := measureSkill(context.Background(), &out, set, 0, func(context.Context, skillab.Task, int, bool) (bool, error) {
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
// author measuring anything until a second person had written tasks; staying quiet
// would let a skill be reported as helping on the only tasks its author chose.
func TestSkillABSaysWhenNothingIsHeldBack(t *testing.T) {
	root := writeTaskSet(t, "open-only", "do the thing | exit 0\n")
	var out bytes.Buffer
	set, repeats, err := skillABArgs([]string{"--tasks", root, "--repeats", "2", "open-only"}, &out)
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	if repeats != 2 || len(set.Tasks) != 1 || set.Skill != "open-only" {
		t.Fatalf("parsed %d repeats over %d tasks for %q", repeats, len(set.Tasks), set.Skill)
	}
	if !strings.Contains(out.String(), "nothing here is held back") {
		t.Errorf("a set with no holdout was measured without saying so:\n%s", out.String())
	}

	// With a holdout, nothing is said: the note is a warning, not a running commentary.
	if err := os.WriteFile(filepath.Join(root, "open-only", skillab.HoldoutFile), []byte("held | exit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var quiet bytes.Buffer
	set, _, err = skillABArgs([]string{"--tasks", root, "open-only"}, &quiet)
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	if set.Holdout() != 1 {
		t.Errorf("%d held-out tasks, want 1", set.Holdout())
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
