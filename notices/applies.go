package notices

import "strings"

// Applies reports whether n is about the Flynn version v.
//
// A notice applies from AffectedFrom (inclusive) up to FixedIn (exclusive). An empty
// AffectedFrom means every version from the beginning; an empty FixedIn means no release
// fixes it yet, so it applies to everything from AffectedFrom onwards. That last case is
// the one that matters most: an advisory published before the fix ships still has to
// reach people.
//
// An unparseable, empty, or all-zero running version matches every notice. Those are the
// development builds: an unstamped tree, a `go install` with no version recorded, and
// Flynn's own "0.0.0-dev" placeholder. The alternative would be for the one build most
// likely to be running unreleased code to be the one build that hears nothing.
func Applies(n Notice, v string) bool {
	running, ok := parseVersion(v)
	if !ok || isZero(running) {
		return true
	}
	if from, ok := parseVersion(n.AffectedFrom); ok && compare(running, from) < 0 {
		return false
	}
	if fixed, ok := parseVersion(n.FixedIn); ok && compare(running, fixed) >= 0 {
		return false
	}
	return true
}

// parseVersion reads a dotted numeric version, tolerating a leading "v" and ignoring any
// pre-release or build suffix ("v1.2.3-rc1+deadbeef" is 1.2.3). It reports false for
// anything it cannot read as at least one number, which the caller reads as "no
// constraint" rather than "no match", so a malformed bound in a feed can never silently
// hide a notice from everyone.
func parseVersion(s string) ([]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
		n := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				return nil, false
			}
			n = n*10 + int(r-'0')
			// A version component this large is not a version, and continuing would
			// overflow into a comparison that says something untrue.
			if n > 1<<30 {
				return nil, false
			}
		}
		out = append(out, n)
	}
	return out, true
}

// isZero reports whether every component is zero, which is what Flynn's development
// placeholder version parses to.
func isZero(v []int) bool {
	for _, n := range v {
		if n != 0 {
			return false
		}
	}
	return true
}

// compare orders two parsed versions component by component, treating a missing trailing
// component as zero, so 1.2 and 1.2.0 are the same version.
func compare(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var ai, bi int
		if i < len(a) {
			ai = a[i]
		}
		if i < len(b) {
			bi = b[i]
		}
		switch {
		case ai < bi:
			return -1
		case ai > bi:
			return 1
		}
	}
	return 0
}
