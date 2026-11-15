//go:build !windows

package sandbox

import "context"

// captureRun applies confinement through exec.Cmd.
func (l *Local) captureRun(ctx context.Context, spec CaptureSpec, confined bool) (ExecResult, error) {
	return l.captureExec(ctx, spec, confined)
}
