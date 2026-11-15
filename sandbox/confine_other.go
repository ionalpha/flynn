//go:build !linux && !darwin && !windows

package sandbox

import (
	"os/exec"

	"github.com/ionalpha/flynn/fault"
)

// kernelConfinementSupported is false here, so confinement is refused.
func kernelConfinementSupported() bool { return false }

// egressEnforceable is false here, so a governed-egress launch refuses.
func egressEnforceable() bool { return false }

// backgroundConfinementExpressible is true because Serve goes through the same
// confine as Exec, which here refuses any confinement.
func backgroundConfinementExpressible() bool { return true }

// confine fails when any isolation was configured, so it never runs a command
// unconfined.
func (l *Local) confine(_ *exec.Cmd) error {
	if l.denyNetwork || l.readonlyFS || l.seccomp {
		return fault.New(fault.Forbidden, "sandbox_confine_unsupported",
			"sandbox: kernel confinement (network, filesystem, and syscall isolation) is not supported on this platform yet; refusing rather than running the command unconfined")
	}
	return nil
}

// platformConfinementTier is unreachable, because kernelConfinementSupported is
// false, so it names the floor.
func (l *Local) platformConfinementTier() string { return "process-jail" }
