//go:build !windows

package sandbox

import "context"

// startStream applies confinement through exec.Cmd.
func (l *Local) startStream(ctx context.Context, spec StreamSpec, confined bool) (*StreamProcess, error) {
	return l.startStreamExec(ctx, spec, confined)
}
