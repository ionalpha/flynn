//go:build !windows

package sandbox

import "context"

// duplexConfinementExpressible reports whether this platform can apply its kernel
// confinement to a duplex background process. Every platform whose confinement is carried
// on an exec.Cmd (Linux and macOS) can, so a confined Session runs through the standard
// library exactly like a confined Stream; Windows overrides this to false.
func duplexConfinementExpressible() bool { return true }

// startSession launches a duplex session on every platform whose confinement can be
// expressed on an exec.Cmd, so the standard-library path applies the confinement directly.
func (l *Local) startSession(ctx context.Context, spec SessionSpec, confined bool) (*Session, error) {
	return l.startSessionExec(ctx, spec, confined)
}
