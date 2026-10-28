//go:build darwin

package sandbox

// readDenyEnforceable reports whether this platform can hide a path from a confined
// command. macOS can: the seatbelt profile denies reads and writes under each hidden path
// (see seatbeltHidden).
func readDenyEnforceable() bool { return true }
