//go:build !linux

package sandbox

// readDenyEnforceable reports false: no other platform builds the private mount view that
// hiding a path needs, so a Local with a read-deny set refuses to launch.
func readDenyEnforceable() bool { return false }
