//go:build linux

package sandbox

import (
	"os/exec"
	"syscall"
)

// denyNetwork configures cmd to run with no network access: it runs in a new network
// namespace, which starts with only a down loopback and no routes, so the command
// cannot make or accept any connection. The network namespace is nested inside a new
// user namespace and the running user is mapped to root inside it, so an unprivileged
// agent can create the isolation without real root on the host. The mappings change
// only what the command sees inside its namespace; it gains no privilege on the host.
func denyNetwork(c *exec.Cmd) error {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET
	c.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: syscall.Getuid(), Size: 1}}
	c.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: syscall.Getgid(), Size: 1}}
	// Writing "deny" to setgroups is required before an unprivileged gid mapping is
	// accepted; the Go runtime does this when this flag is false.
	c.SysProcAttr.GidMappingsEnableSetgroups = false
	return nil
}
