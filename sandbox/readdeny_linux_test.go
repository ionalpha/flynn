//go:build linux

package sandbox

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// runnerLayout stands in for a runner's host files: a data directory with a database in it,
// a socket directory with a listening control socket, and a credential file.
type runnerLayout struct {
	dataDir, socketDir, socket, credential, secret string
}

// newRunnerLayout builds the layout under a fresh directory and starts a listener on the
// socket, so an attempt to connect from inside the sandbox has something to reach.
func newRunnerLayout(t *testing.T) runnerLayout {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := runnerLayout{
		dataDir: filepath.Join(base, "data"), socketDir: filepath.Join(base, "run"),
		credential: filepath.Join(base, "flynn.env"), secret: "runner-secret-value",
	}
	for _, d := range []string{r.dataDir, r.socketDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(r.dataDir, "state.db"), r.credential} {
		if err := os.WriteFile(f, []byte(r.secret), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.socket = filepath.Join(r.socketDir, "control.sock")
	ln, err := net.Listen("unix", r.socket)
	if err != nil {
		t.Skipf("cannot listen on a unix socket here: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go acceptAndClose(ln)
	return r
}

// acceptAndClose answers every connection by closing it, which is all a reach test needs.
func acceptAndClose(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}
}

// hidden is the set a runner passes: everything of its own the sandbox has no use for.
func (r runnerLayout) hidden() LocalOption {
	return WithUnreadablePaths(r.dataDir, r.socketDir, r.credential)
}

// runnerSandbox is the Local a runner launches agent commands in.
func runnerSandbox(t *testing.T, r runnerLayout, extra ...LocalOption) *Local {
	t.Helper()
	l := newTestLocal(t, append([]LocalOption{WithDefaultConfinement(), r.hidden()}, extra...)...)
	requireConfinedRun(t, l)
	return l
}

// helperRun runs the test binary's helper in l with the given environment and returns what
// it printed.
func helperRun(t *testing.T, l *Local, env ...string) string {
	t.Helper()
	bin := copyHelperInto(t, l.Root())
	res, err := l.Capture(context.Background(), CaptureSpec{
		Argv: []string{bin, "-test.run=TestHelperProcess"},
		Env:  append([]string{"SANDBOX_STREAM_HELPER=1"}, env...),
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	return res.Output
}

// TestSandboxCannotReadRunnerDataDir shows the sandbox sees the data directory as an empty
// directory, while the same Local still reads the rest of the host.
func TestSandboxCannotReadRunnerDataDir(t *testing.T) {
	r := newRunnerLayout(t)
	l := runnerSandbox(t, r)
	db := filepath.Join(r.dataDir, "state.db")

	if out := helperRun(t, l, "HELPER_READFILE="+db); !strings.Contains(out, "readerr") || strings.Contains(out, r.secret) {
		t.Fatalf("the sandbox read the runner's database: %q", out)
	}
	res, err := l.Exec(context.Background(), Command{Line: "ls -A " + shellQuote(r.dataDir) + " | wc -l; test -d " + shellQuote(r.dataDir) + " && echo isdir"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "isdir") || !strings.HasPrefix(strings.TrimSpace(res.Output), "0") {
		t.Fatalf("the data directory should look present and empty, got %q", res.Output)
	}
	if out := helperRun(t, l, "HELPER_READFILE=/etc/passwd"); !strings.Contains(out, "read:") {
		t.Fatalf("the rest of the host should stay readable under default confinement: %q", out)
	}
	if got, err := os.ReadFile(filepath.Join(r.dataDir, "state.db")); err != nil || string(got) != r.secret {
		t.Fatalf("the hiding changed the host's own view: %q, %v", got, err)
	}
}

// TestSandboxCannotReadCredentialFiles shows a hidden file reads as empty and cannot be
// removed or replaced from inside the sandbox.
func TestSandboxCannotReadCredentialFiles(t *testing.T) {
	r := newRunnerLayout(t)
	l := runnerSandbox(t, r)

	if out := helperRun(t, l, "HELPER_READFILE="+r.credential); strings.Contains(out, r.secret) {
		t.Fatalf("the sandbox read the runner's credential file: %q", out)
	}
	// The host reads the file as it was: the namespace's view is the only one that changed.
	if got, err := os.ReadFile(r.credential); err != nil || string(got) != r.secret {
		t.Fatalf("the credential file changed on the host: %q, %v", got, err)
	}
	// A write lands in the empty file bound over the credential, so the child may report
	// success; what matters is that the host's file is untouched.
	helperRun(t, l, "HELPER_WRITEFILE="+r.credential)
	if got, _ := os.ReadFile(r.credential); string(got) != r.secret {
		t.Fatalf("the credential file was overwritten on the host: %q", got)
	}
}

// TestSandboxCannotReachControlEndpoints shows neither the control socket nor a loopback API
// of the host can be reached from inside: the socket's directory is hidden and the network
// namespace has no route to the host's loopback.
func TestSandboxCannotReachControlEndpoints(t *testing.T) {
	r := newRunnerLayout(t)
	api, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback here: %v", err)
	}
	t.Cleanup(func() { _ = api.Close() })
	go acceptAndClose(api)

	// A positive control: an unhidden, un-isolated child reaches both, so a refusal below
	// is the confinement and not a broken probe.
	plain := newTestLocal(t)
	for _, dial := range []string{"unix:" + r.socket, "tcp:" + api.Addr().String()} {
		if out := helperRun(t, plain, "HELPER_DIAL="+dial); !strings.Contains(out, "dialed") {
			t.Fatalf("the control probe cannot reach %s even unconfined: %q", dial, out)
		}
	}

	l := runnerSandbox(t, r, WithNetworkDenied())
	for _, dial := range []string{"unix:" + r.socket, "tcp:" + api.Addr().String()} {
		if out := helperRun(t, l, "HELPER_DIAL="+dial); !strings.Contains(out, "dialerr") {
			t.Errorf("the sandbox reached %s: %q", dial, out)
		}
	}
}

// TestSandboxCannotReadRunnerFilesThroughProc tries the way round the hiding that a shared
// process table offers: the runner's own root directory, read through its entry in /proc,
// is a view of the host that the launcher's mounts do not touch.
func TestSandboxCannotReadRunnerFilesThroughProc(t *testing.T) {
	r := newRunnerLayout(t)
	via := filepath.Join("/proc", strconv.Itoa(os.Getpid()), "root", r.dataDir, "state.db")
	// The positive control: without the hiding the path works, so a miss below is the
	// confinement and not a path that never resolved.
	if out := helperRun(t, newTestLocal(t), "HELPER_READFILE="+via); !strings.Contains(out, r.secret) {
		t.Skipf("this host does not let a process read its own root through /proc: %q", out)
	}
	l := runnerSandbox(t, r)
	if out := helperRun(t, l, "HELPER_READFILE="+via); strings.Contains(out, r.secret) {
		t.Fatalf("the sandbox read the runner's database through %s: %q", via, out)
	}
}

// TestReadDenyIsNotAbsorbedByTheFloor fails the launch the way a host that cannot confine
// does, with the opt-in fallback on, and requires the refusal to stand: running the command
// at the floor would hand it the paths that were to be hidden.
func TestReadDenyIsNotAbsorbedByTheFloor(t *testing.T) {
	stubGate(t, func(context.Context, needs) Readiness { return Readiness{Ready: true} })
	l := newTestLocal(t, WithBestEffortConfinement(func(error) {}), WithUnreadablePaths(t.TempDir()))
	l.selfExe = filepath.Join(t.TempDir(), "no-such-binary")
	for path, err := range launchPaths(t, l) {
		if err == nil {
			t.Errorf("%s: the launch succeeded although the set could not be applied", path)
		}
	}
	if l.ConfinementFallbacks() != 0 {
		t.Fatalf("%d launches ran at the floor", l.ConfinementFallbacks())
	}
	if _, err := os.Stat(filepath.Join(l.Root(), "ran")); err == nil {
		t.Fatal("a command ran without the read-deny set")
	}
}

// TestReadDenyRefusesEveryLaunchPath holds the guard to being in front of Exec, Capture,
// Stream, Serve and Session alike.
func TestReadDenyRefusesEveryLaunchPath(t *testing.T) {
	l := newTestLocal(t, WithUnreadablePaths(t.TempDir()))
	for path, err := range launchPaths(t, l) {
		if !errors.Is(err, ErrReadDeny) {
			t.Errorf("%s: got %v, want ErrReadDeny", path, err)
		}
	}
}

// TestReadDenyHideFailureStopsTheCommand shows a path the launcher cannot hide ends the
// launch before the command starts. A dangling symlink exists to the launcher's lstat and
// cannot be mounted over.
func TestReadDenyHideFailureStopsTheCommand(t *testing.T) {
	base := t.TempDir()
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	l := newTestLocal(t, WithDefaultConfinement(), WithUnreadablePaths(dangling))
	requireConfinedRun(t, l)
	res, err := l.Exec(context.Background(), Command{Line: "echo command-ran"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 || strings.Contains(res.Output, "command-ran") {
		t.Fatalf("the command ran although a hidden path could not be hidden: exit %d, %q", res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "hide") {
		t.Errorf("the failure does not say what could not be hidden: %q", res.Output)
	}
}
