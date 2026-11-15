//go:build windows

package sandbox

// networkDenyEnforceable is true for the AppContainer, which withholds
// internetClient. The write-restricted token has no network control, and a Windows
// Filtering Platform rule needs administrator rights.
func networkDenyEnforceable(hostReadable bool) bool { return !hostReadable }
