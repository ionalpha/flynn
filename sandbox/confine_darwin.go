//go:build darwin

package sandbox

import (
	"os"
	"os/exec"

	"github.com/ionalpha/flynn/fault"
)

// sandboxExec is the macOS sandbox launcher: it applies a profile to a command and
// then runs it under that confinement. It ships with the OS at a fixed path.
const sandboxExec = "/usr/bin/sandbox-exec"

// kernelConfinementSupported reports whether this platform can enforce the network,
// filesystem, and syscall confinement, which it can on macOS through the sandbox
// profile applied by confine.
func kernelConfinementSupported() bool { return true }

// confine applies the kernel-enforced isolation a Local was configured for to a
// command about to run, by wrapping it in the macOS sandbox launcher with a profile
// built from the same options the Linux adapter reads. With no options it does
// nothing. Network denial removes all socket access; filesystem confinement makes the
// host read-only except the working directory and a scratch area; syscall confinement
// refuses the privileged file operations a command in its own tree never needs. The
// profile and the launcher together are the macOS counterpart of the Linux network
// namespace, read-only mount view, and syscall filter.
//
// If the launcher is missing the command is refused rather than run unconfined, so a
// caller that asked for confinement never silently gets an unconfined command.
func (l *Local) confine(c *exec.Cmd) error {
	if !l.denyNetwork && !l.readonlyFS && !l.seccomp {
		return nil
	}
	if _, err := os.Stat(sandboxExec); err != nil {
		return fault.New(fault.Forbidden, "sandbox_confine_unsupported",
			"sandbox: the macOS sandbox launcher is not available; refusing rather than running the command unconfined")
	}
	profile := seatbeltProfile(l.root, l.denyNetwork, l.readonlyFS, l.seccomp)
	// Wrap the existing command: the launcher applies the profile, then execs the real
	// command (still c.Args from here on) under it. c.Path points at the launcher; the
	// original program name stays as the launcher's first non-flag argument.
	c.Args = append([]string{sandboxExec, "-p", profile}, c.Args...)
	c.Path = sandboxExec
	return nil
}
