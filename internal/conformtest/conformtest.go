// Package conformtest proves that a conformance suite can fail. A suite that
// has only ever run against correct implementations certifies anything: it is
// indistinguishable from one that checks nothing. [Prove] runs a suite against
// a deliberately broken implementation and asserts that the guarantee the
// defect violates is reported as failed.
//
// The suites take a *testing.T and call t.Run, so there is no seam to hand
// them a recording testing.TB without the real test failing too. [Prove]
// instead re-executes the running test binary, restricted to the calling test
// and told which [Case] to run. The child runs the suite against the broken
// implementation for real; the parent reads the child's verbose output and
// requires the named subtest to appear as failed. A suite that panics or
// hangs also fails the case, since the named failure is then absent.
package conformtest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec" //nolint:depguard // re-executes the running test binary, which no sandbox can host; only test binaries import this package
	"strings"
	"testing"
	"time"
)

// caseEnv carries the selected case name to the child process. Its presence is
// what makes a process the child.
const caseEnv = "FLYNN_CONFORMTEST_CASE"

// childTimeout bounds one child run. The suites finish in well under a second
// against an in-memory fake; the margin covers a loaded CI runner and the
// bus suite's two second delivery waits.
const childTimeout = 90 * time.Second

// Case is one deliberately broken implementation and the guarantee it violates.
type Case struct {
	// Name identifies the defect, for example "Skips tombstone filter". It
	// becomes the subtest name, so keep it unique within one [Prove] call.
	Name string
	// Suite runs the conformance suite against the broken implementation. It
	// runs in the child process with the child's real *testing.T.
	Suite func(t *testing.T)
	// Want is the name of the suite subtest, relative to the suite's top
	// level (for example "ClosedBusRejects"), that must fail. The case also
	// fails when the child exits successfully, so a defect no subtest notices
	// cannot pass unnoticed.
	Want string
}

// Prove runs every case and requires its suite to fail on the named subtest.
// It must be called directly from a top-level test function, which the child
// re-runs by name.
func Prove(t *testing.T, cases []Case) {
	t.Helper()
	if strings.Contains(t.Name(), "/") {
		t.Fatalf("conformtest.Prove must be called from a top-level test, not %q", t.Name())
	}
	if name, ok := os.LookupEnv(caseEnv); ok {
		runChild(t, name, cases)
		return
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.Name] {
			t.Fatalf("duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			out, err := runParent(t, c.Name)
			if err == nil {
				t.Fatalf("suite passed against the broken implementation %q; a suite that cannot fail certifies anything\n%s", c.Name, out)
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("could not run the child process: %v", err)
			}
			marker := "--- FAIL: " + parentTop(t) + "/" + c.Want
			if !hasLine(out, marker) {
				t.Fatalf("suite failed, but not on subtest %q (want a %q line):\n%s", c.Want, marker, out)
			}
		})
	}
}

// parentTop returns the top-level test name from a Prove subtest name.
func parentTop(t *testing.T) string {
	name, _, _ := strings.Cut(t.Name(), "/")
	return name
}

// runChild runs the selected case's suite on the child's own t.
func runChild(t *testing.T, name string, cases []Case) {
	for _, c := range cases {
		if c.Name == name {
			c.Suite(t)
			return
		}
	}
	t.Fatalf("no case named %q", name)
}

// runParent re-executes this test binary for one case and returns its output.
func runParent(t *testing.T, caseName string) (string, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), childTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, //nolint:gosec // exe is this process's own binary and the flags are constants
		"-test.run=^"+parentTop(t)+"$", "-test.v", "-test.count=1",
		"-test.timeout="+childTimeout.String())
	cmd.Env = append(os.Environ(), caseEnv+"="+caseName)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err = cmd.Run()
	return buf.String(), err
}

// hasLine reports whether out has a line that, after indentation, starts with
// marker followed by a space (the elapsed-time suffix) or the end of the line.
// The delimiter keeps "Foo" from matching a failure of "FooBar".
func hasLine(out, marker string) bool {
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if rest, ok := strings.CutPrefix(l, marker); ok && (rest == "" || rest[0] == ' ') {
			return true
		}
	}
	return false
}
