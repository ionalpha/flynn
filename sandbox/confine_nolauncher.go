//go:build !linux

package sandbox

// RunChildLaunchIfRequested is a no-op on every platform that does not use the
// re-exec confinement launcher (only Linux does). On those platforms this binary is
// never re-executed as a launcher, so there is nothing to intercept. It exists so the
// program's entry point can call it unconditionally regardless of platform.
func RunChildLaunchIfRequested() {}
