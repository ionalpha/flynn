//go:build !linux && !windows

package diag

// openFDs has no portable answer outside Linux and Windows. Darwin and the BSDs
// expose descriptor counts only through sysctl or libproc, neither of which is in
// the standard library, and this package adds no dependency to reach them.
func openFDs() int { return Unknown }

// childProcs has no portable answer outside Linux and Windows, for the same reason:
// the process table is reachable only through sysctl here.
func childProcs() int { return Unknown }
