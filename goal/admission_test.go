package goal

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCheckGrounding: a path a check names must exist before the work or be named by the
// objective, and when the run is confined a path outside the working directory that does not
// exist can never be created. The field failures were a check on a host temp file the task
// never mentioned (outside the directory, so unwritable) and a check on a file only the
// worker would make.
func TestCheckGrounding(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	touch(t, filepath.Join(dir, "go.mod"))
	touch(t, filepath.Join(dir, "pkg", "real.go"))
	touch(t, filepath.Join(outside, "present.txt"))
	missingOutside := filepath.Join(outside, "type_output_id_ed25519.txt")

	lookPath := func(name string) (string, error) {
		if name == "node.exe" {
			return name, nil
		}
		return "", exec.ErrNotFound
	}

	cases := []struct {
		name      string
		check     string
		objective string
		confined  bool
		want      []AdmissionRefusal
	}{
		{name: "a file that exists", check: "findstr module go.mod", want: nil},
		{name: "a nested file that exists", check: "go vet pkg/real.go", want: nil},
		{name: "a backslash path that exists", check: `type pkg\real.go`, want: nil},
		{name: "a file only the worker would create", check: "python verify_result.py", want: []AdmissionRefusal{RefusedUngrounded}},
		{name: "a file the objective asks for", check: "python hello.py", objective: "Write hello.py that prints hello", want: nil},
		{name: "named by file name in a longer path", check: "python src/hello.py", objective: "create hello.py", want: nil},
		{name: "objective case does not matter", check: "python Hello.py", objective: "create hello.PY", want: nil},
		{name: "a program on the search path", check: "node.exe -v", want: nil},
		{name: "only the unnamed file is refused", check: "type go.mod && type extra.log", want: []AdmissionRefusal{RefusedUngrounded}},
		{
			name: "an absolute host path the confined run cannot create", check: "if exist " + missingOutside + " exit 0",
			objective: "show the key file with type", confined: true, want: []AdmissionRefusal{RefusedUnwritable},
		},
		{
			name: "outside and unwritable even when the objective names it", check: "type " + missingOutside,
			objective: "write " + missingOutside, confined: true, want: []AdmissionRefusal{RefusedUnwritable},
		},
		{
			name: "an absolute path outside is only ungrounded when the run is not confined", check: "type " + missingOutside,
			confined: false, want: []AdmissionRefusal{RefusedUngrounded},
		},
		{name: "an existing file outside is only read", check: "type " + filepath.Join(outside, "present.txt"), confined: true, want: nil},
		{name: "a parent escape that does not exist", check: "type ../nowhere-at-all.txt", confined: true, want: []AdmissionRefusal{RefusedUnwritable}},
		{name: "an absolute path inside the directory the objective names", check: "type " + filepath.Join(dir, "made.txt"), objective: "make made.txt", confined: true, want: nil},
		{name: "the target of a redirect is the check's own file", check: "go test ./... > results.log 2>&1", want: nil},
		{name: "a package pattern is not a file", check: "go test ./...", want: nil},
		{name: "a url is not a file", check: "curl --fail http://localhost:8080/health.json", want: nil},
		{name: "code inside a quoted argument is not read as files", check: `python -c "import os; print(os.path.exists('made.txt'))"`, want: nil},
		{name: "a flag value is judged", check: "tool --out=report.txt", want: []AdmissionRefusal{RefusedUngrounded}},
		{name: "a cmd switch is not a path", check: "dir /s /b", want: nil},
		{name: "a module import path is not a file", check: "go build github.com/ionalpha/flynn/cmd/flynn", want: nil},
		{name: "a device is not a file", check: "go build ./... > /dev/null", want: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CheckGrounding(c.check, Ground{Dir: dir, Confined: c.confined, Objective: c.objective, LookPath: lookPath})
			var refusals []AdmissionRefusal
			for _, p := range got {
				refusals = append(refusals, p.Refusal)
				if p.Reason == "" || p.Path == "" {
					t.Fatalf("problem %+v does not say what or why", p)
				}
			}
			if len(refusals) != len(c.want) {
				t.Fatalf("CheckGrounding(%q) = %+v, want refusals %v", c.check, got, c.want)
			}
			for i := range refusals {
				if refusals[i] != c.want[i] {
					t.Fatalf("CheckGrounding(%q)[%d] = %q, want %q", c.check, i, refusals[i], c.want[i])
				}
			}
		})
	}
}

