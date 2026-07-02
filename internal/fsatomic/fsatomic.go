// Package fsatomic writes files so a reader never observes a partial file and a
// crash cannot lose the previous contents: write to a sibling temp file, fsync it,
// rename over the destination, then fsync the directory so the rename itself is
// durable. The rename-only idiom used before this package left a window where the
// data sat in the page cache with no durability at all.
package fsatomic

import (
	"os"
	"path/filepath"
)

// WriteFile atomically replaces path with data at the given permissions. The temp
// file is created in the destination directory (rename is only atomic within one
// filesystem) with owner-only access, then chmodded to perm before the rename, so
// the final mode never appears with looser intermediate access. On any failure the
// temp file is removed and the previous contents of path are untouched.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
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

	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // committed; nothing to clean up
	return syncDir(dir)
}
