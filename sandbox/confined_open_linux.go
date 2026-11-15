//go:build linux

package sandbox

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// confinedOpen opens abs with openat2 and RESOLVE_BENEATH from a root descriptor,
// so the kernel refuses any escaping component, including a symlink swapped in
// after resolve, which closes the check-to-open race. A refused escape returns
// ErrDenied. A kernel without openat2 gets a non-following open.
func (l *Local) confinedOpen(abs string, flags int, perm os.FileMode) (*os.File, error) {
	rel, err := l.confinedRel(abs)
	if err != nil {
		return nil, err
	}
	// Rebuilt from the checked relative path, so every open uses what confinedRel
	// accepted.
	abs = filepath.Join(l.root, rel)
	// "." opens the root directory itself.
	rel = filepath.ToSlash(rel)

	dirfd, err := unix.Open(l.root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dirfd) }()

	// openat2 returns EINVAL for a nonzero mode without O_CREATE or O_TMPFILE, where
	// open ignores it, so the mode is dropped for a non-creating open.
	var mode uint64
	if flags&os.O_CREATE != 0 {
		mode = uint64(perm.Perm())
	}
	// RESOLVE_NO_SYMLINKS matches the non-following open used elsewhere, so confined
	// IO has one symlink policy; RESOLVE_BENEATH also refuses ".." and absolute
	// components.
	how := &unix.OpenHow{
		Flags:   uint64(flags) | unix.O_CLOEXEC, //nolint:gosec // G115: flags are os.O_* open-mode bits, small and non-negative
		Mode:    mode,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(dirfd, rel, how)
	if err != nil {
		switch {
		case errors.Is(err, unix.EXDEV), errors.Is(err, unix.ELOOP):
			return nil, ErrDenied
		case errors.Is(err, unix.ENOSYS):
			// Pre-5.6 kernel without openat2.
			return l.openNoFollow(abs, flags, perm)
		default:
			return nil, err
		}
	}
	return os.NewFile(uintptr(fd), abs), nil
}

// openNoFollow is the best-effort fallback for kernels without openat2: it refuses
// a terminal symlink and re-validates the opened path against the root.
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
