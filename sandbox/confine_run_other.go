//go:build !windows

package sandbox

import "context"

// runShell runs a shell command through the standard library, applying this platform's
// in-process confinement via confine when confined is true. Every platform except
// Windows can express its confinement on an exec.Cmd, so the standard library runs the
// command directly. Windows confines a command by launching it inside an AppContainer,
// which cannot be expressed on an exec.Cmd, so it overrides this with its own path.
func (l *Local) runShell(ctx context.Context, name string, args []string, confined bool) (ExecResult, error) {
	return l.runWithExecCmd(ctx, name, args, confined)
}

// closePlatform releases platform confinement state on Close. No platform but Windows
// leaves persistent state behind, so this is a no-op here.
func (l *Local) closePlatform() error { return nil }
