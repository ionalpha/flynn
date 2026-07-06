package sandbox

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess is the child end of the streaming tests: re-executed as a subprocess
// (never a real test), it emits lines, optionally sleeps, optionally echoes a granted
// environment variable, and exits with a requested code, all driven by the environment the
// sandbox granted it. Running the test binary directly (no shell) keeps the tests portable
// and exercises the deny-by-default environment for real.
func TestHelperProcess(_ *testing.T) {
	if os.Getenv("SANDBOX_STREAM_HELPER") != "1" {
		return
	}
	if v := os.Getenv("HELPER_ECHO_ENV"); v != "" {
		// Report the granted value (empty when the variable did not survive the scrub).
		_, _ = os.Stdout.WriteString("env:" + os.Getenv(v) + "\n")
	}
	if v := os.Getenv("HELPER_STDERR"); v != "" {
		// Write to standard error so a combined-output caller (Capture) sees it and a
		// stdout-only caller (Stream) does not.
		_, _ = os.Stderr.WriteString(v + "\n")
	}
	if p := os.Getenv("HELPER_READFILE"); p != "" {
		// Report whether a file outside the workspace can be read, for the readable-dir
		// grant tests: "read:<contents>" on success, "readerr" when the confinement denies it.
		if b, err := os.ReadFile(p); err == nil {
			_, _ = os.Stdout.WriteString("read:" + string(b) + "\n")
		} else {
			_, _ = os.Stdout.WriteString("readerr\n")
		}
	}
	if n, _ := strconv.Atoi(os.Getenv("HELPER_LINES")); n > 0 {
		for i := range n {
			_, _ = os.Stdout.WriteString("line" + strconv.Itoa(i) + "\n")
		}
	}
	if ms, _ := strconv.Atoi(os.Getenv("HELPER_SLEEP_MS")); ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	if code, _ := strconv.Atoi(os.Getenv("HELPER_EXIT")); code != 0 {
		os.Exit(code)
	}
	os.Exit(0)
}

// helperArgv runs the test binary back into TestHelperProcess as a plain subprocess.
func helperArgv() []string {
	return []string{os.Args[0], "-test.run=TestHelperProcess"}
}

