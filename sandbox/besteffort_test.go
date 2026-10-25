package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/netguard"
)

// TestFloorPermittedOnlyForTheExplicitOptIn pins who may run at the floor: nobody by
// default, and with the opt-in nobody who holds a network denial, governed egress or a
// refused read grant.
func TestFloorPermittedOnlyForTheExplicitOptIn(t *testing.T) {
	launchErr := errors.New("launch failed")
	optIn := WithBestEffortConfinement(func(error) {})
	cases := []struct {
		name string
		opts []LocalOption
		err  error
		want bool
	}{
		{"default", []LocalOption{WithDefaultConfinement()}, launchErr, false},
		{"no options", nil, launchErr, false},
		{"opt-in", []LocalOption{optIn}, launchErr, true},
		{"opt-in with network denial", []LocalOption{optIn, WithNetworkDenied()}, launchErr, false},
		{"opt-in with egress", []LocalOption{optIn, WithEgress(netguard.PublicOnly())}, launchErr, false},
		{"opt-in with refused read grant", []LocalOption{optIn}, ErrReadGrant, false},
		{"opt-in with a read-deny set", []LocalOption{optIn, WithUnreadablePaths(os.TempDir())}, launchErr, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newTestLocal(t, c.opts...)
			if got := l.floorPermitted(c.err); got != c.want {
				t.Fatalf("floorPermitted = %v, want %v", got, c.want)
			}
		})
	}
}

// TestFallBackUnexpressibleRecordsOnlyUnderTheOptIn covers the platforms that cannot
// confine a background process: the default refuses and counts nothing, the opt-in runs
// and says why.
func TestFallBackUnexpressibleRecordsOnlyUnderTheOptIn(t *testing.T) {
	reason := errors.New("cannot confine a background process")
	def := newTestLocal(t, WithDefaultConfinement())
	if def.fallBackUnexpressible(reason) || def.ConfinementFallbacks() != 0 || def.BestEffortConfinement() {
		t.Fatal("the default must neither fall back nor count one")
	}
	var got []error
	opted := newTestLocal(t, WithBestEffortConfinement(func(r error) { got = append(got, r) }))
	if !opted.BestEffortConfinement() {
		t.Fatal("the opt-in must be reported")
	}
	if !opted.fallBackUnexpressible(reason) || opted.ConfinementFallbacks() != 1 {
		t.Fatal("the opt-in must fall back and count it")
	}
	if len(got) != 1 || !errors.Is(got[0], reason) {
		t.Fatalf("notice got %v, want the reason once", got)
	}
}

// TestConfinementFallbackDecidedInOnePlace holds the guarantee on [floorPolicy]: the opt-in
// flag is touched in one file, that file never reads the environment, and no launch path
// carries a fallback condition of its own.
func TestConfinementFallbackDecidedInOnePlace(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f) //nolint:gosec // G304: names come from globbing this package directory.
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if f != "besteffort.go" && strings.Contains(src, "floor.optedIn") {
			t.Errorf("%s touches the opt-in flag; only besteffort.go may", f)
		}
		if f == "besteffort.go" && strings.Contains(src, "Getenv") {
			t.Errorf("besteffort.go reads the environment; the opt-in must be a code-level choice")
		}
		if f != "besteffort.go" && f != "local.go" && strings.Contains(src, "kernelConfinementEnforceable") {
			t.Errorf("%s decides on the confinement probe itself; use fallBack", f)
		}
	}
}

// TestDefaultConfinementRefusesWhereThePlatformCannotConfine covers a platform with no kernel
// confinement, where [WithDefaultConfinement] sets nothing: the Local refuses every launch
// path with the unsupported-platform code, and only the opt-in runs at the floor, counted.
func TestDefaultConfinementRefusesWhereThePlatformCannotConfine(t *testing.T) {
	if got := newTestLocal(t, WithDefaultConfinement()).confineUnsupported; got != !kernelConfinementSupported() {
		t.Fatalf("confineUnsupported = %v on a platform whose support is %v", got, kernelConfinementSupported())
	}

	def := newTestLocal(t, WithDefaultConfinement())
	def.confineUnsupported = true
	for name, err := range map[string]error{
		"guard": def.guardLaunch(),
		"exec":  execErr(def),
	} {
		if fault.CodeOf(err) != "sandbox_confine_unsupported" {
			t.Errorf("%s: err = %v, want sandbox_confine_unsupported", name, err)
		}
		if err != nil && !strings.Contains(err.Error(), "microVM tier") {
			t.Errorf("%s: %q does not point to the container or microVM tier", name, err)
		}
	}
	if def.ConfinementFallbacks() != 0 {
		t.Fatal("a refusal must not count as a fallback")
	}

	var got []error
	opted := newTestLocal(t, WithBestEffortConfinement(func(r error) { got = append(got, r) }))
	opted.confineUnsupported = true
	if err := opted.guardLaunch(); err != nil {
		t.Fatalf("the opt-in must pass the guard, got %v", err)
	}
	if opted.ConfinementFallbacks() != 1 || len(got) != 1 {
		t.Fatalf("fallbacks=%d notices=%d, want the opt-in counted and reported once", opted.ConfinementFallbacks(), len(got))
	}
}

func execErr(l *Local) error {
	_, err := l.Exec(context.Background(), Command{Line: "exit 0"})
	return err
}
