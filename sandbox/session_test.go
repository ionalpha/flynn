package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/netguard"
)

// TestSessionHelperProcess is not a real test: it is the echo server the duplex-session
// tests launch as a subprocess. Guarded by an environment variable the parent grants
// through the session's Env, it reads newline-delimited lines from stdin, echoes each back
// on stdout prefixed with "echo:", writes a marker to stderr so the stderr tail can be
// checked, and exits when it reads "quit".
func TestSessionHelperProcess(_ *testing.T) {
	if os.Getenv("FLYNN_SANDBOX_SESSION_HELPER") != "1" {
		return
	}
	_, _ = fmt.Fprintln(os.Stderr, "helper-started")
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := sc.Text()
		if line == "quit" {
			os.Exit(0)
		}
		_, _ = fmt.Fprintln(os.Stdout, "echo:"+line)
	}
	os.Exit(0)
}

// helperSession launches the echo helper under an unconfined best-effort sandbox and
// returns the running session. The helper mode is triggered by an env var granted through
// the session, proving the deny-by-default environment still lets an explicit grant
// through by name.
func helperSession(ctx context.Context, t *testing.T) *Session {
	t.Helper()
	loc, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	t.Cleanup(func() { _ = loc.Close() })
	sess, err := loc.Session(ctx, SessionSpec{
		Argv: []string{os.Args[0], "-test.run=TestSessionHelperProcess"},
		Env:  []string{"FLYNN_SANDBOX_SESSION_HELPER=1"},
	})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	return sess
}

// TestSessionDuplexRoundTrip proves the core property the extension channel needs: the
// caller can write requests to the child's stdin and read its replies from stdout, back
// and forth, over the life of one process, using only its anonymous stdio pipes.
func TestSessionDuplexRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess := helperSession(ctx, t)
	defer func() { _ = sess.Stop() }()

	out := bufio.NewScanner(sess.Stdout())
	for i := range 5 {
		msg := fmt.Sprintf("line-%d", i)
		if _, err := fmt.Fprintln(sess.Stdin(), msg); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if !out.Scan() {
			t.Fatalf("no reply for %q: %v", msg, out.Err())
		}
		if got, want := out.Text(), "echo:"+msg; got != want {
			t.Fatalf("reply %d = %q, want %q", i, got, want)
		}
	}
}

// TestSessionStopReapsProcess proves Stop kills the process and Wait then returns, so a
// disabled extension leaves no orphan and no wedged caller. It is idempotent.
func TestSessionStopReapsProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess := helperSession(ctx, t)

	// Round-trip once so the process is known to be running.
	if _, err := fmt.Fprintln(sess.Stdin(), "ping"); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := bufio.NewScanner(sess.Stdout())
	if !out.Scan() {
		t.Fatalf("no reply: %v", out.Err())
	}

	done := make(chan struct{})
	go func() { _ = sess.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return: the process was not reaped")
	}
	// A second Stop is a no-op, not a panic or a hang.
	if err := sess.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestSessionStderrTailCaptured proves the process's stderr is retained for diagnostics
// (bounded), so a misbehaving extension can be explained without unbounded buffering.
func TestSessionStderrTailCaptured(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess := helperSession(ctx, t)
	defer func() { _ = sess.Stop() }()

	// The helper writes "helper-started" to stderr on startup; poll a bounded number of
	// times (roughly 5s total) for it to appear rather than reading the wall clock.
	for range 250 {
		if strings.Contains(sess.Stderr(), "helper-started") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stderr tail did not capture the helper marker; got %q", sess.Stderr())
}

// TestSessionNoCommand proves an empty argv is refused rather than launching anything.
func TestSessionNoCommand(t *testing.T) {
	loc, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	defer func() { _ = loc.Close() }()
	if _, err := loc.Session(context.Background(), SessionSpec{}); err == nil {
		t.Fatal("expected an error for an empty argv")
	}
}

// TestSessionConfinedRoundTrip exercises the confined duplex path the extension launcher
// actually uses: a read-only host and the syscall filter, with a live stdin/stdout
// conversation over the process's own pipes. It is the security-critical path, so the test
// runs it for real where confinement can be established and still expects a working
// round-trip where the best-effort baseline falls back to the floor. On Windows the confined
// duplex launch is refused, so the round-trip is not attempted there.
func TestSessionConfinedRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("confined duplex launch is refused on Windows; covered by the refusal test")
	}
	loc, err := NewLocal(t.TempDir(), WithReadOnlyFS(), WithSeccomp())
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	t.Cleanup(func() { _ = loc.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess, err := loc.Session(ctx, SessionSpec{
		Argv:    []string{os.Args[0], "-test.run=TestSessionHelperProcess"},
		Env:     []string{"FLYNN_SANDBOX_SESSION_HELPER=1"},
		Confine: true,
	})
	if err != nil {
		t.Fatalf("confined session: %v", err)
	}
	defer func() { _ = sess.Stop() }()

	out := bufio.NewScanner(sess.Stdout())
	if _, err := fmt.Fprintln(sess.Stdin(), "hello"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !out.Scan() {
		t.Fatalf("no reply under confinement: %v (stderr: %q)", out.Err(), sess.Stderr())
	}
	if got := out.Text(); got != "echo:hello" {
		t.Fatalf("confined reply = %q, want echo:hello", got)
	}
}

// TestSessionConfinedRefusedOnWindows proves the refuse-rather-than-downgrade guarantee on
// a platform that cannot express duplex confinement: a session that requires confinement
// (here via governed egress) is refused rather than run unconfined. On platforms that can
// express it the same launch would instead run confined, which the duplex round-trip test
// already covers, so this assertion is Windows-specific.
func TestSessionConfinedRefusedOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("duplex confinement is expressible off Windows; refusal is a Windows-only path")
	}
	loc, err := NewLocal(t.TempDir(), WithEgress(netguard.DenyAll()))
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	defer func() { _ = loc.Close() }()
	_, err = loc.Session(context.Background(), SessionSpec{Argv: []string{os.Args[0]}})
	if err == nil {
		t.Fatal("expected a confined-session refusal on Windows, got nil")
	}
}
