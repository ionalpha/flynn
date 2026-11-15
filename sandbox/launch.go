package sandbox

// A re-executed launcher must build its view and exec the command before any
// program logic. init covers every binary that can confine, because confining
// requires this import. An ordinary process returns at once.
func init() { RunChildLaunchIfRequested() }
