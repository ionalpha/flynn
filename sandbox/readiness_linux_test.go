//go:build linux

package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// stubGate replaces the check the gate runs for the length of a test and forgets what
// earlier tests let through, so each test starts from a gate that has verified nothing.
func stubGate(t *testing.T, fn func(context.Context, needs) Readiness) {
	t.Helper()
	reset := func() {
		gateMu.Lock()
		gatePassed = map[needs]time.Time{}
		gateMu.Unlock()
	}
	reset()
	prev := gateOverride
	gateOverride = fn
	t.Cleanup(func() {
		gateOverride = prev
		reset()
	})
}

func refusing(c Cause) func(context.Context, needs) Readiness {
	return func(context.Context, needs) Readiness {
		return Readiness{Cause: c, Stage: string(stageUser), Detail: "stub refusal", Fix: "stub fix", Alternative: alternativeTiers}
	}
}

// TestRequiredNamespacesAreTheCloneFlagsConfineSets holds the list the runner unit is
// built from to what the sandbox really asks the kernel for: every namespace flag a fully
// configured Local sets is named in the list, and nothing else is.
func TestRequiredNamespacesAreTheCloneFlagsConfineSets(t *testing.T) {
	l, err := NewLocal(t.TempDir(), withoutGate(), WithNetworkDenied(), WithReadOnlyFS(), WithSeccomp())
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("true")
	if err := l.confine(c); err != nil {
		t.Fatal(err)
	}
	names := map[uintptr]string{
		syscall.CLONE_NEWUSER: "user",
		syscall.CLONE_NEWNET:  "net",
		syscall.CLONE_NEWNS:   "mnt",
		syscall.CLONE_NEWPID:  "pid",
		syscall.CLONE_NEWIPC:  "ipc",
		syscall.CLONE_NEWUTS:  "uts",
	}
	flags := c.SysProcAttr.Cloneflags
	var got []string
	for bit, name := range names {
		if flags&bit != 0 {
			got = append(got, name)
			flags &^= bit
		}
	}
	if flags != 0 {
		t.Fatalf("confine sets clone flags %#x this test cannot name; add them to RequiredNamespaces and the runner unit", flags)
	}
	want := RequiredNamespaces()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("confine creates %v, RequiredNamespaces lists %v", got, want)
	}
}

// TestLauncherMessagesComeFromTheClassifierConstants holds the coupling the classifier
// depends on: it recognises a launcher failure by text, so the launcher must print that
// text from the same constants. A reworded launcher message that leaves them behind would
// turn a known refusal into CauseUnknown.
func TestLauncherMessagesComeFromTheClassifierConstants(t *testing.T) {
	for file, idents := range map[string][]string{
		"seccomp_linux.go": {"msgNoNewPrivs", "msgInstallFilter"},
		"confine_linux.go": {"msgMakePrivate"},
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, ident := range idents {
			if !strings.Contains(string(src), ident) {
				t.Errorf("%s does not use %s", file, ident)
			}
		}
	}
}

// TestGateRefusesBeforeAnyChildStarts proves the launch is stopped by the refusal, with
// its reason, and that nothing ran: the command would have created a file.
func TestGateRefusesBeforeAnyChildStarts(t *testing.T) {
	stubGate(t, refusing(CauseAppArmorUserns))
	dir := t.TempDir()
	l, err := NewLocal(dir, WithNetworkDenied(), WithReadOnlyFS(), WithSeccomp())
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.Exec(context.Background(), Command{Line: "touch ran"})
	if err == nil {
		t.Fatal("a host that refuses confinement must refuse the launch")
	}
	if !errors.Is(err, ErrConfinementRefused) {
		t.Fatalf("err = %v, want it to match ErrConfinementRefused", err)
	}
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Readiness.Cause != CauseAppArmorUserns {
		t.Fatalf("err = %v, want a RefusedError carrying the cause", err)
	}
	for _, want := range []string{"stub fix", "container tier", "microVM tier"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ran")); statErr == nil {
		t.Fatal("the command ran although the gate refused")
	}
}

// TestGateRefusesEveryExplicitShape holds each explicit way of asking for confinement to
// the gate, so none is a side door: network denial alone, the read-only view alone, the
// syscall filter alone.
func TestGateRefusesEveryExplicitShape(t *testing.T) {
	shapes := map[string][]LocalOption{
		"network denial": {WithNetworkDenied()},
		"read-only host": {WithReadOnlyFS()},
		"syscall filter": {WithSeccomp()},
		"all three":      {WithNetworkDenied(), WithReadOnlyFS(), WithSeccomp()},
	}
	for name, opts := range shapes {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			stubGate(t, func(ctx context.Context, n needs) Readiness {
				calls.Add(1)
				return refusing(CauseUnknown)(ctx, n)
			})
			dir := t.TempDir()
			l, err := NewLocal(dir, opts...)
			if err != nil {
				t.Fatal(err)
			}
			_, err = l.Exec(context.Background(), Command{Line: "touch ran"})
			if calls.Load() == 0 {
				t.Fatal("the gate was never consulted")
			}
			if !errors.Is(err, ErrConfinementRefused) {
				t.Fatalf("err = %v, want a confinement refusal", err)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "ran")); statErr == nil {
				t.Fatal("the command ran although the gate refused")
			}
		})
	}
}

