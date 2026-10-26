//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadProcReportsAnUnreadableFile(t *testing.T) {
	if _, ok := readProc(filepath.Join(t.TempDir(), "missing")); ok {
		t.Fatal("readProc reported a file that does not exist as read")
	}
	if s, ok := readProc("/proc/self/status"); !ok || s == "" {
		t.Fatalf("readProc could not read /proc/self/status: %q, %v", s, ok)
	}
}

// TestChecksRefuseWhenTheyCannotCreateTheirDirectory shows the two rungs that run through
// the launcher fail, naming the directory, when the scratch location is unusable, instead of
// passing a host they never exercised.
func TestChecksRefuseWhenTheyCannotCreateTheirDirectory(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	for name, run := range map[string]func(context.Context, needs) *failure{
		"launcher": runLauncherStage,
		"canary":   runCanaryStage,
	} {
		t.Run(name, func(t *testing.T) {
			f := run(context.Background(), needsAll)
			if f == nil || !strings.Contains(f.raw, "create check directory") {
				t.Fatalf("got %+v, want a failure naming the directory", f)
			}
		})
	}
}

// TestLauncherStageFailsWhenTheContextIsOver shows a launch that cannot complete is a
// failure of the launcher rung and never a pass.
func TestLauncherStageFailsWhenTheContextIsOver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := runLauncherStage(ctx, needsAll)
	if f == nil || f.stage != stageLauncher {
		t.Fatalf("got %+v, want a launcher-stage failure", f)
	}
}

// unconfinedChecker is a Local that applies no confinement, standing in for a host where
// the confinement silently did nothing: every canary that should be stopped runs.
func unconfinedChecker(t *testing.T, dir string) *Local {
	t.Helper()
	l, err := NewLocal(dir, withoutGate())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// TestCanaryCatchesAnUnconfinedHost is the check's own negative control: the check passes
// only a host that stops the escape, so one that does not must be reported as escaped.
func TestCanaryCatchesAnUnconfinedHost(t *testing.T) {
	base := t.TempDir()
	work, outside := filepath.Join(base, "work"), filepath.Join(base, "outside")
	for _, d := range []string{work, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var sigsys bool
	f := judgeCanaries(context.Background(), unconfinedChecker(t, work), needs{mount: true}, outside, &sigsys)
	if f == nil || !f.escaped {
		t.Fatalf("got %+v, want an escape", f)
	}
}

// TestCanaryChecksTheOutsideDirectoryItself shows a canary that was stopped is not enough:
// the directory it aimed at is read afterwards, and one that cannot be read is no evidence.
func TestCanaryChecksTheOutsideDirectoryItself(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	var sigsys bool
	f := judgeCanaries(context.Background(), unconfinedChecker(t, work), needs{mount: true}, filepath.Join(base, "gone"), &sigsys)
	if f == nil || !f.escaped || !strings.Contains(f.raw, "appeared") {
		t.Fatalf("got %+v, want the unreadable directory reported", f)
	}
}

// TestPositiveCanaryFailureRefuses shows a sandbox that stops the command it must allow is
// refused: stopping everything proves nothing about what it stops.
func TestPositiveCanaryFailureRefuses(t *testing.T) {
	requireUnprivileged(t)
	work := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(work, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(work, 0o700) })
	var sigsys bool
	f := positiveCanary(context.Background(), unconfinedChecker(t, work), &sigsys)
	if f == nil || !strings.Contains(f.raw, "positive canary exited") {
		t.Fatalf("got %+v, want the positive canary's failure", f)
	}
}
