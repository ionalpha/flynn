//go:build !windows

package sandbox

import "context"

// runShell uses exec.Cmd, which can carry confinement everywhere but Windows.
func (l *Local) runShell(ctx context.Context, name string, args []string, stdin []byte, confined bool) (ExecResult, error) {
	return l.runWithExecCmd(ctx, name, args, stdin, confined)
}

// closePlatform removes the scratch directory, the only host state left; grants
// end with each child's confinement.
func (l *Local) closePlatform() error {
	l.revokeReadableDirs()
	l.revokeWritableDirs()
	return l.removeScratch()
}

// revokeReadableDirs is a no-op because a read-only host is already readable, so
// WithReadableDir grants nothing here.
func (l *Local) revokeReadableDirs() { _ = l.readableDirs }

// revokeWritableDirs is a no-op because the grant is a bind mount on Linux or a
// profile rule on macOS, which ends with the process.
func (l *Local) revokeWritableDirs() { _ = l.writableDirs }
