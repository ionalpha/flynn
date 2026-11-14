package mission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

// dryRunner stands in for the run's sandbox: it answers each check with the verdict scripted
// for it, and counts how many were run.
type dryRunner struct {
	verdicts map[string]goal.ItemVerdict
	ran      []string
}

func (d *dryRunner) VerifyItem(_ context.Context, _ resource.Resource, it goal.LedgerItem) (goal.ItemVerdict, error) {
	d.ran = append(d.ran, it.Verify)
	if v, ok := d.verdicts[it.Verify]; ok {
		return v, nil
	}
	return goal.ItemVerdict{Executed: true, ExitCode: 1, Output: "FAIL"}, nil
}

type admissionLog struct{ got []goal.AdmissionVerdict }

func (a *admissionLog) RecordAdmission(_ context.Context, _ resource.Resource, v goal.AdmissionVerdict) error {
	a.got = append(a.got, v)
	return nil
}

func admitPlanner(model llm.Model, dir string, confined bool, run *dryRunner, rec *admissionLog) *Planner {
	return NewPlanner(model, WithPlannerAdmission(Admission{Verifier: run, Recorder: rec, Dir: dir, Confined: confined}))
}

func planOf(verify string) string {
	return `[{"item":"do the work","verify":` + jsonString(verify) + `}]`
}

func jsonString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// TestPlannerAdmission: every way a check can be wrong is sent back to the model with the
// reason, and the corrected plan is taken. Each row is a failure seen in a live run: a file only
// the worker would create, a host temp path the confined run cannot write, a check that already
// passed, a quoting error, and a program the host lacks.
func TestPlannerAdmission(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostTemp := filepath.Join(t.TempDir(), "curl_output_61516.txt")
	good := "go test ./..."

	cases := []struct {
		name     string
		bad      string
		verdict  goal.ItemVerdict
		dryRuns  bool // whether the bad check is expected to reach the sandbox
		refusal  goal.AdmissionRefusal
		mentions string
	}{
		{name: "invented artifact", bad: "type proof.txt", refusal: goal.RefusedUngrounded, mentions: "proof.txt"},
		{name: "host temp path", bad: "if exist " + hostTemp + " exit 0", refusal: goal.RefusedUnwritable, mentions: "outside the working directory"},
		{
			name: "already passes", bad: "findstr module go.mod", dryRuns: true,
			verdict: goal.ItemVerdict{Executed: true, Passed: true}, refusal: goal.RefusedAlreadyPasses, mentions: "already passes",
		},
		{
			name: "unparseable quoting", bad: `powershell -Command "if (1) {"`, dryRuns: true,
			verdict: goal.ItemVerdict{Executed: true, ExitCode: 1, Output: "Missing closing '}' in statement block"}, refusal: goal.RefusedMalformed, mentions: "could not parse",
		},
		{
			name: "missing program", bad: "nosuchtool --verify", dryRuns: true,
			verdict: goal.ItemVerdict{Executed: true, ExitCode: 9009, Output: "'nosuchtool' is not recognized as an internal or external command,"}, refusal: goal.RefusedMissingProgram, mentions: "nosuchtool",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := &dryRunner{verdicts: map[string]goal.ItemVerdict{c.bad: c.verdict}}
			rec := &admissionLog{}
			model := llmtest.NewScripted(llmtest.SayText(planOf(c.bad)), llmtest.SayText(planOf(good)))
			spec := goal.Spec{Objective: "make the tests pass", StopCondition: "go test passes"}

			items, err := admitPlanner(model, dir, true, run, rec).Plan(context.Background(), goalResource(t, spec))
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(items) != 1 || items[0].Verify != good {
				t.Fatalf("items = %+v, want the corrected check", items)
			}

			reqs := model.Requests()
			if len(reqs) != 2 {
				t.Fatalf("model called %d times, want 2 (plan, then the correction)", len(reqs))
			}
			msg := reqs[1].Messages[len(reqs[1].Messages)-1].TextContent()
			if !strings.Contains(msg, "not admitted") || !strings.Contains(msg, string(c.refusal)) || !strings.Contains(msg, c.mentions) {
				t.Fatalf("the model was told %q, want the refusal %q mentioning %q", msg, c.refusal, c.mentions)
			}

			sawBad := false
			for _, ran := range run.ran {
				sawBad = sawBad || ran == c.bad
			}
			if sawBad != c.dryRuns {
				t.Fatalf("bad check dry-run = %v, want %v (ran %v): text rules must refuse before the sandbox is used", sawBad, c.dryRuns, run.ran)
			}

			if len(rec.got) != 2 {
				t.Fatalf("recorded %d verdicts, want one per attempt: %+v", len(rec.got), rec.got)
			}
			if first := rec.got[0]; first.Admitted || first.Refusal != c.refusal || first.Reason == "" || first.Item != goal.ItemID("do the work", c.bad) {
				t.Fatalf("first verdict = %+v, want a refusal %q for the bad check", first, c.refusal)
			}
			if second := rec.got[1]; !second.Admitted || second.Refusal != "" || !second.DryRun {
				t.Fatalf("second verdict = %+v, want an admitted, dry-run verdict", second)
			}
		})
	}
}

