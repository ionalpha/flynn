//go:build !windows

package sandbox

import "context"

// captureRun runs a program to completion on every platform whose confinement can be
// expressed on an exec.Cmd (all but Windows), so the standard-library path applies the
// confinement directly. Windows confines through an AppContainer and overrides this with
// its own path.
func (l *Local) captureRun(ctx context.Context, spec CaptureSpec, confined bool) (ExecResult, error) {
	return l.captureExec(ctx, spec, confined)
}
