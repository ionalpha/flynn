package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// missingProgram is a program name no host has, used to drive the launch paths' start
// failures without depending on anything installed.
const missingProgram = "flynn-no-such-program-xyz"

// TestLookPathResolvesAndReports proves the sanctioned resolver finds a program that is on
// PATH and reports one that is not, so a caller outside this package (which may not import
// os/exec) can tell "not installed" from "not reachable inside the sandbox".
func TestLookPathResolvesAndReports(t *testing.T) {
	// The platform shell is the one program guaranteed present on every host we run on.
	name, _ := shell("")
	path, err := LookPath(name)
	if err != nil {
		t.Fatalf("LookPath(%q): %v", name, err)
	}
	if path == "" || !strings.Contains(strings.ToLower(filepath.Base(path)), name) {
		t.Fatalf("LookPath(%q) = %q, want an absolute path to it", name, path)
	}
	if _, err := LookPath(missingProgram); err == nil {
		t.Fatal("a program that is not installed must not resolve")
	}
}

// TestBaselineEnvHoldsNoCredentialNames proves the inherited environment is the tiny,
// secret-free baseline the platform's shell needs, so no credential the agent holds can
// reach a command by inheritance.
func TestBaselineEnvHoldsNoCredentialNames(t *testing.T) {
	keys := baselineKeys()
	if len(keys) == 0 {
		t.Fatal("a command needs at least a PATH")
	}
	want := "PATH"
	found := false
	for _, k := range keys {
		if k == want {
			found = true
		}
		for _, bad := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD"} {
			if strings.Contains(strings.ToUpper(k), bad) {
				t.Fatalf("baseline key %q looks credential-bearing", k)
			}
		}
	}
	if !found {
		t.Fatalf("baseline keys %v do not carry PATH", keys)
	}
	if runtime.GOOS == "windows" {
		// cmd.exe will not start without SystemRoot, so the Windows baseline must carry it.
		if !slicesContain(keys, "SystemRoot") {
			t.Fatalf("the Windows baseline must carry SystemRoot, got %v", keys)
		}
	}
}

