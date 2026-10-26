//go:build linux

package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/sandbox"
)

// These tests run the shipped unit's own directives on a real service manager, with
// "flynn sandbox check" as the command, and show that the check says no when the unit is
// made too tight. A directive read from a file is not evidence that a kernel enforces it,
// so the controls start the real thing. They need root, systemd and a built binary, and
// skip with the reason when the host lacks any of them.

// transientSkip are the unit settings that describe where the service runs from rather than
// what it may do, so a transient test unit leaves them out.
var transientSkip = []string{
	"ExecStart", "Type", "Restart", "User", "Group", "StateDirectory", "RuntimeDirectory",
	"RuntimeDirectoryMode", "UMask", "EnvironmentFile",
}

// enforcesRestrictNamespaces lists the architectures on which systemd installs the
// RestrictNamespaces= filter; elsewhere the directive does nothing and a control that
// expects it to bite would fail for the wrong reason.
var enforcesRestrictNamespaces = []string{"amd64", "386", "ppc64", "ppc64le", "s390x"}

// unitRun is one transient service the control started.
type unitRun struct {
	exit   int
	stdout string
	stderr string
}

// readiness decodes the check's JSON record.
func (r unitRun) readiness(t *testing.T) sandbox.Readiness {
	t.Helper()
	var got sandbox.Readiness
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("the check printed no readiness record (exit %d): %v\nstdout: %s\nstderr: %s", r.exit, err, r.stdout, r.stderr)
	}
	return got
}

// sigsysStatus is how systemd words a service killed by a filtered system call, in its
// older ("status=31/SYS") and newer ("status=SYS") forms.
var sigsysStatus = regexp.MustCompile(`status=(31/)?SYS`)

// killedBySIGSYS reports whether the service manager recorded the service dying of a
// filtered system call.
func (r unitRun) killedBySIGSYS() bool {
	return sigsysStatus.MatchString(r.stderr)
}

// requireRealUnit skips unless a transient unit can be started here, and returns the path
// of a flynn binary the unit's account can execute.
func requireRealUnit(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("starting a transient unit needs root")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run is not installed")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("systemd is not the init system of this host")
	}
	// PrivateTmp hides /tmp from the unit and ProtectHome hides /home, so the binary goes
	// under /run, which the unit's account can read.
	dir, err := os.MkdirTemp("/run", "flynn-unit-test-")
	if err != nil {
		t.Skipf("cannot create a directory under /run: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the unit's unprivileged account must traverse it to reach the binary under test
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "flynn")
	if prebuilt := os.Getenv("FLYNN_BIN"); prebuilt != "" {
		b, err := os.ReadFile(prebuilt)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o755); err != nil { //nolint:gosec // the binary under test must be executable by the unit's account
			t.Fatal(err)
		}
		return dst
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("set FLYNN_BIN to a built flynn binary: the go tool is not installed")
	}
	build := exec.Command("go", "build", "-o", dst, "./cmd/flynn")
	build.Dir = ".."
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build flynn: %v\n%s", err, out)
	}
	return dst
}

// unitProperties turns the shipped unit's [Service] settings into the -p arguments of
// systemd-run, so the control runs the directives that ship and not a copy of them.
func unitProperties(t *testing.T) []string {
	t.Helper()
	var props []string
	for _, d := range parseUnit(readFile(t, runnerUnitPath)) {
		if d.section == "[Service]" && !slices.Contains(transientSkip, d.key) {
			props = append(props, "-p", d.key+"="+d.value)
		}
	}
	// The unit runs as flynn, an account the test host does not have; nobody is the
	// unprivileged account every host has.
	return append(props, "-p", "User=nobody")
}

// withoutKey drops every -p argument for one directive.
func withoutKey(props []string, key string) []string {
	var out []string
	for i := 0; i+1 < len(props); i += 2 {
		if !strings.HasPrefix(props[i+1], key+"=") {
			out = append(out, props[i], props[i+1])
		}
	}
	return out
}

var unitSeq int

// runUnit starts "flynn sandbox check --json" as a transient service with the properties.
func runUnit(t *testing.T, flynn string, props []string) unitRun {
	t.Helper()
	unitSeq++
	args := []string{"--wait", "--pipe", "--collect", "--unit=flynn-neg-" + strconv.Itoa(os.Getpid()) + "-" + strconv.Itoa(unitSeq), "-p", "RuntimeMaxSec=40"}
	args = append(args, props...)
	args = append(args, "--", flynn, "sandbox", "check", "--json")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "systemd-run", args...) //nolint:gosec // the arguments are the shipped unit's own directives and a binary this test built
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("the unit did not finish within 90s: a refusal must be prompt\n%s", stderr.String())
	}
	run := unitRun{stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		run.exit = exit.ExitCode()
	case err != nil:
		t.Skipf("cannot run systemd-run here: %v\n%s", err, stderr.String())
	}
	return run
}

