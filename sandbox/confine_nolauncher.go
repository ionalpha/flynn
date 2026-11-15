//go:build !linux

package sandbox

// RunChildLaunchIfRequested is a no-op, because only Linux re-executes a launcher.
func RunChildLaunchIfRequested() {}
