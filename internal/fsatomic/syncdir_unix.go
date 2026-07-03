//go:build !windows

package fsatomic

import "os"

// syncDir flushes the directory entry so the rename that installed the new file
// survives a crash. Without it the file contents are durable but the name that
// points at them may not be.
func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- dir is a caller-supplied directory path, opened read-only to fsync its entry
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
