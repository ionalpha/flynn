//go:build !linux && !windows

package sandbox

import (
	"errors"
	"os"
	"syscall"
)

// confinedOpen opens abs without following a terminal symlink and re-validates the opened
// path against the root. This is the portable hardening for Unix platforms without
// openat2 (macOS, the BSDs): O_NOFOLLOW refuses a symlink swapped into the final
// component, the most direct escape, and the post-open re-validation catches an
// intermediate component that was redirected outside the root. It narrows the
// time-of-check/time-of-use window rather than closing it atomically the way the Linux
// openat2 path does.
func (l *Local) confinedOpen(abs string, flags int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(abs, flags|syscall.O_NOFOLLOW, perm) //nolint:gosec // abs is confined to the sandbox root by resolve, opened O_NOFOLLOW, and re-validated post-open by revalidateOpened
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			// A symlink occupied the final component: refuse rather than follow it out.
			return nil, ErrDenied
		}
		return nil, err
	}
	return l.revalidateOpened(f, abs)
}
