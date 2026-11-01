package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scratchModule is a Go module with one deliberate bug: Add subtracts. The test file is the
// specification the model must satisfy, and the scenario checks it was left alone.
var scratchModule = map[string]string{
	"go.mod": "module scratch\n\ngo 1.21\n",
	"calc.go": `package scratch

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a - b
}
`,
	"calc_test.go": `package scratch

import "testing"

func TestAdd(t *testing.T) {
	for _, c := range []struct{ a, b, want int }{{1, 2, 3}, {0, 0, 0}, {-4, 4, 0}, {10, 5, 15}} {
		if got := Add(c.a, c.b); got != c.want {
			t.Errorf("Add(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
`,
}

// TestLiveEditAFileAndRunItsTestsInTheSandbox gives the model a scratch repository whose
// test fails, and asks for the fix. The record must show it edited a file and ran the tests
// through the shell tool (which runs inside the sandbox), the run must converge, and the
// host's own `go test` must then pass on a test file the model did not touch. The last
// check is the model-independent proof: the work is right whatever the model said.
func TestLiveEditAFileAndRunItsTestsInTheSandbox(t *testing.T) {
	spec := liveModel(t)
	have(t, "go")
	in := liveInstance(t, spec)
	for name, body := range scratchModule {
		if err := os.WriteFile(filepath.Join(in.workspace, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The sandbox gives a command a minimal environment, so the toolchain and its cache must
	// be reachable from there; a test that fails for that reason is a product finding, and
	// the record below shows the command's own output.
	res := in.liveGoal(
		"The Go tests in this directory fail. Fix the bug in calc.go without editing calc_test.go, "+
			"then run `go test ./...` with the bash tool to confirm they pass.",
		"-verify", "go test ./...",
	)
	rec := in.sealedRun(res)

	if edits := rec.calls("write", "edit"); len(edits) == 0 {
		t.Errorf("the model changed no file; tools called: %v", callNames(rec))
	}
	if !ranCommand(rec, "go test") {
		t.Errorf("the model never ran the tests through the shell tool; tools called: %v", callNames(rec))
	}
	if !rec.converged() {
		t.Errorf("the run did not converge\nstdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
	}

	got, err := os.ReadFile(filepath.Join(in.workspace, "calc_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != scratchModule["calc_test.go"] {
		t.Error("the model edited calc_test.go, the specification it was told to leave alone")
	}
	check := exec.Command("go", "test", "./...")
	check.Dir = in.workspace
	if out, err := check.CombinedOutput(); err != nil {
		t.Errorf("the host's own go test fails after the run: %v\n%s", err, out)
	}
}

// ranCommand reports whether a bash call in the record ran a command containing sub.
func ranCommand(rec *runRecord, sub string) bool {
	for _, c := range rec.calls("bash") {
		if strings.Contains(c.command(), sub) {
			return true
		}
	}
	return false
}

// callNames lists the tool of each call in the record, for a failure message.
func callNames(rec *runRecord) []string {
	var names []string
	for _, c := range rec.calls() {
		names = append(names, c.tool)
	}
	return names
}
