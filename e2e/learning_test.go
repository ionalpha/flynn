package e2e

import (
	"strings"
	"sync/atomic"
	"testing"
)

// The learning loop through the shipped binary. Every other goal in this suite runs
// with -no-learn, so these are the tests that cover what a user gets by default: the
// bundled skill pack offered to a run, a skill read through skill_read and credited
// with the outcome, and a converged run distilled into durable skills and memory that
// the next run is handed.

// isDistillRequest reports whether a request is the post-run distillation call,
// identified by the distiller's standing prompt (learn.defaultDistillSystem).
func isDistillRequest(req oaiRequest) bool {
	return strings.Contains(req.System, "You distill durable, reusable lessons")
}

// skillRow returns the `flynn skill ls` row for slug, or "" when it is not listed.
func skillRow(ls, slug string) string {
	for _, line := range strings.Split(ls, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == slug {
			return line
		}
	}
	return ""
}

// TestLearningRunReadsABundledSkillAndCreditsIt: on a fresh install the pack is in the
// store, the run can read a bundled skill through skill_read, and the read and the win
// are recorded against that skill, so ranking reflects runs that took a skill up.
func TestLearningRunReadsABundledSkillAndCreditsIt(t *testing.T) {
	var build atomic.Int32
	fake := newFakeOpenAIFunc(t, func(req oaiRequest, _ int) oaiReply {
		if isDistillRequest(req) {
			return finalText("[]")
		}
		if build.Add(1) == 1 {
			return toolCall("call_1", "skill_read", `{"skill":"systematic-debugging"}`)
		}
		return finalText("Read the debugging procedure; nothing else to do.")
	})
	in := newInstance(t).withModel(fake)

	before := in.run("skill", "ls")
	requireExit(t, before, 0, "skill ls before")
	if row := skillRow(before.stdout, "systematic-debugging"); row == "" || !strings.Contains(row, "bundled") {
		t.Fatalf("the bundled pack is not in a fresh install's store:\n%s", before.stdout)
	}

	res := in.run("goal", "debug why the calculator test fails")
	requireExit(t, res, 0, "goal with learning on")

	var sawSkillBody bool
	for i := range fake.count() {
		for _, m := range fake.request(t, i).Messages {
			if m.Role == "tool" && strings.Contains(m.Content, "Systematic debugging") {
				sawSkillBody = true
			}
		}
	}
	if !sawSkillBody {
		t.Fatal("skill_read did not hand the run the bundled skill's body")
	}

	after := in.run("skill", "ls")
	requireExit(t, after, 0, "skill ls after")
	row := skillRow(after.stdout, "systematic-debugging")
	f := strings.Fields(row)
	if len(f) < 4 || f[2] != "1" || f[3] != "1" {
		t.Fatalf("the read and the win were not credited to the skill (want READS 1, WINS 1):\n%s", row)
	}
}

// TestLearningRunDistillsASkillAndAMemoryTheNextRunIsHanded: a converged run is
// distilled; the skill passes its check and is kept, the memory is stored, and the
// next run on the same install is offered both.
func TestLearningRunDistillsASkillAndAMemoryTheNextRunIsHanded(t *testing.T) {
	lessons := `[
	  {"kind":"skill","title":"tidy-imports","body":"Group standard library imports first, then a blank line, then the rest.","tags":["go","imports"],"check":"` + passingCheck + `"},
	  {"kind":"memory","title":"calculator build","body":"The calculator package builds with plain go build and has no generated code.","tags":["calculator"]}
	]`
	fake := newFakeOpenAIFunc(t, func(req oaiRequest, _ int) oaiReply {
		if isDistillRequest(req) {
			return finalText(lessons)
		}
		return finalText("Done.")
	})
	in := newInstance(t).withModel(fake)

	first := in.run("goal", "tidy the imports in the calculator package")
	requireExit(t, first, 0, "first goal")

	ls := in.run("skill", "ls")
	requireExit(t, ls, 0, "skill ls")
	if row := skillRow(ls.stdout, "tidy-imports"); row == "" || !strings.Contains(row, "local") {
		t.Fatalf("the distilled skill was not kept:\n%s\nfirst run output:\n%s", ls.stdout, first.combined())
	}

	n := fake.count()
	second := in.run("goal", "tidy the imports in the calculator package again")
	requireExit(t, second, 0, "second goal")

	var handed string
	for i := n; i < fake.count(); i++ {
		req := fake.request(t, i)
		if !isDistillRequest(req) {
			handed = req.System
			break
		}
	}
	for _, want := range []string{"tidy-imports", "calculator package builds with plain go build"} {
		if !strings.Contains(handed, want) {
			t.Errorf("the next run was not handed %q; its system prompt was:\n%s", want, handed)
		}
	}

	// The distilled memory is the agent's own note, so it reaches the next run through
	// recall rather than the wake digest; the record of pushes and uses still reads back.
	requireExit(t, in.run("memory", "usage"), 0, "memory usage")
}
