//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const runnerCanary = "runner-environ-canary-value"

// TestRunnerProcessHelper is the stand-in runner that
// TestConfinedCommandCannotReadRunnerEnviron starts as a separate process, so its initial
// environment holds a canary the way a service's EnvironmentFile= secrets sit in a real
// runner's. It does nothing in an ordinary test run. It prints what a command at the
// process-jail floor, and a kernel-confined one where the host allows it, read from the
// runner's /proc/<pid>/environ.
func TestRunnerProcessHelper(t *testing.T) {
	if os.Getenv("RUNNER_HELPER") == "" {
		return
	}
	environ := "HELPER_READFILE=" + filepath.Join("/proc", strconv.Itoa(os.Getpid()), "environ")
	report := func(label string, l *Local) {
		out := strings.ReplaceAll(helperRun(t, l, environ), "\x00", "\n")
		_, _ = fmt.Fprintf(os.Stdout, "RESULT-%s:%s:END\n", label, out)
	}
	report("FLOOR", newTestLocal(t))
	if confined := newTestLocal(t, WithDefaultConfinement()); confined.kernelConfinementEnforceable() {
		report("CONFINED", confined)
	}
}

// runRunnerHelper runs the stand-in runner and returns what the commands it launched
// printed, keyed by "FLOOR" and "CONFINED".
func runRunnerHelper(t *testing.T) map[string]string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=TestRunnerProcessHelper", "-test.v") //nolint:gosec // G204: re-executes this test binary.
	cmd.Env = append(os.Environ(), "RUNNER_HELPER=1", "RUNNER_CANARY="+runnerCanary)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("stand-in runner failed: %v\n%s", err, b)
	}
	got := map[string]string{}
	for _, label := range []string{"FLOOR", "CONFINED"} {
		if _, after, ok := strings.Cut(string(b), "RESULT-"+label+":"); ok {
			got[label], _, _ = strings.Cut(after, ":END")
		}
	}
	if _, ok := got["FLOOR"]; !ok {
		t.Fatalf("stand-in runner printed no result:\n%s", b)
	}
	return got
}

// TestConfinedCommandCannotReadRunnerEnviron pins what the unit's comment and the README
// claim about a runner's /proc/<pid>/environ, where an EnvironmentFile= puts its secrets: a
// kernel-confined command is refused the read, and a command at the floor, which only
// [WithBestEffortConfinement] runs, is not. The floor run is the control: the same read from
// the same user succeeds there, so the refusal belongs to the confinement and the canary is
// really in the runner's environment.
//
// Marking the runner non-dumpable would close the floor case too, and is not done, because
// the kernel then makes /proc/<child pid>/uid_map root-owned and the runner can no longer
// map the user namespace of the commands it confines.
func TestConfinedCommandCannotReadRunnerEnviron(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root holds CAP_SYS_PTRACE and reads any process's environment; run as an unprivileged user")
	}
	got := runRunnerHelper(t)
	if !strings.Contains(got["FLOOR"], runnerCanary) {
		t.Skipf("this host does not let a same-user process read its runner's environment, so the control proves nothing: %q", got["FLOOR"])
	}
	confined, ok := got["CONFINED"]
	if !ok {
		t.Skip("this host does not enforce kernel confinement")
	}
	if strings.Contains(confined, runnerCanary) {
		t.Fatalf("a kernel-confined command read the runner's environment: %q", confined)
	}
	if !strings.Contains(confined, "readerr") {
		t.Fatalf("the confined read should be refused by the kernel, got %q", confined)
	}
}
