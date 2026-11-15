//go:build windows

package fsatomic

// syncDir is a no-op on Windows. The Go file API cannot fsync a directory, because
// FlushFileBuffers needs write access that os.Open on a directory does not grant,
// and NTFS metadata journaling covers the rename.
func syncDir(string) error { return nil }
