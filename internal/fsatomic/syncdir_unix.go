//go:build !windows

package fsatomic

import "os"

// syncDir flushes the directory entry so the rename that installed the new file
// survives a crash. Without it the file contents are durable but the name that
// points at them may not be.
func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- opens the caller's own parent directory, only to fsync it
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
