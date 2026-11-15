//go:build darwin

package sandbox

import (
	"os"
	"os/exec"

	"github.com/ionalpha/flynn/fault"
)

// sandboxExec ships with macOS at a fixed path.
const sandboxExec = "/usr/bin/sandbox-exec"

// kernelConfinementSupported is true through the seatbelt profile.
func kernelConfinementSupported() bool { return true }

// egressEnforceable is true because the seatbelt profile allows outbound only to
// the loopback proxy.
func egressEnforceable() bool { return true }

// backgroundConfinementExpressible is true because a seatbelt profile holds for a
// backgrounded process too, so Serve gets Exec's tier.
func backgroundConfinementExpressible() bool { return true }

// egressAddr returns "" without egress. The launch path releases the egress lock
// after starting the proxy, so this cannot deadlock.
func (l *Local) egressAddr() string {
	if l.egress == nil {
		return ""
	}
	l.egress.mu.Lock()
	defer l.egress.mu.Unlock()
	return l.egress.addr
}

// confine wraps c in sandbox-exec with a profile built from the options Linux
// reads: network denial removes socket access, a read-only host leaves the
// workspace and scratch writable, and the syscall rule refuses privileged file
// operations. A missing launcher refuses the command.
func (l *Local) confine(c *exec.Cmd) error {
	if !l.denyNetwork && !l.readonlyFS && !l.seccomp && l.egress == nil {
		return nil
	}
	if _, err := os.Stat(sandboxExec); err != nil {
		return fault.New(fault.Forbidden, "sandbox_confine_unsupported",
			"sandbox: the macOS sandbox launcher is not available; refusing rather than running the command unconfined")
	}
	// A read-only host leaves the child no temp or cache directory, so scratch is
	// granted.
	var writable []string
	if l.readonlyFS {
		var err error
		if writable, err = l.grantScratch(c); err != nil {
			return err
		}
	}
	profile := seatbeltProfile(l.root, l.denyNetwork, l.readonlyFS, l.seccomp, l.egressAddr(), writable...) +
		seatbeltHidden(l.unreadable)
	// The original program name becomes the launcher's first non-flag argument.
	c.Args = append([]string{sandboxExec, "-p", profile}, c.Args...)
	c.Path = sandboxExec
	return nil
}

// platformConfinementTier ignores WithHostReadable, because the seatbelt profile
// already leaves the host readable.
func (l *Local) platformConfinementTier() string { return "seatbelt" }
