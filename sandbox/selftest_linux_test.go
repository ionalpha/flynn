//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// requireRealConfinement skips a test that needs the host to run the real check, naming
// the refusal so a CI runner that forbids user namespaces says why the test did not run.
func requireRealConfinement(t *testing.T) {
	t.Helper()
	if r := CheckConfinement(context.Background()); !r.Ready {
		t.Skipf("kernel confinement unavailable on this host: %v", r.Err())
	}
}

// TestSelfTestRerunsAfterLaunchFailure holds the two ways a remembered pass ends: a
// confined launch that fails drops it, and a pass older than [gateTTL] is not trusted.
// A profile reload or a sysctl change between launches is met by the next check.
func TestSelfTestRerunsAfterLaunchFailure(t *testing.T) {
	var calls atomic.Int32
	stubGate(t, func(context.Context, needs) Readiness {
		calls.Add(1)
		return Readiness{Ready: true}
	})
	l, err := NewLocal(t.TempDir(), WithReadOnlyFS())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	l.selfExe = filepath.Join(t.TempDir(), "no-such-binary")

	for range 2 {
		if _, err := l.Exec(context.Background(), Command{Line: "true"}); err == nil {
			t.Fatal("a launch that cannot set the confinement up must fail")
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("the check ran %d times for 2 launches, want a re-run after the launch failure", calls.Load())
	}

	now := time.Unix(1_700_000_000, 0)
	prev := gateNow
	gateNow = func() time.Time { return now }
	t.Cleanup(func() { gateNow = prev })
	calls.Store(0)
	n := needs{mount: true}
	for range 3 {
		if err := requireConfinement(n); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("a fresh pass was not reused: %d checks", calls.Load())
	}
	now = now.Add(gateTTL + time.Second)
	if err := requireConfinement(n); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("a pass older than the TTL was trusted: %d checks", calls.Load())
	}
}

// TestRunnerPathNeverFallsBackUnconfined injects the fault the runner meets on a bad
// host, namespace setup failing, in both shapes: the gate refuses, and the gate passes but
// the launch fails. Under [WithDefaultConfinement] every launch path returns an error and
// no command runs, so no path re-runs unconfined.
func TestRunnerPathNeverFallsBackUnconfined(t *testing.T) {
	shapes := map[string]func(*testing.T, *Local){
		"the gate refuses": func(t *testing.T, _ *Local) { stubGate(t, refusing(CauseUserNamespacesDisabled)) },
		"the launch fails after a pass": func(t *testing.T, l *Local) {
			stubGate(t, func(context.Context, needs) Readiness { return Readiness{Ready: true} })
			l.selfExe = filepath.Join(t.TempDir(), "no-such-binary")
		},
	}
	for name, inject := range shapes {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			l, err := NewLocal(dir, WithDefaultConfinement())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			inject(t, l)
			for path, err := range launchPaths(t, l) {
				if err == nil {
					t.Errorf("%s: the launch succeeded although confinement could not be set up", path)
				}
			}
			if _, statErr := os.Stat(filepath.Join(dir, "ran")); statErr == nil {
				t.Fatal("a command ran unconfined")
			}
			if l.ConfinementFallbacks() != 0 {
				t.Fatalf("a fallback was recorded: %d", l.ConfinementFallbacks())
			}
		})
	}
}

// TestSelfTestRunsProductionSandboxCommand holds the canary stage to being evidence. The
// same two commands pass on a confined Local and are told apart on an unconfined one: the
// write outside the working directory is contained under the production options and
// escapes without them, so a host whose confinement is silently absent fails the stage.
func TestSelfTestRunsProductionSandboxCommand(t *testing.T) {
	requireRealConfinement(t)
	ctx := context.Background()
	if f := runCanaryStage(ctx, needsAll); f != nil {
		t.Fatalf("a host that confines must pass the canary stage: %+v", f)
	}

	base := t.TempDir()
	work, outside := filepath.Join(base, "work"), filepath.Join(base, "outside")
	for _, d := range []string{work, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bare, err := NewLocal(work, withoutGate())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bare.Close() })
	var sigsys bool
	f := judgeCanaries(ctx, bare, needs{mount: true}, outside, &sigsys)
	if f == nil || !f.escaped {
		t.Fatalf("a Local with no confinement must fail the canary as escaped, got %+v", f)
	}
	if res := classify(*f, fakeProc{}.read); res.Ready || res.Cause != CauseConfinementIneffective {
		t.Fatalf("an escaped canary must refuse as ineffective confinement, got %+v", res)
	}
}

// TestCanaryStageRefusesWhenThePositiveCanaryFails holds the stage to proving the sandbox
// runs the commands it should: a Local that cannot run the harmless command is a refusal
// and is not read as "everything was contained".
func TestCanaryStageRefusesWhenThePositiveCanaryFails(t *testing.T) {
	requireRealConfinement(t)
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := checkLocal(work, needs{mount: true}, new(bool))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}
	f := judgeCanaries(context.Background(), l, needs{mount: true}, base, new(bool))
	if f == nil || f.escaped {
		t.Fatalf("a sandbox that cannot run the positive canary must refuse without claiming an escape, got %+v", f)
	}
	if res := classify(*f, fakeProc{}.read); res.Ready {
		t.Fatalf("the failure classified as a pass: %+v", res)
	}
}
