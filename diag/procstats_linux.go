//go:build linux

package diag

import "os"

// openFDs counts the process's open descriptors. /proc/self/fd holds one entry per
// descriptor, including the one the directory read itself opened, which is why the
// count is reduced by one.
//
// This reads one directory belonging to this process. There is deliberately no
// childProcs here: counting children by reading the ppid out of every /proc/<pid>/stat
// costs an open and a read per process on the machine, on every sample. The count comes
// from the spawners instead, through Config.Children.
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
