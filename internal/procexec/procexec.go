// Package procexec constructs every child process Flynn starts through os/exec.
//
// On Windows a console program started by a parent that has no console (Flynn run from a
// launcher, a service, an editor's task runner, or any GUI host) is given a console of its
// own, and unless the launch asks for none that console is a window on the user's screen.
// The window flashes for as long as the child lives, and a tool-heavy agent run opens one
// per command. The request is a creation flag, set on the command before it starts, so it
// has to be on every command; a single call site that builds its command with
// [os/exec.Command] directly brings the windows back. The rigor package therefore fails
// the build on a direct call outside this package (see its TestChildProcessesAreStartedThroughProcexec),
// and this package is the only place the flag is spelled.
//
// A command built here still inherits the parent's console when there is one; the flag only
// withholds a new window, so output, exit codes and the pipes a caller wires are unchanged.
// The one start that must reach the user's console, the editor handoff, is the documented
// exemption in that gate rather than a variant here, because a variant that skips the flag
// is the mistake the gate exists to stop.
package procexec

import (
	"context"
	"os/exec" //nolint:depguard // this package is the spawn point the rest of the module goes through
)

// Command returns the command for name and its arguments, configured to open no console
// window on Windows. It is [os/exec.Command] with that one difference; on every other
// platform it is identical.
func Command(name string, arg ...string) *exec.Cmd {
	c := exec.Command(name, arg...) //nolint:gosec // the caller owns the vetting of name and arg, as with os/exec
	c.SysProcAttr = hideWindow(c.SysProcAttr)
	return c
}

// CommandContext is [Command] bound to ctx: the process is killed if ctx ends before it
// does, exactly as [os/exec.CommandContext].
func CommandContext(ctx context.Context, name string, arg ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, name, arg...) //nolint:gosec // the caller owns the vetting of name and arg, as with os/exec
	c.SysProcAttr = hideWindow(c.SysProcAttr)
	return c
}
