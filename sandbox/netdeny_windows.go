//go:build windows

package sandbox

// networkDenyEnforceable reports whether the tier can deny the network. The AppContainer
// does, by withholding the internetClient capability. The write-restricted token that
// hostReadable selects restricts writes only and has no network control, and a Windows
// Filtering Platform rule that would add one needs administrator rights.
func networkDenyEnforceable(hostReadable bool) bool { return !hostReadable }
