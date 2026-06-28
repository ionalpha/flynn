package sandbox

// Confined opening closes a time-of-check/time-of-use window in the local file
// boundary. resolve validates a path by symlink-resolving its nearest existing ancestor
// and checking it stays within the root, then returns the cleaned, pre-symlink path. The
// subsequent open follows symlinks at open time, so between the check and the open a path
// component can be swapped to a symlink pointing outside the root and the open follows it
// out. In the agent model the code that could plant that symlink is the semi-trusted,
// model-authored process running in the working tree, so the race is reachable.
//
// confinedOpen closes it by resolving and opening the same object atomically. On Linux it
// uses openat2 with RESOLVE_BENEATH, which makes the kernel refuse a resolution that
// would leave the root, so no swap between check and open can redirect it. On platforms
// without that primitive it opens the final component without following a terminal
// symlink and re-validates the opened path against the root, which is best-effort
// hardening rather than an atomic guarantee. Either way, resolve still runs first as the
// cheap lexical and static-symlink check, so an obviously escaping path is denied before
// an fd is ever opened.

import (
	"os"
	"path/filepath"
)

// revalidateOpened confirms an already-opened file still resolves to a path within the
// root, closing it and returning ErrDenied if an intermediate symlink redirected the
// resolution outside. It is the post-open check the non-atomic platform paths layer on
// top of O_NOFOLLOW; the Linux openat2 path does not need it because the kernel enforced
// confinement during resolution.
func (l *Local) revalidateOpened(f *os.File, abs string) (*os.File, error) {
	if realPath, err := filepath.EvalSymlinks(abs); err == nil && !l.within(realPath) {
		_ = f.Close()
		return nil, ErrDenied
	}
	return f, nil
}
