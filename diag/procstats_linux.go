//go:build linux

package diag

import "os"

// openFDs counts the process's open descriptors from /proc/self/fd, less the one the
// directory read itself opened. There is no childProcs: reading every /proc/<pid>/stat
// costs a read per process on every sample, so Config.Children supplies the count.
func openFDs() int {
	f, err := os.Open("/proc/self/fd")
	if err != nil {
		return Unknown
	}
	defer func() { _ = f.Close() }()

	names, err := f.Readdirnames(-1)
	if err != nil {
		return Unknown
	}
	if n := len(names) - 1; n >= 0 {
		return n
	}
	return 0
}
