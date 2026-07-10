//go:build linux

package diag

import (
	"os"
	"strconv"
	"strings"
)

// openFDs counts the process's open descriptors. /proc/self/fd holds one entry per
// descriptor, including the one the directory read itself opened, which is why the
// count is reduced by one.
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

// childProcs counts live processes whose parent is this one, by reading the ppid
// field of every /proc/<pid>/stat. It counts direct children only: a grandchild
// whose parent exited is reparented away and is no longer this process's problem.
func childProcs() int {
	proc, err := os.Open("/proc")
	if err != nil {
		return Unknown
	}
	defer func() { _ = proc.Close() }()

	names, err := proc.Readdirnames(-1)
	if err != nil {
		return Unknown
	}

	self := os.Getpid()
	count := 0
	for _, name := range names {
		if _, err := strconv.Atoi(name); err != nil {
			continue // not a pid directory
		}
		ppid, ok := readPPID("/proc/" + name + "/stat")
		if ok && ppid == self {
			count++
		}
	}
	return count
}

// readPPID extracts the parent pid from a /proc/<pid>/stat line. The second field
// is the executable name in parentheses and may itself contain spaces and
// parentheses, so the scan starts after the final ')' rather than splitting the
// whole line. From there the fields are state, then ppid.
func readPPID(path string) (int, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // a fixed /proc path built from a digit-only directory name
	if err != nil {
		return 0, false
	}
	line := string(data)
	end := strings.LastIndexByte(line, ')')
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(line[end+1:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}
