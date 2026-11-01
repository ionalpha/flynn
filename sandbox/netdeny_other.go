//go:build !windows

package sandbox

// networkDenyEnforceable reports true: Linux unshares the network namespace and macOS
// denies network in the seatbelt profile, on every tier. A platform with neither refuses
// the launch for a network denial in its own confinement code.
func networkDenyEnforceable(bool) bool { return true }
