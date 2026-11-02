//go:build linux

package sandbox

// PrivateProcAvailable lets the external test package ask the question the launch path
// asks, so a test of the /proc isolation can skip by name on a host that cannot have it.
var PrivateProcAvailable = privateProcAvailable