// TestGateAsksForOnlyWhatTheLocalUses keeps the check from refusing a Local over a part
// of the sandbox it does not use.
func TestGateAsksForOnlyWhatTheLocalUses(t *testing.T) {
	tests := []struct {
		name string
		opts []LocalOption
		want needs
	}{
		{"network denial", []LocalOption{WithNetworkDenied()}, needs{net: true}},
		{"read-only host", []LocalOption{WithReadOnlyFS()}, needs{mount: true}},
		{"syscall filter", []LocalOption{WithSeccomp()}, needs{seccomp: true}},
		{"all three", []LocalOption{WithNetworkDenied(), WithReadOnlyFS(), WithSeccomp()}, needsAll},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, err := NewLocal(t.TempDir(), tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if got := l.hostNeeds(); got != tc.want {
				t.Fatalf("hostNeeds = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestGateRemembersPassesAndNeverRefusals proves a pass is checked once per shape while a
// refusal is re-checked on the next launch, so a repaired host recovers and a transient
// failure is not frozen in.
func TestGateRemembersPassesAndNeverRefusals(t *testing.T) {
	var calls atomic.Int32
	ready := false
	stubGate(t, func(ctx context.Context, n needs) Readiness {
		calls.Add(1)
		if ready {
			return Readiness{Ready: true}
		}
		return refusing(CauseUnknown)(ctx, n)
	})
	n := needs{seccomp: true}
	for range 3 {
		if err := requireConfinement(n); !errors.Is(err, ErrConfinementRefused) {
			t.Fatalf("err = %v, want a refusal", err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("a refusal was remembered: %d checks for 3 launches", calls.Load())
	}
	ready = true
	for range 3 {
		if err := requireConfinement(n); err != nil {
			t.Fatalf("err = %v after the host recovered", err)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("a pass was not remembered: %d checks, want 4", calls.Load())
	}
	ready = false
	if err := requireConfinement(n); err != nil {
		t.Fatalf("a remembered pass should stand for this process, got %v", err)
	}
	if err := requireConfinement(needs{net: true}); err == nil {
		t.Fatal("a pass for one shape must not admit another")
	}
}

// TestNoEnvironmentVariableTurnsConfinementOff sets every plausible off switch and shows
// the gate still refuses. The source scan in TestReadinessCodeReadsNoEnvironment is the
// structural half of this guarantee; this is the behavioural half.
func TestNoEnvironmentVariableTurnsConfinementOff(t *testing.T) {
	for _, name := range []string{
		"FLYNN_SANDBOX_CONFINE", "FLYNN_SANDBOX", "FLYNN_SANDBOX_DISABLE", "FLYNN_SANDBOX_OFF",
		"FLYNN_SANDBOX_SKIP", "FLYNN_SANDBOX_SKIP_CHECK", "FLYNN_NO_SANDBOX", "FLYNN_UNCONFINED",
		"FLYNN_ALLOW_UNCONFINED", "FLYNN_INSECURE", "SANDBOX_DISABLE", "CI",
	} {
		for _, val := range []string{"0", "1", "off", "false", "true", "skip", "unconfined"} {
			t.Setenv(name, val)
			stubGate(t, refusing(CauseUnknown))
			if err := requireConfinement(needsAll); !errors.Is(err, ErrConfinementRefused) {
				t.Fatalf("%s=%s: err = %v, want the refusal to stand", name, val, err)
			}
		}
	}
}

// TestOnlyTheReadinessCheckSetsTheGateBypass holds the one internal bypass to the check
// itself: [withoutGate] is the only writer of the flag, and no exported option uses it.
func TestOnlyTheReadinessCheckSetsTheGateBypass(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"readiness_linux.go": true, "confine_linux.go": true, "local.go": true}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || allowed[f] {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "selfTest") {
			t.Errorf("%s touches the gate bypass flag; only the readiness check may set it", f)
		}
	}
}

// TestCheckConfinementOnThisHost runs the real check. Where it refuses, the refusal is
// logged for the operator and the test skips, since a CI runner may forbid unprivileged
// user namespaces (the existing confinement tests skip there for the same reason).
func TestCheckConfinementOnThisHost(t *testing.T) {
	r := CheckConfinement(context.Background())
	if r.Ready {
		if r.Err() != nil || r.Cause != "" || r.Fix != "" {
			t.Fatalf("a ready result must carry no refusal: %+v", r)
		}
		return
	}
	t.Logf("this host refuses confinement: %v", r.Err())
	if r.Cause == "" || r.Fix == "" || r.Alternative == "" || r.Stage == "" {
		t.Fatalf("a refusal must name its cause, stage, fix and alternative: %+v", r)
	}
	t.Skip("kernel confinement unavailable on this host; the refusal above names why")
}
