// Open descriptors: a process-level counter the Go runtime does not expose. It is read
// from the operating system, so each platform supplies its own implementation and any
// platform that cannot answer returns Unknown rather than a plausible zero. Every
// implementation reads only what belongs to this process, so its cost does not grow with
// the machine's process table.
//
// The counter exists because the agent's worst leaks are not Go leaks. A run that opens a
// file per turn and never closes it shows a flat heap and a flat goroutine count right up
// to the moment it fails. The other such leak, a spawned command that is never reaped, is
// counted by the spawners rather than read from the OS: see Config.Children.

package diag

// Unknown is the value a counter takes when it cannot be reported: a platform that does
// not expose it, or an application that supplied no way to read it. It is negative so
// that no reader can average, sum, or threshold it into a false zero.
const Unknown = -1
