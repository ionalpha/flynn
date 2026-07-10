//go:build !windows

package sandbox

import "time"

// CleanStaleProfiles removes nothing off Windows and reports so. Confinement on the other
// platforms registers no persistent operating-system object: a Linux sandbox lives in a
// mount and network namespace that dies with its process, and a macOS one is a profile
// passed to the launcher for the life of the command. Only the Windows AppContainer tier
// registers a profile that can outlive the sandbox that made it, so only Windows needs a
// collector. It stays defined on every platform so a caller can sweep on startup without
// writing a build tag.
func CleanStaleProfiles(time.Time) (int, error) { return 0, nil }

// LiveProfileCount is always zero off Windows, where no profile is ever registered.
func LiveProfileCount() int { return 0 }
