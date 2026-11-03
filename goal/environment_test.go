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
			"cmd wording names the program whatever the exit code", "skill_read systematic-debugging >NUL", 1,
			"'skill_read' is not recognized as an internal or external command,\noperable program or batch file.", "skill_read", true,
		},
		{
			"cmd wording behind a non-zero code that is neither 1 nor 9009", "x", 255,
			"'x' is not recognized as an internal or external command", "x", true,
		},
		{
			"cmd wording quoted mid-line by a check that ran is not a missing program", "go test ./...", 1,
			"--- FAIL: TestShell: got \"'x' is not recognized as an internal or external command\"", "", false,
		},
		{"a raw 256 with nothing to read stays an ordinary failure", "bash -c true", 256, "", "", false},
		{"no text and no command word falls back to the whole check", "&&", 127, "", "&&", true},
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
	// Proof is required: an advisory run moves past the item instead (see
	// TestAnAdvisoryRunMovesPastAnItemWhoseProgramIsMissing).
	h, w := producerHarness(t, ver, ev, WithLedgerConvergence())

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

// TestCommandWords: only the first word of each command is a command. Empty commands, quoted
// empties, assignments and the tail of a substitution are skipped rather than read as programs.
func TestCommandWords(t *testing.T) {
	cases := []struct {
		check string
		want  []string
	}{
		{"go build ./... && lint", []string{"go", "lint"}},
		{"&& test -f x", []string{"test"}},
		{`"" ; grep x`, []string{"grep"}},
		{"FOO=bar make", nil},
		{`test "$(python hello.py)" = "hello"`, []string{"test", "python"}},
		{"", nil},
	}
	for _, c := range cases {
		if got := commandWords(c.check); !slices.Equal(got, c.want) {
			t.Fatalf("commandWords(%q) = %v, want %v", c.check, got, c.want)
		}
	}
}

// runChecksTo drives EnvironmentRepeatLimit rounds of build, verify and observe against a
// planned two-item ledger whose verifier returns the given verdict every time. opts go to
// the reconciler, as for producerHarness.
func runChecksTo(t *testing.T, v ItemVerdict, rounds int, opts ...Option) (Status, *fakeEvidence) {
	t.Helper()
	ev := &fakeEvidence{}
	ver := &fakeVerifier{verdicts: []ItemVerdict{v}}
	h, w := producerHarness(t, ver, ev, opts...)
	ref := h.plannedGoal(t, "checks", twoItemLedger(t))
	h.setStatus(t, ref, func(st *Status) { st.Planned = true })
	for range rounds {
		h.reconcile(t, ref)
		if _, err := w.ProcessOnce(h.ctx); err != nil {
			t.Fatalf("build step: %v", err)
		}
		h.reconcile(t, ref)
		if _, err := w.ProcessOnce(h.ctx); err != nil {
			t.Fatalf("verify step: %v", err)
		}
		h.reconcile(t, ref)
	}
	return h.status(t, ref), ev
}

// TestAnAgentToolNamedInACheckStopsAsAnEnvironmentProblem is the first field failure: the
// model wrote `skill_read systematic-debugging >NUL` as a check, cmd.exe printed that the
// program is not recognized, and the exit code was 1 and not 9009. The goal stopped as not
// converging on a check that never found its program. It must stop naming the program and
// saying the work was not judged, as it does for a 9009.
func TestAnAgentToolNamedInACheckStopsAsAnEnvironmentProblem(t *testing.T) {
	st, ev := runChecksTo(t, ItemVerdict{
		Executed: true, ExitCode: 1,
		Output: "'skill_read' is not recognized as an internal or external command,\noperable program or batch file.",
		Detail: "exit 1: 'skill_read' is not recognized as an internal or external command",
	}, EnvironmentRepeatLimit, WithLedgerConvergence())

	if len(ev.recs) == 0 || ev.recs[0].Provenance == ProvenanceExecuted {
		t.Fatalf("recorded %+v: a check that never ran must not be on the record as executed", ev.recs)
	}
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want %q (message %q)", st.Phase, PhaseStalled, st.Message)
	}
	if reason := stalledReason(st); reason != "CheckCannotRun" {
		t.Fatalf("stalled reason = %q, want CheckCannotRun (message %q)", reason, st.Message)
	}
	if !strings.Contains(st.Message, `"skill_read"`) {
		t.Fatalf("stall message does not name the missing program: %q", st.Message)
	}
	if st.Ledger[0].Proven {
		t.Fatal("an item was proven by a check that never ran")
	}
}

// TestACheckThatRanAndFailedAtExitOneStillCountsAsNotConverging is the control for the
// change above: a program that exists, runs, exits 1 and prints ordinary failure text is
// the work being wrong, and the convergence rule must still see it.
func TestACheckThatRanAndFailedAtExitOneStillCountsAsNotConverging(t *testing.T) {
	st, ev := runChecksTo(t, ItemVerdict{
		Executed: true, ExitCode: 1, Output: "FAIL: TestHealth: got 500, want 200", Detail: "exit 1: FAIL",
	}, VerdictRepeatLimit, WithLedgerConvergence())

	if len(ev.recs) == 0 || ev.recs[0].Provenance != ProvenanceExecuted {
		t.Fatalf("recorded %+v: a check that ran must stay on the record as executed", ev.recs)
	}
	if st.MissingCommand != "" || st.MissingRepeat != 0 {
		t.Fatalf("a check that ran was read as a missing program: %q x%d", st.MissingCommand, st.MissingRepeat)
	}
	if reason := stalledReason(st); reason == "CheckCannotRun" {
		t.Fatalf("a failing check was classified as the environment: %q", st.Message)
	}
	if st.Ledger[0].Proven {
		t.Fatal("an item was proven by a failing check")
	}
}

// TestNamedCommands: the programs a check runs that are among the given names, in order and
// once each. A name that is only an argument is not a command, so a check that greps for a
// tool's name in a file is left alone.
func TestNamedCommands(t *testing.T) {
	tools := []string{"skill_read", "bash", "read", "github_issue_get"}
	cases := []struct {
		name, check string
		want        []string
	}{
		{"the tool is the whole check", "skill_read systematic-debugging >NUL", []string{"skill_read"}},
		{"the tool after a chain", "go build ./... && skill_read x", []string{"skill_read"}},
		{"bash wrapping the command", `bash -c "echo hi" > "%TEMP%un1.log" 2>&1 && findstr hi "%TEMP%un1.log"`, []string{"bash"}},
		{"quoted command word", `"bash" -c true`, []string{"bash"}},
		{"two tools, each once", "read a; read b | skill_read c", []string{"read", "skill_read"}},
		{"a tool name as an argument", "findstr skill_read notes.txt", nil},
		{"a tool name inside a longer word", "reader --check", nil},
		{"a path-qualified command is a file, not the tool", "./read.sh", nil},
		{"no tools given", "skill_read x", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			names := tools
			if c.name == "no tools given" {
				names = nil
			}
			if got := NamedCommands(c.check, names); !slices.Equal(got, c.want) {
				t.Fatalf("NamedCommands(%q) = %v, want %v", c.check, got, c.want)
			}
		})
	}
}