func TestStreamStreamsStdout(t *testing.T) {
	l := newTestLocal(t)
	p, err := l.Stream(context.Background(), StreamSpec{
		Argv: helperArgv(),
		Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_LINES=3"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := readLines(t, p)
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	want := []string{"line0", "line1", "line2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("lines = %v, want %v", got, want)
	}
}

func TestStreamWaitReportsNonZeroExit(t *testing.T) {
	l := newTestLocal(t)
	p, err := l.Stream(context.Background(), StreamSpec{
		Argv: helperArgv(),
		Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_EXIT=3"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = readLines(t, p)
	if err := p.Wait(); err == nil {
		t.Fatal("Wait returned nil for a non-zero exit; want an error")
	}
}

func TestStreamContextCancelKills(t *testing.T) {
	l := newTestLocal(t)
	ctx, cancel := context.WithCancel(context.Background())
	p, err := l.Stream(ctx, StreamSpec{
		Argv: helperArgv(),
		// Emit one line immediately, then sleep well past the test's patience.
		Env: []string{"SANDBOX_STREAM_HELPER=1", "HELPER_LINES=1", "HELPER_SLEEP_MS=60000"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	sc := bufio.NewScanner(p.Stdout())
	if !sc.Scan() {
		t.Fatal("expected the first line before the sleep")
	}
	cancel()
	done := make(chan struct{})
	go func() {
		_ = p.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Wait did not return after cancel; the process was not killed")
	}
}

func TestStreamGrantsEnvButScrubsHost(t *testing.T) {
	t.Setenv("SANDBOX_HOST_SECRET", "must-not-leak")
	l := newTestLocal(t)
	// Echo the host secret's name: it was set in this process's environment but never
	// granted, so it must not survive the deny-by-default scrub.
	p, err := l.Stream(context.Background(), StreamSpec{
		Argv: helperArgv(),
		Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_ECHO_ENV=SANDBOX_HOST_SECRET"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := readLines(t, p)
	_ = p.Wait()
	if len(got) != 1 || got[0] != "env:" {
		t.Fatalf("host secret leaked into the child env: %v", got)
	}

	// Now grant a variable and confirm it does reach the child.
	p, err = l.Stream(context.Background(), StreamSpec{
		Argv: helperArgv(),
		Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_ECHO_ENV=GRANTED", "GRANTED=reached"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got = readLines(t, p)
	_ = p.Wait()
	if len(got) != 1 || got[0] != "env:reached" {
		t.Fatalf("granted var did not reach the child: %v", got)
	}
}

func TestStreamConfinedProducesOutput(t *testing.T) {
	l := newTestLocal(t, WithDefaultConfinement())
	p, err := l.Stream(context.Background(), StreamSpec{Argv: echoArgv("confined-ok"), Confine: true})
	if err != nil {
		t.Fatalf("Stream confined: %v", err)
	}
	got := readLines(t, p)
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(got) == 0 || !strings.Contains(strings.Join(got, "\n"), "confined-ok") {
		t.Fatalf("confined stream output = %v, want it to contain confined-ok", got)
	}
}

func TestStreamConfinedCancelKills(t *testing.T) {
	l := newTestLocal(t, WithDefaultConfinement())
	// A confined child can exec only a binary it can read: on Windows the AppContainer
	// cannot read the test binary in its temp dir, so copy the helper into the working
	// directory, the one location the confinement grants it.
	bin := copyHelperInto(t, l.Root())
	ctx, cancel := context.WithCancel(context.Background())
	p, err := l.Stream(ctx, StreamSpec{
		Argv:    []string{bin, "-test.run=TestHelperProcess"},
		Env:     []string{"SANDBOX_STREAM_HELPER=1", "HELPER_LINES=1", "HELPER_SLEEP_MS=60000"},
		Confine: true,
	})
	if err != nil {
		t.Fatalf("Stream confined: %v", err)
	}
	sc := bufio.NewScanner(p.Stdout())
	if !sc.Scan() {
		t.Fatal("expected the first line before the sleep")
	}
	cancel()
	done := make(chan struct{})
	go func() {
		_ = p.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Wait did not return after cancel; the confined process was not killed")
	}
}

func TestStreamRejectsEmptyArgv(t *testing.T) {
	l := newTestLocal(t)
	if _, err := l.Stream(context.Background(), StreamSpec{}); err == nil {
		t.Fatal("Stream with no argv returned nil error; want a rejection")
	}
}

// echoArgv prints msg through the platform's shell as a system binary the confinement can
// execute (a confined AppContainer child can exec only system-readable binaries, so the
// test binary is not usable there).
func echoArgv(msg string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", "echo " + msg}
	}
	return []string{"/bin/sh", "-c", "echo " + msg}
}

// copyHelperInto copies the running test binary into dir (the confinement's writable
// working directory) so a confined child can exec it, and returns the copy's path.
func copyHelperInto(t *testing.T, dir string) string {
	t.Helper()
	src, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	name := "helper"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, src, 0o755); err != nil { //nolint:gosec // a test helper binary must be executable
		t.Fatalf("write helper: %v", err)
	}
	return dst
}

// readLines drains a process's stdout into a slice of trimmed lines.
func readLines(t *testing.T, p *StreamProcess) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(p.Stdout())
	for sc.Scan() {
		out = append(out, strings.TrimRight(sc.Text(), "\r"))
	}
	return out
}

// newTestLocal builds a Local rooted at a fresh temp directory, closed on cleanup.
func newTestLocal(t *testing.T, opts ...LocalOption) *Local {
	t.Helper()
	l, err := NewLocal(t.TempDir(), opts...)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}
