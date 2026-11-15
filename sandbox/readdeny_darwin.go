//go:build darwin

package sandbox

// readDenyEnforceable is true because seatbeltHidden denies all access under a path.
func readDenyEnforceable() bool { return true }
