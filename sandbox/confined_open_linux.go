//go:build linux

package sandbox

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// confinedOpen opens abs (already cleaned and within-root by resolve) atomically confined
// beneath the root. It resolves the path from a directory descriptor on the root with
// openat2 + RESOLVE_BENEATH, so the kernel refuses any component, including a symlink
// swapped in after resolve ran, that would leave the root. This removes the
// time-of-check/time-of-use window between validating a path and opening it.
//
// An escape refused by the kernel (an absolute symlink, a "..", or a symlink pointing out
// of the root) surfaces as ErrDenied, the same outcome resolve gives for a statically
// escaping path. On a kernel too old for openat2 it falls back to a non-following open.
func (l *Local) confinedOpen(abs string, flags int, perm os.FileMode) (*os.File, error) {
	rel, err := filepath.Rel(l.root, abs)
	if err != nil {
		return nil, ErrDenied
	}
	// openat2 resolves relative to dirfd; "." opens the root directory itself.
	rel = filepath.ToSlash(rel)

	dirfd, err := unix.Open(l.root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dirfd) }()

	// openat2 is stricter than open: a nonzero mode without O_CREATE (or O_TMPFILE) is
	// EINVAL, where open would just ignore it. Mirror open's leniency so a caller passing
	// a perm on a non-creating open is not rejected.
	var mode uint64
	if flags&(os.O_CREATE) != 0 {
		mode = uint64(perm.Perm())
	}
	// RESOLVE_NO_SYMLINKS refuses every symlink component, matching the non-following
	// open used on platforms without openat2 so confined file IO has one symlink policy
	// everywhere; RESOLVE_BENEATH additionally refuses a non-symlink escape (a ".." or an
	// absolute component). Together they make a swapped-in symlink or any other escape a
	// resolution failure rather than a followed redirect.
	how := &unix.OpenHow{
		Flags:   uint64(flags) | unix.O_CLOEXEC, //nolint:gosec // G115: flags are os.O_* open-mode bits, small and non-negative
		Mode:    mode,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(dirfd, rel, how)
	if err != nil {
		switch {
		case errors.Is(err, unix.EXDEV), errors.Is(err, unix.ELOOP):
			// The resolution tried to leave the root: a swapped-in or escaping symlink.
			return nil, ErrDenied
		case errors.Is(err, unix.ENOSYS):
			// Pre-5.6 kernel without openat2: fall back to a non-following open.
			return l.openNoFollow(abs, flags, perm)
		default:
			return nil, err
		}
	}
	return os.NewFile(uintptr(fd), abs), nil
}

// openNoFollow is the pre-openat2 fallback: it opens without following a terminal symlink
// and re-validates the opened path against the root. It is best-effort, used only on
// kernels that lack openat2.
func (l *Local) openNoFollow(abs string, flags int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(abs, flags|unix.O_NOFOLLOW, perm) //nolint:gosec // abs is confined to the sandbox root by resolve, opened O_NOFOLLOW, and re-validated post-open by revalidateOpened
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, ErrDenied
		}
		return nil, err
	}
	return l.revalidateOpened(f, abs)
}
