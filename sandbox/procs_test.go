package sandbox

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ionalpha/flynn/procs"
)

// The registry is process-wide, and this package's other tests leave background helpers
// running, so every assertion here is a delta against a baseline read at the start rather
// than an absolute count.

// A backgrounded server counts as a live child from the moment it starts until it is
// reaped, which is the whole point: an unreaped Serve is the leak child_procs exists to
// show. Nothing walks the machine's process table to learn this.
func TestServeCountsAsALiveChild(t *testing.T) {
	base := procs.Live()

	l := newServeSandbox(t, "block")
	p, err := l.Serve(context.Background(), ServeSpec{Argv: []string{os.Args[0]}})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if got := procs.Live(); got != base+1 {
		t.Fatalf("a running server counts %d live children, want %d", got, base+1)
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// reap runs on its own goroutine, so the decrement lands shortly after Stop returns.
	waitFor(t, 5*time.Second, func() bool { return procs.Live() == base })
	if got := procs.Live(); got != base {
		t.Fatalf("a stopped and reaped server still counts %d live children, want %d", got, base)
	}
}

// A one-shot Exec is started and waited on inside the call, so by the time it returns the
// child is reaped and the count is back where it began. The balance is what matters: a
// spawn path that increments and never decrements would show every command the agent ever
// ran as a live child, and the leak watchdog would fire on a healthy process.
func TestExecLeavesNoLiveChild(t *testing.T) {
	base := procs.Live()

	l := newLocal(t)
	for range 3 {
		if _, err := l.Exec(context.Background(), Command{Line: "echo hello"}); err != nil {
			t.Fatalf("Exec: %v", err)
		}
	}
	if got := procs.Live(); got != base {
		t.Fatalf("after three completed Execs: %d live children, want %d", got, base)
	}

	// A non-zero exit is still a reaped child.
	if _, err := l.Exec(context.Background(), Command{Line: "exit 7"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := procs.Live(); got != base {
		t.Fatalf("after a non-zero exit: %d live children, want %d", got, base)
	}
}

// A command that could not start never ran, so it must not be counted. A spawn path that
// counted a failed exec would drift upward on every missing binary, and drift is exactly
// the shape the leak detector fires on.
func TestFailedSpawnLeavesNoLiveChild(t *testing.T) {
	base := procs.Live()

	l := newLocal(t)
	if _, err := l.Serve(context.Background(), ServeSpec{Argv: []string{"flynn-no-such-binary-6f4400a7"}}); err == nil {
		t.Fatal("Serve of a missing binary should fail")
	}
	if got := procs.Live(); got != base {
		t.Fatalf("a server that never started counts %d live children, want %d", got, base)
	}
}
