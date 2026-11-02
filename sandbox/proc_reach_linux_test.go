//go:build linux

package sandbox_test

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/sandbox/sandboxtest"
)

// Environment and argument values that stand in for a token passed to a long-lived
// process the way the runner receives its bot and API tokens: in the environment and as a
// command-line flag. Each is distinct so a verdict names the file that gave it away.
const (
	procCanaryRoleEnv   = "FLYNN_PROC_CANARY_ROLE"
	procCanaryEnvSecret = "planted-canary-in-the-environment"
	procCanaryArgSecret = "planted-canary-in-the-arguments"
	procCanaryPrefix    = "CANARY "
)

// TestParentProcessIsUnreadableToConfinedCommand holds the default confinement to one
// property: a command it runs cannot read another process's environment or command line
// out of /proc. The Linux sandbox gives the command a new user, network and mount
// namespace and, where the host allows it, a PID namespace with a /proc of its own; without
// that /proc lists the host's processes, and the kernel's rule for reading
// /proc/<pid>/environ and cmdline is "same owner". The runner
// passes its bot and API tokens as flags and in its environment, and a confined command
// runs as the runner's own user, so it is the parent that must be protected.
//
// The planted secrets live in a helper process the test starts (not in the test process,
// whose environ block the kernel snapshots at exec and which os.Setenv does not change),
// and that helper is the confined command's parent, as the runner is for the shell tool.
// An unconfined command reads both files first: if it cannot, the host already hides
// them and the test cannot tell a working confinement from an absent exposure, so it
// skips by name instead of passing.
func TestParentProcessIsUnreadableToConfinedCommand(t *testing.T) {
	sandboxtest.RequireConfinement(t)
	if !sandbox.PrivateProcAvailable() {
		t.Skip("this host masks parts of /proc (a container runtime does), so the kernel refuses the private /proc the isolation needs and the command keeps the host's; keep secrets off the command line there")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestProcCanaryParent$", "-test.v", "--", "token="+procCanaryArgSecret)
	cmd.Env = append(
		os.Environ(),
		procCanaryRoleEnv+"=parent",
		"FLYNN_CANARY_TOKEN="+procCanaryEnvSecret,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper parent failed: %v\n%s", err, out)
	}
	verdicts := parseCanaryVerdicts(string(out))
	t.Logf("kernel and verdicts: %s", describeHost(verdicts))

	if v := verdicts["control-environ"]; v != "leak" {
		t.Skipf("an unconfined command cannot read the parent's environ here (%q), so the exposure this test guards does not exist on this host", v)
	}
	if v := verdicts["control-cmdline"]; v != "leak" {
		t.Skipf("an unconfined command cannot read the parent's cmdline here (%q), so the exposure this test guards does not exist on this host", v)
	}
	if v := verdicts["confined-own-environ"]; v != "clear" {
		t.Fatalf("the confined command's own environment carries the planted secret (%q): the sandbox passed the parent's environment through", v)
	}
	for _, file := range []string{"environ", "cmdline"} {
		if v := verdicts["confined-"+file]; v == "leak" {
			t.Errorf("a default-confined command read the parent's /proc/<pid>/%s and found the planted secret", file)
		} else if v == "" {
			t.Errorf("no verdict for the confined read of /proc/<pid>/%s:\n%s", file, out)
		}
	}
}

// TestProcCanaryParent is the helper the test above starts, not a test of its own: it is
// the process that holds the planted secrets, and it runs the commands whose reach is
// measured, one unconfined (the control) and one under the default confinement the shell
// tool uses. It prints one verdict per line and exits 0 whenever it ran.
func TestProcCanaryParent(t *testing.T) {
	if os.Getenv(procCanaryRoleEnv) != "parent" {
		t.Skip("helper for TestParentProcessIsUnreadableToConfinedCommand")
	}
	pid := os.Getpid()
	read := func(label string, opts ...sandbox.LocalOption) {
		sb, err := sandbox.NewLocal(t.TempDir(), opts...)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sb.Close() }()
		res, err := sb.Exec(context.Background(), sandbox.Command{Line: probeLine(pid)})
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		for _, line := range strings.Split(res.Output, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				_, _ = fmt.Fprintf(os.Stdout, "%s%s-%s=%s\n", procCanaryPrefix, label, k, v)
			}
		}
	}
	read("control")
	read("confined", sandbox.WithDefaultConfinement(), sandbox.WithNetworkDenied())
	_, _ = fmt.Fprintf(os.Stdout, "%skernel=%s\n", procCanaryPrefix, kernelRelease())
}

// probeLine is the shell script the measured command runs: for each of the parent's two
// /proc files it reports whether the planted secret was readable, the file was readable
// without it, or the read was refused. The last line reports whether the command's own
// environment carries the planted environment secret.
func probeLine(pid int) string {
	return fmt.Sprintf(`probe() {
  if grep -aqF -- "$2" /proc/%[1]d/$1 2>/dev/null; then echo "$1=leak"
  elif cat /proc/%[1]d/$1 >/dev/null 2>&1; then echo "$1=clear"
  else echo "$1=denied"; fi
}
probe environ %[2]s
probe cmdline %[3]s
if env | grep -qF -- %[2]s; then echo "own-environ=leak"; else echo "own-environ=clear"; fi
`, pid, procCanaryEnvSecret, procCanaryArgSecret)
}

// parseCanaryVerdicts reads the helper's "CANARY name=value" lines.
func parseCanaryVerdicts(out string) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), procCanaryPrefix)
		if !ok {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

// describeHost renders the verdicts for the test log, so a CI run records what was
// measured and not only that it passed.
func describeHost(m map[string]string) string {
	return fmt.Sprintf("kernel=%s control(environ=%s cmdline=%s) confined(environ=%s cmdline=%s)",
		m["kernel"], m["control-environ"], m["control-cmdline"], m["confined-environ"], m["confined-cmdline"])
}

// kernelRelease reads the running kernel's release string.
func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}
