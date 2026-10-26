//go:build !linux && !darwin

package sandbox

// readDenyEnforceable reports false. Windows confines a command with a write-restricted
// token, which checks its restricting identities on writes only, so no access entry can keep
// a read away from a command that runs as the user; a Local with a read-deny set refuses to
// launch.
func readDenyEnforceable() bool { return false }
