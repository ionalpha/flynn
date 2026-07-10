//go:build !windows

package sandbox

import "context"

// runShell runs a shell command through the standard library, applying this platform's
// in-process confinement via confine when confined is true. Every platform except
// Windows can express its confinement on an exec.Cmd, so the standard library runs the
// command directly. Windows confines a command by launching it inside an AppContainer,
// which cannot be expressed on an exec.Cmd, so it overrides this with its own path.
func (l *Local) runShell(ctx context.Context, name string, args []string, stdin []byte, confined bool) (ExecResult, error) {
	return l.runWithExecCmd(ctx, name, args, stdin, confined)
}

// closePlatform releases platform confinement state on Close. No platform but Windows
// leaves persistent state behind, so this is a no-op here.
func (l *Local) closePlatform() error {
	l.revokeReadableDirs()
	l.revokeWritableDirs()
	return nil
}

// revokeReadableDirs is a no-op off Windows: a read-only host already permits a confined
// child to read directories outside the workspace, so WithReadableDir grants nothing here
// and there is nothing to revoke. It stays defined so Close is uniform across platforms.
func (l *Local) revokeReadableDirs() { _ = l.readableDirs }

// revokeWritableDirs is a no-op off Windows. WithWritableDir does take effect here (a
// read-only host denies the write until the directory is granted), but the grant lives in
// the confinement the child runs under (a rw bind mount on Linux, a profile rule on
// macOS) rather than in a host access list, so it disappears with the process and leaves
// nothing to revoke. It stays defined so Close is uniform across platforms.
func (l *Local) revokeWritableDirs() { _ = l.writableDirs }
