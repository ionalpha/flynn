// Package procexec constructs every child process Flynn starts through os/exec.
//
// On Windows, a console program started by a parent with no console (a launcher, a
// service, an editor task runner, a GUI host) gets a console of its own, shown as a
// window unless the launch asks for none. The window stays for the child's lifetime,
// one per command in a tool-heavy run. The request is a creation flag set before
// start, so it must be on every command, and one direct [os/exec.Command] call brings
// the windows back. internal/rigor fails the build on a direct call outside this
// package (TestChildProcessesAreStartedThroughProcexec), and only this package spells
// the flag.
//
// A command built here still inherits the parent's console when there is one; output,
// exit codes and pipes are unchanged. The editor handoff, which must reach the user's
// console, is an exemption in that gate instead of a variant here, because a variant
// without the flag is the mistake the gate stops.
package procexec

import (
	"context"
	"os/exec" //nolint:depguard // this package is the spawn point the rest of the module goes through
)

// Command is [os/exec.Command] configured to open no console window on Windows. On
// other platforms it is identical.
func Command(name string, arg ...string) *exec.Cmd {
	c := exec.Command(name, arg...) //nolint:gosec // the caller owns the vetting of name and arg, as with os/exec
	c.SysProcAttr = hideWindow(c.SysProcAttr)
	return c
}

// CommandContext is [Command] bound to ctx: the process is killed if ctx ends first,
// as with [os/exec.CommandContext].
func CommandContext(ctx context.Context, name string, arg ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, name, arg...) //nolint:gosec // the caller owns the vetting of name and arg, as with os/exec
	c.SysProcAttr = hideWindow(c.SysProcAttr)
	return c
}
