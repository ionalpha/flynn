// Package fsatomic writes files so a reader never sees a partial file and a crash
// cannot lose the previous contents: write a sibling temp file, fsync it, rename it
// over the destination, then fsync the directory so the rename is durable. A rename
// without the fsyncs leaves the data in the page cache, lost on a crash.
package fsatomic

import (
	"io"
	"os"
	"path/filepath"
)

// tempFile is the part of *os.File the write path drives. It is an interface so a
// test can fail each step (a full disk on write, a handle lost before the sync),
// which a real filesystem will not do on demand; the previous contents must
// survive every one. Production always gets a real *os.File.
type tempFile interface {
	Name() string
	Write(p []byte) (int, error)
	Chmod(mode os.FileMode) error
	Sync() error
	Close() error
}

// createTemp opens the sibling temp file. It is a package variable because a failing
// write, chmod, sync or close cannot be provoked on a real filesystem, and each must
// leave the destination untouched.
var createTemp = func(dir, pattern string) (tempFile, error) { return os.CreateTemp(dir, pattern) }

// WriteFile atomically replaces path with data at perm. The temp file is created in
// the destination directory, because rename is atomic only within one filesystem,
// with owner-only access, then chmodded to perm before the rename, so the file never
// appears with looser access. On failure the temp file is removed and path keeps its
// previous contents.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	return WriteStream(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// WriteStream is WriteFile for produced content: fn writes into the temp file and
// the same sequence commits it, so a caller streaming megabytes (a profile dump, an
// encoder) need not buffer them first. An error from fn abandons the write and is
// returned unwrapped, so the caller's error vocabulary survives.
func WriteStream(path string, perm os.FileMode, fn func(io.Writer) error) error {
	return commit(path, perm, fn, func(tmp, dst string) (bool, error) {
		return true, os.Rename(tmp, dst)
	})
}

// CreateFile writes data to path at perm only if nothing is there yet. A reader sees
// either no file or all of it, and of several callers racing to create path exactly
// one succeeds; the others get an error matching [fs.ErrExist] and path keeps the
// winner's contents. The temp file is hard-linked into place, which fails rather than
// replaces when the name is taken, so a filesystem without hard links refuses every
// call with an error that does not match [fs.ErrExist].
func CreateFile(path string, data []byte, perm os.FileMode) error {
	return commit(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}, func(tmp, dst string) (bool, error) {
		// The link leaves the temp name behind; the deferred cleanup removes it.
		return false, os.Link(tmp, dst)
	})
}

// commit stages fn's output in a synced sibling temp file and has place put it at
// path. place reports whether it consumed the temp file; one that did not leaves it
// for removal. The directory is synced after a successful place so the new name is
// durable.
func commit(path string, perm os.FileMode, fn func(io.Writer) error, place func(tmp, dst string) (bool, error)) error {
	dir := filepath.Dir(path)
	tmp, err := createTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if err := fn(tmp); err != nil {
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	consumed, err := place(tmpName, path)
	if err != nil {
		return err
	}
	if consumed {
		tmpName = ""
	}
	return syncDir(dir)
}
