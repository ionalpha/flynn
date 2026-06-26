package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// This file is the red-team containment matrix: it proves that each isolation tier
// actually denies what its declared containment claims, by running real escape attempts
// against the real Local adapter and asserting the outcome. An axis a tier claims to
// confine must contain its escape; an axis a tier does not claim must let the escape
// through (so the matrix also catches a tier that silently fails to confine, and one that
// blocks work it never promised to). A regression that opens a contained axis fails here,
// which is the build-time guard behind the containment promise.
//
// The escape probes are platform-specific (each platform enforces confinement with its
// own primitives), so each platform file supplies redteamEscapes for its adapter; the
// tiers, the matrix runner, and the filesystem-write escape are shared. Axes a platform
// does not probe are simply absent from its escape list. Resource and fork-bomb limits
// are deliberately not asserted here: that confinement is not built yet, so claiming it
// would be a false promise; it joins the matrix when the resource-limit tier lands.

// containAxis names a containment dimension the matrix probes.
type containAxis int

const (
	// axisFSWrite is writing to a path outside the working-directory grant.
	axisFSWrite containAxis = iota
	// axisSyscall is making a syscall the filter is meant to deny.
	axisSyscall
	// axisEgress is opening an outbound network connection.
	axisEgress
)

func (a containAxis) String() string {
	switch a {
	case axisFSWrite:
		return "filesystem-write"
	case axisSyscall:
		return "syscall"
	case axisEgress:
		return "network-egress"
	default:
		return "axis"
	}
}

// redteamTier is one isolation configuration and the set of axes it claims to confine.
type redteamTier struct {
	name     string
	opts     []LocalOption
	confines map[containAxis]bool
}

// redteamTiers are the Local tiers the matrix exercises, shared across platforms: the
// bare process jail confines none of the probed axes, the kernel-confined tier adds
// filesystem-write and syscall confinement, and denying the network adds egress on top.
func redteamTiers() []redteamTier {
	return []redteamTier{
		{name: "process-jail", confines: map[containAxis]bool{}},
		{
			name:     "kernel-confined",
			opts:     []LocalOption{WithReadOnlyFS(), WithSeccomp()},
			confines: map[containAxis]bool{axisFSWrite: true, axisSyscall: true},
		},
		{
			name:     "kernel-confined+egress-denied",
			opts:     []LocalOption{WithReadOnlyFS(), WithSeccomp(), WithNetworkDenied()},
			confines: map[containAxis]bool{axisFSWrite: true, axisSyscall: true, axisEgress: true},
		},
	}
}

// redteamEscape is one adversarial probe against one axis. run executes the escape under
// sb (rooted at root, with outside a sibling directory outside the grant) and reports
// whether the action was contained (prevented), or a non-empty skip when the host cannot
// enforce or distinguish the confinement (a developer box with no network or no
// unprivileged namespaces). want is whether this tier claims to confine the escape's axis,
// which lets a probe skip rather than misjudge when it cannot tell a real denial from a
// host that simply cannot perform the action (an unconfirmable allow path).
type redteamEscape struct {
	name string
	axis containAxis
	run  func(t *testing.T, sb *Local, root, outside string, want bool) (contained bool, skip string)
}

// TestContainmentRedTeamMatrix runs every escape against every tier and asserts the
// outcome matches the tier's declared confinement.
func TestContainmentRedTeamMatrix(t *testing.T) {
	for _, tr := range redteamTiers() {
		for _, esc := range redteamEscapes() {
			t.Run(tr.name+"/"+esc.name, func(t *testing.T) {
				if !tierEnforceable(t, tr) {
					t.Skipf("this host cannot enforce the %q tier; the confinement falls back to the floor here", tr.name)
				}
				root := t.TempDir()
				outside := t.TempDir()
				sb, err := NewLocal(root, tr.opts...)
				if err != nil {
					t.Fatalf("build %q: %v", tr.name, err)
				}
				defer func() { _ = sb.Close() }()

				want := tr.confines[esc.axis]
				contained, skip := esc.run(t, sb, root, outside, want)
				if skip != "" {
					t.Skip(skip)
				}
				if contained != want {
					verb := "let through"
					if want {
						verb = "contained"
					}
					t.Fatalf("tier %q must have %s the %s escape %q, but contained=%v", tr.name, verb, esc.axis, esc.name, contained)
				}
			})
		}
	}
}

