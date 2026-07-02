//go:build windows

package fsatomic

// syncDir is a no-op on Windows: directories cannot be fsynced through the Go file
// API (FlushFileBuffers needs a handle with write access, which os.Open on a
// directory does not grant), and NTFS metadata journaling covers the rename.
func syncDir(string) error { return nil }
