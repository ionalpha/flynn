//go:build !windows

package sandbox

import "context"

// duplexConfinementExpressible is true because exec.Cmd carries the confinement.
func duplexConfinementExpressible() bool { return true }

// startSession applies confinement through exec.Cmd.
func (l *Local) startSession(ctx context.Context, spec SessionSpec, confined bool) (*Session, error) {
	return l.startSessionExec(ctx, spec, confined)
}
