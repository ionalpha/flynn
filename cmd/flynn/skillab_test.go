package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/skill/skillab"
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
