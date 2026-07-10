// Process-level counters the Go runtime does not expose: open descriptors and
// live child processes. Both are read from the operating system, so each platform
// supplies its own implementation and any platform that cannot answer returns
// Unknown rather than a plausible zero.
//
// These two counters exist because the agent's worst leaks are not Go leaks. A run
// that opens a file per turn and never closes it, or spawns a sandboxed command and
// never reaps it, shows a flat heap and a flat goroutine count right up to the
// moment it fails.

package diag

// Unknown is the value a counter takes on a platform that cannot report it. It is
// negative so that no reader can average, sum, or threshold it into a false zero.
const Unknown = -1
