//go:build !windows

package sandbox

// networkDenyEnforceable is true: Linux unshares the network namespace and macOS
// denies it in the seatbelt profile.
func networkDenyEnforceable(bool) bool { return true }
