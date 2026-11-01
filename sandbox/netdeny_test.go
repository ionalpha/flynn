package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestNetworkDeniedReportsWhatTheTierEnforces holds the rule the posture record rests on:
// a denial that was asked for is reported only where the tier enforces it. The default
// confinement on Windows is the write-restricted token, which has no network control, so
// there the request is not a denial; the AppContainer tier (WithKernelConfinement) and the
// other platforms enforce it.
func TestNetworkDeniedReportsWhatTheTierEnforces(t *testing.T) {
	defaultTier := NetworkDenySupported()
	cases := []struct {
		name string
		opts []LocalOption
		want bool
	}{
		{"nothing asked", nil, false},
		{"default confinement alone", []LocalOption{WithDefaultConfinement()}, false},
		{"default confinement with a denial", []LocalOption{WithDefaultConfinement(), WithNetworkDenied()}, defaultTier},
		{"default confinement, denial and acceptance", []LocalOption{WithDefaultConfinement(), WithNetworkDenied(), WithOpenNetworkWhereUndeniable()}, defaultTier},
		{"kernel confinement", []LocalOption{WithKernelConfinement()}, true},
		{"denial without a tier", []LocalOption{WithNetworkDenied()}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := newTestLocal(t, c.opts...).NetworkDenied(); got != c.want {
				t.Fatalf("NetworkDenied = %v, want %v", got, c.want)
			}
		})
	}
}

// TestNetworkDenyRefusesWhereUnenforceable covers the platform whose default tier cannot
// deny the network: the request refuses the launch on every path that launches, the opt-in
// to best-effort confinement does not lift it, and only the explicit acceptance lets the
// command run, reported as an open network.
func TestNetworkDenyRefusesWhereUnenforceable(t *testing.T) {
	if NetworkDenySupported() {
		t.Skip("this platform denies the network on the default tier; the confinement tests cover it")
	}
	refused := map[string][]LocalOption{
		"default":            {WithDefaultConfinement(), WithNetworkDenied()},
		"best-effort opt-in": {WithBestEffortConfinement(func(error) {}), WithNetworkDenied()},
	}
	for name, opts := range refused {
		t.Run(name, func(t *testing.T) {
			l := newTestLocal(t, opts...)
			if err := l.guardLaunch(); !errors.Is(err, ErrNetworkDeny) {
				t.Fatalf("guardLaunch = %v, want ErrNetworkDeny", err)
			}
			if _, err := l.Exec(context.Background(), Command{Line: "echo ran"}); !errors.Is(err, ErrNetworkDeny) {
				t.Fatalf("Exec = %v, want ErrNetworkDeny", err)
			}
		})
	}

	l := newTestLocal(t, WithDefaultConfinement(), WithNetworkDenied(), WithOpenNetworkWhereUndeniable())
	res, err := l.Exec(context.Background(), Command{Line: "echo ran"})
	if err != nil {
		t.Fatalf("Exec with the acceptance = %v, want it to run", err)
	}
	if !strings.Contains(res.Output, "ran") {
		t.Fatalf("output = %q, want the command's output", res.Output)
	}
	if l.NetworkDenied() {
		t.Fatal("an accepted open network must not be reported as denied")
	}
}
