package goal

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestMissingCommand: the exit code decides whether a program was missing, and the text only
// names it. A check that ran and failed for its own reasons, including output that happens to
// say "not found", is the work being wrong and must not read as the environment.
func TestMissingCommand(t *testing.T) {
	cases := []struct {
		name     string
		check    string
		exit     int
		output   string
		want     string
		wantMiss bool
	}{
		{
			"cmd.exe missing program", `test "$(python hello.py)" = "hello"`, 9009,
			"'test' is not recognized as an internal or external command,\noperable program or batch file.", "test", true,
		},
		{"posix shell missing program", "pytest -q", 127, "sh: 1: pytest: not found", "pytest", true},
		{"posix shell, bash wording", "pytest -q", 127, "bash: pytest: command not found", "pytest", true},
		{"code with no recognisable text names the first word", "make all && make test", 127, "", "make", true},
		{
			"powershell wording exits 1", "Get-Foo", 1,
			"The term 'Get-Foo' is not recognized as the name of a cmdlet, function, script file, or operable program.", "Get-Foo", true,
		},
		{"exit 1 with not found in the output ran its check", "go test ./...", 1, "route: not found", "", false},
		{
			"exit 1 with cmd wording but the wrong code is not trusted", "go test ./...", 1,
			"'x' is not recognized as an internal or external command", "", false,
		},
		{"a passing check", "go test ./...", 0, "ok", "", false},
		{"an ordinary failure", "go test ./...", 2, "FAIL", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := MissingCommand(c.check, c.exit, c.output)
			if ok != c.wantMiss || got != c.want {
				t.Fatalf("MissingCommand(%q, %d, %q) = (%q, %v), want (%q, %v)",
					c.check, c.exit, c.output, got, ok, c.want, c.wantMiss)
			}
		})
	}
}

// TestClassifyUnrunnableDropsExecutionProvenance: a check whose program does not exist
// verified nothing, so it must not keep the executed marker that the gate and the
// non-convergence count both read as "the work was tried and failed".
func TestClassifyUnrunnableDropsExecutionProvenance(t *testing.T) {
	missing := ItemVerdict{Executed: true, ExitCode: 9009, Output: "'test' is not recognized as an internal or external command"}
	got := classifyUnrunnable("test -f out.txt", missing)
	if got.Executed || got.Passed {
		t.Fatalf("verdict = %+v, want neither executed nor passed", got)
	}
	if got.Missing != "test" {
		t.Fatalf("Missing = %q, want the program that was not found", got.Missing)
	}
	if !strings.Contains(got.Detail, "environment") || !strings.Contains(got.Detail, "tampering") {
		t.Fatalf("detail does not tell the agent it is an environment problem and not to stand in for the program: %q", got.Detail)
	}

	for name, v := range map[string]ItemVerdict{
		"ran and failed": {Executed: true, ExitCode: 1, Output: "FAIL"},
		"passed":         {Executed: true, Passed: true},
		"never ran":      {ExitCode: 9009, Output: "'test' is not recognized as an internal or external command"},
	} {
		if out := classifyUnrunnable("go test ./...", v); out != v {
			t.Fatalf("%s: verdict changed from %+v to %+v", name, v, out)
		}
	}
}

// TestObserveMissing: the same program again extends the count, a different one restarts
// it, and anything that is not a missing program ends it.
func TestObserveMissing(t *testing.T) {
	var s Status
	s.observeMissing("test")
	if s.StalledForEnvironment() {
		t.Fatal("stopped after one sighting, before the agent could act on being told")
	}
	s.observeMissing("grep")
	if s.MissingCommand != "grep" || s.MissingRepeat != 1 {
		t.Fatalf("a different program kept the old count: %+v", s)
	}
	s.observeMissing("grep")
	if !s.StalledForEnvironment() {
		t.Fatalf("not stalled after %d sightings of %q", s.MissingRepeat, s.MissingCommand)
	}
	if msg := s.EnvironmentReason(); !strings.Contains(msg, `"grep"`) || !strings.Contains(msg, "not judged") {
		t.Fatalf("reason does not name the program and say the work was not judged: %q", msg)
	}
	s.observeMissing("")
	if s.MissingCommand != "" || s.MissingRepeat != 0 || s.StalledForEnvironment() {
		t.Fatalf("a verdict that ran did not end the count: %+v", s)
	}
}

