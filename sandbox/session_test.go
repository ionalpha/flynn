package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// TestSessionConfinedRoundTrip exercises the confined duplex path with a live stdin/stdout
// conversation over the process's own pipes. It uses the default (best-effort) confinement,
// the same one the confined Stream test uses: where the platform enforces the read-only host
// and syscall filter the session runs confined, and where it cannot (a locked-down CI runner
// that restricts unprivileged user namespaces) it falls back to the process-jail floor rather
// than failing. Either way the duplex pipes must survive, which is what this asserts. The
// production extension launcher instead requires confinement and refuses to downgrade; that
// refusal path is covered separately.
func TestSessionConfinedRoundTrip(t *testing.T) {
	// Grant read to the directory the helper binary lives in, as the extension launcher
	// does for a verified binary, so the Windows AppContainer child can execute it (the
	// container has no access to a test temp dir otherwise). It is harmless where the
	// binary is already readable.
	binDir := filepath.Dir(os.Args[0])
	loc, err := NewLocal(t.TempDir(), WithDefaultConfinement(), WithReadableDir(binDir))
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

// TestSessionGovernedEgressRefusedOnWindows proves per-host governed egress fails closed on
// Windows: it has no enforcement leg there (an AppContainer grants or denies network
// wholesale, it cannot filter by host), so a session that asks for governed egress is refused
// rather than run with its network silently unfiltered. Deny-all egress (WithNetworkDenied)
// is a separate path that IS enforced on Windows and is what the extension launcher uses for a
// network-free extension. Off Windows governed egress is enforced by the proxy, so this
// refusal is Windows-specific.
func TestSessionGovernedEgressRefusedOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("governed egress is enforced off Windows; the refusal is a Windows-only path")
	}
	loc, err := NewLocal(t.TempDir(), WithEgress(netguard.Policy{AllowPublic: true, AllowHosts: []string{"example.com"}}))
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	defer func() { _ = loc.Close() }()
	_, err = loc.Session(context.Background(), SessionSpec{Argv: []string{os.Args[0]}})
	if err == nil {
		t.Fatal("expected governed egress to be refused on Windows, got nil")
	}
}
