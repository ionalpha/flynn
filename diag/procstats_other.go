//go:build !linux && !windows

package diag

// openFDs is Unknown outside Linux and Windows: Darwin and the BSDs expose the count
// only through sysctl or libproc, and this package adds no dependency for them.
func openFDs() int { return Unknown }
