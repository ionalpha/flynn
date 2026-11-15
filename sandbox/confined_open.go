package sandbox

// Confined opening closes the race between resolve and open: the process in the
// working tree can swap a path component for a symlink out of the root.
//
// On Linux confinedOpen uses openat2 with RESOLVE_BENEATH, so the kernel refuses
// to leave the root. Elsewhere it opens with O_NOFOLLOW and re-validates, which
// is best effort. resolve still runs first, so an escaping path opens no fd.

import (
	"os"
	"path/filepath"
)

// revalidateOpened closes the file and returns ErrDenied if an intermediate
// symlink led outside the root. openat2 needs no such check.
func (l *Local) revalidateOpened(f *os.File, abs string) (*os.File, error) {
	if realPath, err := filepath.EvalSymlinks(abs); err == nil && !l.within(realPath) {
		_ = f.Close()
		return nil, ErrDenied
	}
	return f, nil
}

// confinedRel repeats the containment check at the point of use, so every open
// and mkdir gets a path [filepath.IsLocal] accepted. On Windows IsLocal also
// refuses device names such as NUL and COM1.
func (l *Local) confinedRel(abs string) (string, error) {
	rel, err := filepath.Rel(l.root, abs)
	if err != nil || !filepath.IsLocal(rel) {
		return "", ErrDenied
	}
	return rel, nil
}

// mkdirParents uses an [os.Root] because [os.MkdirAll] would follow a parent
// swapped for a symlink out of the root.
func (l *Local) mkdirParents(abs string) error {
	rel, err := l.confinedRel(filepath.Dir(abs))
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.MkdirAll(rel, 0o750)
}
