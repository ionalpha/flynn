package notices

import "strings"

// Applies reports whether n covers Flynn version v: from AffectedFrom (inclusive) to
// FixedIn (exclusive), either bound open when empty.
//
// An unparseable, empty or all-zero v matches every notice. Those are development
// builds (an unstamped tree, a `go install` with no version, "0.0.0-dev"), which are
// the builds most likely to run unreleased code.
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

// parseVersion reads a dotted numeric version, ignoring a leading "v" and any
// pre-release or build suffix ("v1.2.3-rc1+deadbeef" is 1.2.3). [Applies] treats false
// as no constraint, so a malformed bound cannot hide a notice from everyone.
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
			// Stop before overflow makes the comparison wrong.
			if n > 1<<30 {
				return nil, false
			}
		}
		out = append(out, n)
	}
	return out, true
}

// isZero matches the development placeholder "0.0.0-dev".
func isZero(v []int) bool {
	for _, n := range v {
		if n != 0 {
			return false
		}
	}
	return true
}

// compare treats a missing trailing component as zero, so 1.2 equals 1.2.0.
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
