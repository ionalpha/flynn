//go:build !windows

package sandbox

import "context"

// startStream launches a streamed subprocess on every platform whose confinement can be
// expressed on an exec.Cmd (all but Windows), so the standard-library path applies the
// confinement directly. Windows confines through an AppContainer and overrides this with
// its own path.
func (l *Local) startStream(ctx context.Context, spec StreamSpec, confined bool) (*StreamProcess, error) {
	return l.startStreamExec(ctx, spec, confined)
}
