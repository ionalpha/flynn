//go:build !linux && !darwin

package sandbox

// readDenyEnforceable is false because the Windows write-restricted token checks
// writes only, so no access entry can stop a read by the user.
func readDenyEnforceable() bool { return false }