// TestPlannerAdmissionAdmitsACheckThatFailsForTheRightReason: the case the rule exists to let
// through. A check on an existing file, or a file the objective asks for, that fails now.
func TestPlannerAdmissionAdmitsACheckThatFailsForTheRightReason(t *testing.T) {
	dir := t.TempDir()
	run := &dryRunner{}
	rec := &admissionLog{}
	model := llmtest.NewScripted(llmtest.SayText(planOf("python hello.py")))

	items, err := admitPlanner(model, dir, true, run, rec).Plan(context.Background(),
		goalResource(t, goal.Spec{Objective: "Write hello.py that prints hello", StopCondition: "running it prints hello"}))
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	if len(model.Requests()) != 1 || len(run.ran) != 1 || len(rec.got) != 1 || !rec.got[0].Admitted {
		t.Fatalf("calls=%d ran=%v recorded=%+v, want one pass through admission", len(model.Requests()), run.ran, rec.got)
	}
}

// TestPlannerAdmissionWithoutAnExecutedDryRun: a host that cannot run checks at all must not
// have every plan refused for it. The check is admitted on its text, and the record says no
// dry run happened.
func TestPlannerAdmissionWithoutAnExecutedDryRun(t *testing.T) {
	run := &dryRunner{verdicts: map[string]goal.ItemVerdict{"go test ./...": {Detail: "the check could not run: containment"}}}
	rec := &admissionLog{}
	model := llmtest.NewScripted(llmtest.SayText(planOf("go test ./...")))
	items, err := admitPlanner(model, t.TempDir(), true, run, rec).Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"}))
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	if len(rec.got) != 1 || !rec.got[0].Admitted || rec.got[0].DryRun || !strings.Contains(rec.got[0].Reason, "without a dry run") {
		t.Fatalf("verdict = %+v, want admitted with no dry run and a reason", rec.got)
	}
}

// TestPlannerThatKeepsWritingAnInadmissibleCheck is transient once the correction budget is
// spent, with its own code, so the retry ladder samples again and the goal never starts on the
// check.
func TestPlannerThatKeepsWritingAnInadmissibleCheck(t *testing.T) {
	plan := planOf("type invented.txt")
	model := llmtest.NewScripted(llmtest.SayText(plan), llmtest.SayText(plan), llmtest.SayText(plan))
	_, err := admitPlanner(model, t.TempDir(), true, &dryRunner{}, &admissionLog{}).Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"}))
	if err == nil {
		t.Fatal("an ungrounded check was admitted")
	}
	if fault.Classify(err) != fault.Transient || !strings.Contains(err.Error(), "planner_check_not_admitted") {
		t.Fatalf("err = %v (class %v), want transient planner_check_not_admitted", err, fault.Classify(err))
	}
	if n := len(model.Requests()); n != 1+plannerCorrections {
		t.Fatalf("model called %d times, want %d", n, 1+plannerCorrections)
	}
}

// TestPlannerAdmissionIsOptIn: a planner not given an admission keeps its old behaviour, and a
// planner given one tells the model up front what will be checked.
func TestPlannerAdmissionIsOptIn(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText(planOf("type invented.txt")))
	if items, err := NewPlanner(model).Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"})); err != nil || len(items) != 1 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	if strings.Contains(model.Requests()[0].System, "run once") {
		t.Fatal("a planner without admission told the model its checks would be run")
	}

	model = llmtest.NewScripted(llmtest.SayText(`[]`))
	if _, err := admitPlanner(model, t.TempDir(), true, &dryRunner{}, &admissionLog{}).Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"})); err != nil {
		t.Fatal(err)
	}
	sys := model.Requests()[0].System
	for _, want := range []string{"run once", "fails then", "outside it", "invent a file"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("planning prompt does not say %q: %q", want, sys)
		}
	}
}

type failingDryRun struct{ err error }

func (f failingDryRun) VerifyItem(context.Context, resource.Resource, goal.LedgerItem) (goal.ItemVerdict, error) {
	return goal.ItemVerdict{}, f.err
}

type failingRecord struct{ err error }

func (f failingRecord) RecordAdmission(context.Context, resource.Resource, goal.AdmissionVerdict) error {
	return f.err
}

// TestPlannerAdmissionFailsWhenItCannotJudgeOrRecord: an error from the dry run or from the
// record is returned as it is. Swallowing either would start a goal on a check nobody
// vetted, or one whose vetting left no trace.
func TestPlannerAdmissionFailsWhenItCannotJudgeOrRecord(t *testing.T) {
	boom := context.Canceled
	cases := []struct {
		name string
		adm  Admission
	}{
		{name: "dry run fails", adm: Admission{Verifier: failingDryRun{err: boom}, Recorder: &admissionLog{}, Dir: t.TempDir(), Confined: true}},
		{name: "record fails", adm: Admission{Verifier: &dryRunner{}, Recorder: failingRecord{err: boom}, Dir: t.TempDir(), Confined: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model := llmtest.NewScripted(llmtest.SayText(planOf("go test ./...")))
			items, err := NewPlanner(model, WithPlannerAdmission(c.adm)).Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"}))
			if !errors.Is(err, boom) || items != nil {
				t.Fatalf("items=%v err=%v, want the error and no plan", items, err)
			}
		})
	}
}
