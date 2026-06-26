package sandbox

// A process that was re-executed as a filesystem-confinement launcher has to finish
// building its confined view and exec the real command before it runs any normal
// program logic. Doing that here, in package init, covers every binary that can run a
// confined command: constructing a confined Local requires importing this package, so
// importing it is enough and no entry point has to remember to make the call. In an
// ordinary process (not a launcher) this returns immediately and startup proceeds.
func init() { RunChildLaunchIfRequested() }