// tierEnforceable reports whether this host actually sets up the tier's confinement. The
// kernel confinements fail loudly where the platform cannot provide them (for example
// where unprivileged user namespaces are restricted), so a benign command that errors
// means the tier is not enforceable here and its escapes must be skipped rather than
// judged against a floor fallback. The process jail is always enforceable.
func tierEnforceable(t *testing.T, tr redteamTier) bool {
	t.Helper()
	if len(tr.opts) == 0 {
		return true
	}
	// A platform that provides no kernel confinement at all runs the command at the
	// floor, so its confined tiers are never enforceable and their escapes must be
	// skipped rather than judged against a no-op confinement.
	if !kernelConfinementSupported() {
		return false
	}
	sb, err := NewLocal(t.TempDir(), tr.opts...)
	if err != nil {
		t.Fatalf("build %q for enforceability probe: %v", tr.name, err)
	}
	defer func() { _ = sb.Close() }()
	_, err = sb.Exec(context.Background(), Command{Line: "echo ok"})
	return err == nil
}

// fsWriteEscape attempts to write a file outside the working-directory grant. It is shared
// across platforms because the probe (a redirected echo) and the verdict (did the file
// appear outside the grant) are the same everywhere: containment means no file was
// written outside the working tree, regardless of the command's own exit code.
func fsWriteEscape() redteamEscape {
	return redteamEscape{
		name: "write-outside-workdir",
		axis: axisFSWrite,
		run: func(_ *testing.T, sb *Local, _, outside string, _ bool) (bool, string) {
			target := filepath.Join(outside, "escape.txt")
			if _, err := sb.Exec(context.Background(), Command{Line: "echo x > " + target}); err != nil {
				return false, "exec failed before the write could be judged: " + err.Error()
			}
			_, statErr := os.Stat(target)
			return os.IsNotExist(statErr), ""
		},
	}
}

// egressEscape attempts an outbound connection with the given platform probe. The verdict
// is want-aware to stay honest about the network's own unreliability: under a tier that
// denies egress, a failed connect is containment, but it is only trusted once an
// unconfined connect confirms the network exists to be denied; under a tier that allows
// egress, a failed connect cannot be told apart from a real denial, so it skips rather
// than falsely reporting containment. A successful connect under a deny tier is a real
// escape. The probe must exit non-zero when the connection cannot be made.
func egressEscape(probe string) redteamEscape {
	return redteamEscape{
		name: "outbound-connect",
		axis: axisEgress,
		run: func(t *testing.T, sb *Local, _, _ string, want bool) (bool, string) {
			res, err := sb.Exec(context.Background(), Command{Line: probe})
			if err != nil {
				return false, "egress probe exec failed: " + err.Error()
			}
			connected := res.ExitCode == 0
			if !want {
				// This tier should allow egress. A failed connect here is indistinguishable
				// from a host with no outbound path, so it cannot confirm the allow path.
				if !connected {
					return false, "could not confirm the allow path: no outbound connection under this tier"
				}
				return false, ""
			}
			// This tier should deny egress. A successful connect is a real escape; a failed
			// connect is containment only if the network exists unconfined to be denied.
			if connected {
				return false, ""
			}
			open, err := NewLocal(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if ores, oerr := open.Exec(context.Background(), Command{Line: probe}); oerr != nil || ores.ExitCode != 0 {
				return false, "host has no outbound network unconfined; cannot judge the denial"
			}
			return true, ""
		},
	}
}
