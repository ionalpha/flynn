//go:build !windows

package sandbox

import "time"

// CleanStaleProfiles removes nothing, because Linux namespaces and macOS profiles
// end with the process. It exists so a caller can sweep without a build tag.
func CleanStaleProfiles(time.Time) (int, error) { return 0, nil }

// LiveProfileCount is always zero off Windows.
func LiveProfileCount() int { return 0 }
