//go:build !linux && !windows

package sandbox

import (
	"errors"
	"os"
	"syscall"
)

// confinedOpen serves Unix without openat2. O_NOFOLLOW refuses a symlink swapped
// into the final component and re-validation catches a redirected intermediate
// one. This narrows the check-to-open race; only openat2 closes it.
func (l *Local) confinedOpen(abs string, flags int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(abs, flags|syscall.O_NOFOLLOW, perm) //nolint:gosec // abs is confined to the sandbox root by resolve, opened O_NOFOLLOW, and re-validated post-open by revalidateOpened
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, ErrDenied
		}
		return nil, err
	}
	return l.revalidateOpened(f, abs)
}
