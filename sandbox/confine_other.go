//go:build !linux

package sandbox

import (
	"os/exec"

	"github.com/ionalpha/flynn/fault"
)

// confine reports that kernel-enforced isolation is not available on this platform.
// When a Local was configured to deny the network or confine the filesystem, it
// fails rather than running the command without that isolation, so a caller that
// asked for confinement never silently gets an unconfined command. The platform's
// native isolation (its kernel-confinement adapter) provides the equivalent where it
// lands.
func (l *Local) confine(_ *exec.Cmd) error {
	if l.denyNetwork || l.readonlyFS {
		return fault.New(fault.Forbidden, "sandbox_confine_unsupported",
			"sandbox: kernel confinement (network and filesystem isolation) is not supported on this platform yet; refusing rather than running the command unconfined")
	}
	return nil
}

// RunChildLaunchIfRequested is a no-op on platforms without the re-exec launcher:
// filesystem confinement is unsupported here, so this binary is never re-executed as
// a confinement launcher. It exists so the program's entry point can call it
// unconditionally.
func RunChildLaunchIfRequested() {}