// requireShippedUnitReady is the positive control. A host that refuses the shipped unit
// (user namespaces off, an AppArmor restriction with no profile) makes every negative
// control meaningless, so those skip with the refusal that explains it.
func requireShippedUnitReady(t *testing.T, flynn string) {
	t.Helper()
	run := runUnit(t, flynn, unitProperties(t))
	got := run.readiness(t)
	if !got.Ready {
		t.Skipf("the shipped unit is refused on this host, so there is nothing to tighten: %s: %s", got.Cause, got.Detail)
	}
	if run.exit != 0 {
		t.Fatalf("the check reported ready but exited %d", run.exit)
	}
}

// TestNegativeControlUnitFailsSelfTest sets RestrictNamespaces=yes, the line a plain
// hardened service carries, in place of the namespace line the sandbox needs, and requires
// the self-test to refuse and to name the restriction.
func TestNegativeControlUnitFailsSelfTest(t *testing.T) {
	flynn := requireRealUnit(t)
	if !slices.Contains(enforcesRestrictNamespaces, runtime.GOARCH) {
		t.Skipf("systemd does not enforce RestrictNamespaces= on %s, so the control cannot bite there", runtime.GOARCH)
	}
	requireShippedUnitReady(t, flynn)

	for _, value := range []string{"yes", "true"} {
		t.Run("RestrictNamespaces="+value, func(t *testing.T) {
			props := append(withoutKey(unitProperties(t), "RestrictNamespaces"), "-p", "RestrictNamespaces="+value)
			run := runUnit(t, flynn, props)
			got := run.readiness(t)
			if got.Ready || run.exit == 0 {
				t.Fatalf("a unit with RestrictNamespaces=%s passed the self-test (exit %d): %+v", value, run.exit, got)
			}
			if got.Cause != sandbox.CauseNamespaceRestricted {
				t.Fatalf("cause = %q, want %q: %+v", got.Cause, sandbox.CauseNamespaceRestricted, got)
			}
			if !strings.Contains(got.Fix, "RestrictNamespaces=user net mnt") {
				t.Errorf("the remedy does not name the line to set: %q", got.Fix)
			}
		})
	}
}

// TestSyscallGroupTighteningNamedFailure removes one system call or group at a time from
// the shipped filter. Every case must end promptly, never by SIGSYS (the unit sets
// SystemCallErrorNumber=EPERM so a refused call fails instead of killing), never as ready,
// and where the sandbox's own child is what loses the call, with the named cause.
func TestSyscallGroupTighteningNamedFailure(t *testing.T) {
	flynn := requireRealUnit(t)
	requireShippedUnitReady(t, flynn)

	// What the sandbox's launcher needs: the self-test must name the filter.
	for _, call := range []string{"mount", "@mount", "prctl"} {
		t.Run("sandbox needs "+call, func(t *testing.T) {
			run := runUnit(t, flynn, append(unitProperties(t), "-p", "SystemCallFilter=~"+call))
			if run.killedBySIGSYS() {
				t.Fatalf("the service was killed by SIGSYS; SystemCallErrorNumber=EPERM must turn that into a refusal\n%s", run.stderr)
			}
			got := run.readiness(t)
			if got.Ready || run.exit == 0 {
				t.Fatalf("a filter without %s passed the self-test: %+v", call, got)
			}
			if got.Cause != sandbox.CauseSeccompConflict {
				t.Fatalf("cause = %q, want %q: %+v", got.Cause, sandbox.CauseSeccompConflict, got)
			}
			if got.Fix == "" || got.Alternative == "" {
				t.Errorf("the refusal gives no remedy or alternative: %+v", got)
			}
		})
	}

	// What the process needs to run at all: there is no record to read, but the failure
	// must still be prompt, a non-zero exit and never a kill by SIGSYS.
	for _, group := range []string{"@file-system", "@process", "@basic-io", "@io-event", "execve"} {
		t.Run("flynn needs "+group, func(t *testing.T) {
			run := runUnit(t, flynn, append(unitProperties(t), "-p", "SystemCallFilter=~"+group))
			if run.exit == 0 {
				t.Fatalf("the service ran with %s removed: %s", group, run.stdout)
			}
			if run.killedBySIGSYS() {
				t.Fatalf("the service was killed by SIGSYS\n%s", run.stderr)
			}
		})
	}
}
