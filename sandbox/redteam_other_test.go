//go:build !linux && !darwin && !windows

package sandbox

// redteamEscapes on a platform with no kernel-confinement adapter probes only the
// filesystem-write axis, which the process jail tier exercises. The confined tiers are
// reported unenforceable on such a host and their cells skip, so the matrix stays honest:
// it never asserts a containment the platform cannot provide.
func redteamEscapes() []redteamEscape {
	return []redteamEscape{
		fsWriteEscape(),
	}
}