func slicesContain(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestConfinementTierMatchesTheReportedLevel proves the tier name and the containment level
// never disagree: a sandbox that reports only the process jail names the jail, and one that
// reports the kernel tier names a kernel mechanism. A record that named a tier the level
// did not support would overstate what held.
func TestConfinementTierMatchesTheReportedLevel(t *testing.T) {
	plain := newTestLocal(t)
	if got := plain.ConfinementTier(); got != "process-jail" {
		t.Fatalf("an unconfined sandbox is the process jail, got %q", got)
	}
	if plain.Containment() != ContainmentNone {
		t.Fatal("an unconfined sandbox must not claim a containment level")
	}

	confined := newTestLocal(t, WithDefaultConfinement())
	tier, level := confined.ConfinementTier(), confined.Containment()
	switch level {
	case ContainmentKernel:
		if tier == "process-jail" {
			t.Fatal("a kernel-confined sandbox must name its kernel mechanism, not the jail")
		}
	default:
		if tier != "process-jail" {
			t.Fatalf("a sandbox that cannot enforce the kernel tier must name the jail, got %q", tier)
		}
	}
}

// TestHostReadableStillReportsAKernelTierWhereEnforced proves the read-permitting tier is a
// different mechanism, not a lower level: it is still kernel-enforced where the platform can
// enforce it, so a caller can record which tier ran rather than being unable to tell them
// apart.
func TestHostReadableStillReportsAKernelTierWhereEnforced(t *testing.T) {
	l := newTestLocal(t, WithDefaultConfinement(), WithHostReadable())
	if l.Containment() == ContainmentKernel && l.ConfinementTier() == "process-jail" {
		t.Fatal("a kernel-confined host-readable sandbox must name its own mechanism")
	}
}

// TestWritableDirsIsTheAuditSurface proves the widening a caller asked for is reportable in
// its resolved form, and that the report is a copy: a caller cannot widen the grants by
// writing to the returned slice.
func TestWritableDirsIsTheAuditSurface(t *testing.T) {
	if got := newTestLocal(t).WritableDirs(); got != nil {
		t.Fatalf("a sandbox with no grants widens nothing, got %v", got)
	}
	dir := t.TempDir()
	l := newTestLocal(t, WithWritableDir(dir, ""), WithReadableDir(dir, ""))
	got := l.WritableDirs()
	if len(got) != 1 {
		t.Fatalf("want exactly the one granted directory (an empty path is ignored), got %v", got)
	}
	if !filepath.IsAbs(got[0]) {
		t.Fatalf("a grant is recorded in resolved absolute form, got %q", got[0])
	}
	got[0] = "/hijacked"
	if l.WritableDirs()[0] == "/hijacked" {
		t.Fatal("WritableDirs must return a copy; a caller must not be able to widen the grants")
	}
	if len(l.readableDirs) != 1 {
		t.Fatalf("an empty readable path is ignored, got %v", l.readableDirs)
	}
}

// TestExecTimeoutBoundsAHangingCommand proves the wall-clock cap ends a command that would
// otherwise run far past it, and that the killed command never reports success. Without the
// cap this call would take a minute; with it, it returns at once.
func TestExecTimeoutBoundsAHangingCommand(t *testing.T) {
	l := newTestLocal(t, WithExecTimeout(50*time.Millisecond))
	res, err := l.Capture(context.Background(), CaptureSpec{
		Argv: helperArgv(),
		Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_SLEEP_MS=60000"},
	})
	if err == nil && res.ExitCode == 0 {
		t.Fatal("a command killed by the exec timeout must never report success")
	}
}

// TestExecTimeoutLeavesAProperCommandAlone proves the cap is a ceiling, not a blanket:
// a command that finishes inside it runs to completion through the shell-line path and
// reports its own result.
func TestExecTimeoutLeavesAProperCommandAlone(t *testing.T) {
	l := newTestLocal(t, WithExecTimeout(30*time.Second))
	res, err := l.Exec(context.Background(), Command{Line: "echo within-the-cap"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "within-the-cap") {
		t.Fatalf("a command inside the cap runs normally, got %+v", res)
	}
}

// TestLaunchPathsRefuseAnEmptyCommand proves every launch path refuses an empty or blank
// argv rather than exec'ing something undefined.
func TestLaunchPathsRefuseAnEmptyCommand(t *testing.T) {
	l := newTestLocal(t)
	ctx := context.Background()
	for _, argv := range [][]string{nil, {""}} {
		if _, err := l.Serve(ctx, ServeSpec{Argv: argv}); err == nil {
			t.Fatalf("Serve(%q) must be refused", argv)
		}
		if _, err := l.Stream(ctx, StreamSpec{Argv: argv}); err == nil {
			t.Fatalf("Stream(%q) must be refused", argv)
		}
		if _, err := l.Session(ctx, SessionSpec{Argv: argv}); err == nil {
			t.Fatalf("Session(%q) must be refused", argv)
		}
		if _, err := l.Capture(ctx, CaptureSpec{Argv: argv}); err == nil {
			t.Fatalf("Capture(%q) must be refused", argv)
		}
	}
}

// TestLaunchPathsSurfaceAStartFailure proves a program that cannot be started is an error on
// every launch path, never a handle to a process that does not exist.
func TestLaunchPathsSurfaceAStartFailure(t *testing.T) {
	l := newTestLocal(t)
	ctx := context.Background()
	argv := []string{missingProgram}

	if p, err := l.Serve(ctx, ServeSpec{Argv: argv}); err == nil {
		_ = p.Stop()
		t.Fatal("Serve must fail when the program cannot be started")
	}
	if p, err := l.Stream(ctx, StreamSpec{Argv: argv}); err == nil {
		_ = p.Wait()
		t.Fatal("Stream must fail when the program cannot be started")
	}
	if s, err := l.Session(ctx, SessionSpec{Argv: argv}); err == nil {
		_ = s.Stop()
		t.Fatal("Session must fail when the program cannot be started")
	}
	if _, err := l.Capture(ctx, CaptureSpec{Argv: argv}); err == nil {
		t.Fatal("Capture must fail when the program cannot be started")
	}
	if _, err := l.Exec(ctx, Command{Line: missingProgram}); err != nil {
		// A shell that cannot find the program is a non-zero exit, not a start failure:
		// the shell itself started. Only report if the shell could not be run at all.
		t.Fatalf("Exec runs the line through a shell, so a missing program is a non-zero exit: %v", err)
	}
}

// TestServeHandleReportsTheProcess proves a started server's handle carries its pid, its
// retained output, and a done channel that closes when it exits, so a host can supervise it.
func TestServeHandleReportsTheProcess(t *testing.T) {
	l := newServeSandbox(t, "exit") // the helper prints a diagnostic and exits non-zero
	p, err := l.Serve(context.Background(), ServeSpec{Argv: []string{os.Args[0]}})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if p.PID() == 0 {
		t.Fatal("a started process must report a pid")
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a process that exits must close its done channel")
	}
	if p.Running() {
		t.Fatal("an exited process must not report running")
	}
	if !strings.Contains(p.Output(), "cannot bind") {
		t.Fatalf("the retained tail should carry why the server failed, got %q", p.Output())
	}
	// Waiting on an exited process returns its recorded outcome without a second OS wait.
	if err := p.Wait(context.Background()); err == nil {
		t.Fatal("a server that exited non-zero must report its exit error")
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("stopping an already-exited process is a no-op: %v", err)
	}
}

// TestServeWaitHonorsTheContext proves Wait returns when the caller's context is done and
// does not kill the process: the caller decides whether to Stop it.
func TestServeWaitHonorsTheContext(t *testing.T) {
	l := newServeSandbox(t, "block")
	p, err := l.Serve(context.Background(), ServeSpec{Argv: []string{os.Args[0]}})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait should return the context error, got %v", err)
	}
	if !p.Running() {
		t.Fatal("a context-bounded Wait must not kill the process")
	}
}

// TestTailBufferKeepsOnlyTheTail proves a chatty server cannot grow the retained buffer
// without bound, and that what survives is the most recent output (where a failure's reason
// is).
func TestTailBufferKeepsOnlyTheTail(t *testing.T) {
	tb := newTailBuffer(16)
	if _, err := tb.Write([]byte(strings.Repeat("o", 100))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := tb.Write([]byte("the-real-reason")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := tb.String()
	if len(got) > 16 {
		t.Fatalf("the tail buffer is bounded at 16 bytes, got %d", len(got))
	}
	if !strings.Contains(got, "real-reason") {
		t.Fatalf("the most recent output should survive, got %q", got)
	}
}

// TestFileOpsDenyEscapes proves every file operation on the always-on floor refuses a path
// that leaves the root, so the deny-by-default filesystem boundary holds on the read, the
// write, and the walk alike.
func TestFileOpsDenyEscapes(t *testing.T) {
	l := newTestLocal(t)
	ctx := context.Background()
	for _, p := range []string{"../escape.txt", filepath.Join("sub", "..", "..", "escape.txt")} {
		if _, err := l.ReadFile(ctx, p); !errors.Is(err, ErrDenied) {
			t.Fatalf("ReadFile(%q) should be denied, got %v", p, err)
		}
		if err := l.WriteFile(ctx, p, []byte("x")); !errors.Is(err, ErrDenied) {
			t.Fatalf("WriteFile(%q) should be denied, got %v", p, err)
		}
		if _, err := l.Walk(ctx, p); !errors.Is(err, ErrDenied) {
			t.Fatalf("Walk(%q) should be denied, got %v", p, err)
		}
	}
}

// TestWriteFileCreatesParentsAndRoundTrips proves a confined write creates the directories
// it needs and the value reads back through the confined read, relative to the root.
func TestWriteFileCreatesParentsAndRoundTrips(t *testing.T) {
	l := newTestLocal(t)
	ctx := context.Background()
	if err := l.WriteFile(ctx, "a/b/c.txt", []byte("data")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := l.ReadFile(ctx, "a/b/c.txt")
	if err != nil || string(got) != "data" {
		t.Fatalf("read back = %q err=%v", got, err)
	}
	// The write landed inside the root, not somewhere the rel() rendering hid.
	if _, err := os.Stat(filepath.Join(l.Root(), "a", "b", "c.txt")); err != nil {
		t.Fatalf("the file should exist under the root: %v", err)
	}
}

// TestGlobAndWalkStayWithinTheRoot proves both listings report root-relative paths and never
// leak a host path or a match from outside the root.
func TestGlobAndWalkStayWithinTheRoot(t *testing.T) {
	l := newTestLocal(t)
	ctx := context.Background()
	for _, p := range []string{"one.txt", "two.txt", "sub/three.txt"} {
		if err := l.WriteFile(ctx, p, []byte("x")); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	matches, err := l.Glob(ctx, "*.txt")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("want the two top-level files, got %v", matches)
	}
	for _, m := range matches {
		if filepath.IsAbs(m) {
			t.Fatalf("a match must be root-relative, got %q", m)
		}
	}
	// A pattern that reaches outside the root lists nothing outside it: the only path that can
	// survive the confinement filter is the root itself.
	sibling := filepath.Join(filepath.Dir(l.Root()), "sibling.txt")
	if err := os.WriteFile(sibling, []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(sibling) })
	outside, err := l.Glob(ctx, "../*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, m := range outside {
		if m != "." {
			t.Fatalf("an escaping pattern must list nothing outside the root, got %v", outside)
		}
	}
	files, err := l.Walk(ctx, ".")
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("walk should find every regular file under the root, got %v", files)
	}
	for _, f := range files {
		if strings.Contains(f, "\\") {
			t.Fatalf("a walked path is reported in slash form, got %q", f)
		}
	}
}

// TestGlobRefusesAMalformedPattern proves a bad pattern is reported as an error rather than
// silently matching nothing, which a caller would read as "no such file".
func TestGlobRefusesAMalformedPattern(t *testing.T) {
	if _, err := newTestLocal(t).Glob(context.Background(), "["); err == nil {
		t.Fatal("a malformed glob pattern must be an error")
	}
}

// TestWithEnvIgnoresAnEmptyGrant proves granting nothing changes nothing: the environment
// stays the deny-by-default baseline rather than allocating an empty grant map.
func TestWithEnvIgnoresAnEmptyGrant(t *testing.T) {
	l := newTestLocal(t, WithEnv(nil), WithEnv(map[string]string{}))
	if l.granted != nil {
		t.Fatalf("an empty grant must not create a grant map, got %v", l.granted)
	}
	if len(l.env()) == 0 {
		t.Fatal("the baseline environment must still be built")
	}
}
