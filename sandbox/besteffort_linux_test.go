//go:build linux

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// launchPaths runs a touch of "ran" through every launch path that starts a process and
// returns the error each gave, keyed by the path's name.
func launchPaths(t *testing.T, l *Local) map[string]error {
	t.Helper()
	ctx := context.Background()
	touch := []string{"/bin/sh", "-c", "touch ran"}
	out := map[string]error{}
	_, out["exec"] = l.Exec(ctx, Command{Line: "touch ran"})
	_, out["capture"] = l.Capture(ctx, CaptureSpec{Argv: touch})
	if p, err := l.Stream(ctx, StreamSpec{Argv: touch, Confine: true}); err != nil {
		out["stream"] = err
	} else {
		_ = p.Wait()
	}
	if p, err := l.Serve(ctx, ServeSpec{Argv: touch, Confine: true}); err != nil {
		out["serve"] = err
	} else {
		_ = p.Stop()
	}
	if s, err := l.Session(ctx, SessionSpec{Argv: touch, Confine: true}); err != nil {
		out["session"] = err
	} else {
		_ = s.Stop()
	}
	return out
}

// TestDefaultConfinementRefusesOnEveryLaunchPath is the fail-closed default: a Local built
// with only [WithDefaultConfinement] on a host that refuses confinement gives the classified
// refusal from every launch path, names the container and microVM tiers, and never runs the
// command.
func TestDefaultConfinementRefusesOnEveryLaunchPath(t *testing.T) {
	stubGate(t, refusing(CauseAppArmorUserns))
	dir := t.TempDir()
	l, err := NewLocal(dir, WithDefaultConfinement())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	for path, err := range launchPaths(t, l) {
		if !errors.Is(err, ErrConfinementRefused) {
			t.Errorf("%s: err = %v, want ErrConfinementRefused", path, err)
			continue
		}
		var refused *RefusedError
		if !errors.As(err, &refused) || refused.Readiness.Cause != CauseAppArmorUserns {
			t.Errorf("%s: err = %v, want a RefusedError carrying the cause", path, err)
		}
		for _, want := range []string{"stub fix", "container tier", "microVM tier"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not contain %q", path, err, want)
			}
		}
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ran")); statErr == nil {
		t.Fatal("a command ran although the default refused")
	}
	if l.ConfinementFallbacks() != 0 || l.BestEffortConfinement() {
		t.Fatal("the default must record no fallback and report no opt-in")
	}
}

// TestBestEffortConfinementRunsAtTheFloorAndSaysSo is the opt-in side: the same refusing
// host runs the command at the process-jail floor, and every such run reaches the notice
// with the classified reason and is counted.
func TestBestEffortConfinementRunsAtTheFloorAndSaysSo(t *testing.T) {
	stubGate(t, refusing(CauseUserNamespacesDisabled))
	dir := t.TempDir()
	var notices []error
	l, err := NewLocal(dir, WithBestEffortConfinement(func(reason error) { notices = append(notices, reason) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if !l.BestEffortConfinement() {
		t.Fatal("the opt-in must be reported")
	}
	res, err := l.Exec(context.Background(), Command{Line: "touch ran"})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("the opt-in must run the command at the floor: res=%+v err=%v", res, err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ran")); statErr != nil {
		t.Fatalf("the command did not run: %v", statErr)
	}
	if l.ConfinementFallbacks() != 1 || len(notices) != 1 {
		t.Fatalf("fallbacks=%d notices=%d, want 1 of each", l.ConfinementFallbacks(), len(notices))
	}
	var refused *RefusedError
	if !errors.As(notices[0], &refused) || refused.Readiness.Cause != CauseUserNamespacesDisabled {
		t.Fatalf("notice = %v, want the classified refusal", notices[0])
	}
	if got := l.ConfinementTier(); got != "process-jail" {
		t.Fatalf("a Local running at the floor reports tier %q, want process-jail", got)
	}
}

// TestBestEffortConfinementNeverRelaxesNetworkDenial holds the opt-in to the axis it must
// not touch: a Local that denies the network still refuses when the host cannot enforce it.
func TestBestEffortConfinementNeverRelaxesNetworkDenial(t *testing.T) {
	stubGate(t, refusing(CauseAppArmorUserns))
	dir := t.TempDir()
	l, err := NewLocal(dir, WithBestEffortConfinement(func(error) {}), WithNetworkDenied())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	_, err = l.Exec(context.Background(), Command{Line: "touch ran"})
	if !errors.Is(err, ErrConfinementRefused) {
		t.Fatalf("err = %v, want the refusal: the opt-in must not open a denied network", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ran")); statErr == nil {
		t.Fatal("the command ran with its network denial unenforceable")
	}
}

// TestEnvironmentCannotEnableTheFallback sets every plausible switch in the environment and
// shows the default still refuses.
func TestEnvironmentCannotEnableTheFallback(t *testing.T) {
	for _, name := range []string{
		"FLYNN_SANDBOX_BEST_EFFORT", "FLYNN_SANDBOX_DISABLE", "FLYNN_SANDBOX_UNCONFINED",
		"FLYNN_SANDBOX_FLOOR", "FLYNN_NO_SANDBOX", "FLYNN_SANDBOX_CHECK_SKIP",
	} {
		t.Setenv(name, "1")
	}
	stubGate(t, refusing(CauseUnknown))
	dir := t.TempDir()
	l, err := NewLocal(dir, WithDefaultConfinement())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if _, err := l.Exec(context.Background(), Command{Line: "touch ran"}); !errors.Is(err, ErrConfinementRefused) {
		t.Fatalf("err = %v, want the refusal whatever the environment says", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ran")); statErr == nil {
		t.Fatal("an environment variable let a command run unconfined")
	}
}