// TestCheckGroundingNeedsADirectory: a host-supplied sandbox has no directory the run chose,
// so grounding has nothing to compare against and must refuse nothing rather than guess.
func TestCheckGroundingNeedsADirectory(t *testing.T) {
	if got := CheckGrounding("type nowhere.txt", Ground{Confined: true}); len(got) != 0 {
		t.Fatalf("grounding without a directory refused %+v", got)
	}
}

// TestDryRunRefusal: the verdict of running a check once before any work. A check that
// already passes proves nothing, and one the shell cannot parse or start never could. A check
// that fails for the reason it should, and one that could not be executed at all, are admitted.
func TestDryRunRefusal(t *testing.T) {
	cases := []struct {
		name    string
		check   string
		verdict ItemVerdict
		want    AdmissionRefusal
	}{
		{name: "passes already", check: "go build ./...", verdict: ItemVerdict{Executed: true, Passed: true}, want: RefusedAlreadyPasses},
		{name: "fails as a check for unfinished work should", check: "go test ./...", verdict: ItemVerdict{Executed: true, ExitCode: 1, Output: "--- FAIL: TestX\nFAIL"}, want: ""},
		{name: "a missing program under cmd", check: "nosuch run", verdict: ItemVerdict{Executed: true, ExitCode: 9009, Output: "'nosuch' is not recognized as an internal or external command,\r\noperable program or batch file."}, want: RefusedMissingProgram},
		{name: "a missing program under sh", check: "nosuch run", verdict: ItemVerdict{Executed: true, ExitCode: 127, Output: "sh: 1: nosuch: not found"}, want: RefusedMissingProgram},
		{name: "a powershell quoting failure", check: `powershell -Command "if (Test-Path 'a') {"`, verdict: ItemVerdict{Executed: true, ExitCode: 1, Output: "At line:1 char:20\r\nMissing closing '}' in statement block or type definition.\r\n    + FullyQualifiedErrorId : ParseException"}, want: RefusedMalformed},
		{name: "a cmd syntax error", check: "if exist (", verdict: ItemVerdict{Executed: true, ExitCode: 255, Output: "The syntax of the command is incorrect."}, want: RefusedMalformed},
		{name: "a cmd unexpected token", check: "if x ( y", verdict: ItemVerdict{Executed: true, ExitCode: 255, Output: ") was unexpected at this time."}, want: RefusedMalformed},
		{name: "an sh syntax error", check: `echo "a`, verdict: ItemVerdict{Executed: true, ExitCode: 2, Output: "sh: 1: Syntax error: Unterminated quoted string"}, want: RefusedMalformed},
		{name: "a bash syntax error", check: `echo "a`, verdict: ItemVerdict{Executed: true, ExitCode: 2, Output: "bash: -c: line 1: unexpected EOF while looking for matching `\"'"}, want: RefusedMalformed},
		{name: "output that only mentions a syntax error", check: "go test ./parser", verdict: ItemVerdict{Executed: true, ExitCode: 1, Output: "    parser_test.go:12: input rejected: syntax error near x\nFAIL"}, want: ""},
		{name: "not executed", check: "go test ./...", verdict: ItemVerdict{Detail: "the check could not run: containment"}, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := DryRunRefusal(c.check, c.verdict)
			if got != c.want {
				t.Fatalf("DryRunRefusal = %q (%s), want %q", got, reason, c.want)
			}
			if (got == "") != (reason == "") {
				t.Fatalf("refusal %q and reason %q disagree", got, reason)
			}
			if got == RefusedMissingProgram && !strings.Contains(reason, "nosuch") {
				t.Fatalf("reason does not name the program: %q", reason)
			}
		})
	}
}
