//go:build !linux && !darwin

package sandbox

import (
	"os/exec"

	"github.com/ionalpha/flynn/fault"
)

// kernelConfinementSupported reports whether this platform can enforce the network,
// filesystem, and syscall confinement. Linux and macOS have adapters that do; every
// other platform (including Windows until its AppContainer adapter lands) does not
// yet, so it reports false and confinement is refused rather than faked.
func kernelConfinementSupported() bool { return false }

// confine reports that kernel-enforced isolation is not available on this platform.
// When a Local was configured to deny the network, confine the filesystem, or filter
// syscalls, it fails rather than running the command without that isolation, so a
// caller that asked for confinement never silently gets an unconfined command. The
// platform's native confinement adapter provides the equivalent where it lands.
func (l *Local) confine(_ *exec.Cmd) error {
	if l.denyNetwork || l.readonlyFS || l.seccomp {
		return fault.New(fault.Forbidden, "sandbox_confine_unsupported",
			"sandbox: kernel confinement (network, filesystem, and syscall isolation) is not supported on this platform yet; refusing rather than running the command unconfined")
	}
	return nil
}