// TestShadowedCommands: a file in the workspace that answers to a command the check names
// is the agent editing its own verifier. Arguments, path-qualified commands, directories and
// files that merely contain the name are not stand-ins.
func TestShadowedCommands(t *testing.T) {
	write := func(dir, name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("@exit /b 0"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name  string
		files []string
		dirs  []string
		check string
		want  []string
	}{
		{"batch file named for the command", []string{"test.bat"}, nil, `test "$(python hello.py)" = "hello"`, []string{"test"}},
		{"exe", []string{"grep.exe"}, nil, "grep -q hi out.txt", []string{"grep"}},
		{"bare file", []string{"test"}, nil, "test -f out.txt", []string{"test"}},
		{"second command in a chain", []string{"lint.cmd"}, nil, "go build ./... && lint", []string{"lint"}},
		{"a directory is not a stand-in", nil, []string{"test"}, "test -f out.txt", nil},
		{"path-qualified command names the file", []string{"check.bat"}, nil, "./check.bat", nil},
		{"an argument is not a command", []string{"hello.bat"}, nil, "python hello", nil},
		{"unrelated files", []string{"main.go", "README.md"}, nil, "go test ./...", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range c.files {
				write(dir, f)
			}
			for _, d := range c.dirs {
				if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if got := ShadowedCommands(c.check, dir); !slices.Equal(got, c.want) {
				t.Fatalf("ShadowedCommands(%q) = %v, want %v", c.check, got, c.want)
			}
		})
	}
	if got := ShadowedCommands("test -f x", ""); got != nil {
		t.Fatalf("with no directory named, got %v", got)
	}
}

// TestAMissingProgramStopsAsAnEnvironmentProblem is the field failure at the level it ran:
// a check names a program the host lacks, so every verification exits 9009 with the same
// words. Before, each one counted as the work failing its check and the goal stopped as not
// converging, telling the operator the agent was stuck. It must stop naming the program and
// saying the work was not judged.
func TestAMissingProgramStopsAsAnEnvironmentProblem(t *testing.T) {
	missing := ItemVerdict{
		Executed: true, ExitCode: 9009,
		Output: "'test' is not recognized as an internal or external command,\noperable program or batch file.",
		Detail: "exit 9009: 'test' is not recognized as an internal or external command",
	}
	ev := &fakeEvidence{}
	ver := &fakeVerifier{verdicts: []ItemVerdict{missing}}
	h, w := producerHarness(t, ver, ev)

	ledger := twoItemLedger(t)
	ref := h.plannedGoal(t, "missing-program", ledger)
	h.setStatus(t, ref, func(st *Status) { st.Planned = true })

	for range EnvironmentRepeatLimit {
		h.reconcile(t, ref) // dispatch the build step
		if _, err := w.ProcessOnce(h.ctx); err != nil {
			t.Fatalf("build step: %v", err)
		}
		h.reconcile(t, ref) // observe it, dispatch the item's check
		if _, err := w.ProcessOnce(h.ctx); err != nil {
			t.Fatalf("verify step: %v", err)
		}
		h.reconcile(t, ref) // observe the check
	}

	st := h.status(t, ref)
	if len(ev.recs) == 0 || ev.recs[0].Provenance == ProvenanceExecuted {
		t.Fatalf("recorded %+v: a check that never ran must not be on the record as executed", ev.recs)
	}
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want %q (message %q)", st.Phase, PhaseStalled, st.Message)
	}
	if reason := stalledReason(st); reason != "CheckCannotRun" {
		t.Fatalf("stalled reason = %q, want CheckCannotRun (message %q)", reason, st.Message)
	}
	if !strings.Contains(st.Message, `"test"`) {
		t.Fatalf("stall message does not name the missing program: %q", st.Message)
	}
	if st.Ledger[0].Proven {
		t.Fatal("an item was proven by a check that never ran")
	}
}

// TestAMissingProgramThenARealFailureIsNotAnEnvironmentStall: once the check runs, whatever
// it says is about the work, and the environment count must not carry over.
func TestAMissingProgramThenARealFailureIsNotAnEnvironmentStall(t *testing.T) {
	ver := &fakeVerifier{verdicts: []ItemVerdict{
		{Executed: true, ExitCode: 9009, Output: "'test' is not recognized as an internal or external command"},
		{Executed: true, ExitCode: 1, Output: "FAIL", Detail: "exit 1: FAIL"},
	}}
	ev := &fakeEvidence{}
	h, w := producerHarness(t, ver, ev)
	ref := h.plannedGoal(t, "recovers", twoItemLedger(t))

	for range 2 {
		h.reconcile(t, ref)
		if _, err := w.ProcessOnce(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.reconcile(t, ref)
		if _, err := w.ProcessOnce(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.reconcile(t, ref)
	}

	st := h.status(t, ref)
	if st.MissingRepeat != 0 || st.MissingCommand != "" {
		t.Fatalf("the count survived a check that ran: %q x%d", st.MissingCommand, st.MissingRepeat)
	}
	if reason := stalledReason(st); reason == "CheckCannotRun" {
		t.Fatalf("stopped as an environment problem after the check ran: %q", st.Message)
	}
}

// TestAShadowedCheckIsRefusedNotRun: a file standing in for the check's command makes the
// check measure what the agent wrote. The verifier must not be asked, the record must not
// hold an executed verdict, and the agent is told why.
func TestAShadowedCheckIsRefusedNotRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "curl.bat"), []byte("@exit /b 0"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := &fakeEvidence{}
	ver := &fakeVerifier{verdicts: []ItemVerdict{{Passed: true, Executed: true}}}
	h := newHarness(t, neverMet{}, WithLedgerGate(ev, newGate(t, RequireExecuted())))
	w := NewWorker(h.store, h.jobs, h.clk, &fakeExec{}, WithItemVerification(ver, ev), WithCheckDir(dir))

	ref := h.plannedGoal(t, "shadowed", twoItemLedger(t)) // the first item's check runs curl
	h.reconcile(t, ref)
	if _, err := w.ProcessOnce(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t, ref)
	if _, err := w.ProcessOnce(h.ctx); err != nil {
		t.Fatal(err)
	}

	if len(ver.asked) != 0 {
		t.Fatalf("the verifier ran a check a workspace file stands in for: asked %v", ver.asked)
	}
	if len(ev.recs) != 1 || ev.recs[0].Passed || ev.recs[0].Provenance == ProvenanceExecuted {
		t.Fatalf("recorded %+v, want one unexecuted, unpassed verdict", ev.recs)
	}
	st := h.status(t, ref)
	if !strings.Contains(st.ItemFeedback, "curl") || !strings.Contains(st.ItemFeedback, "tampering") {
		t.Fatalf("the agent was not told which file was refused and why: %q", st.ItemFeedback)
	}
}
