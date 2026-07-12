package selfupdate

import (
	"strconv"
	"strings"
)

// Version is a parsed release version. Only the shape flynn actually tags is
// accepted: "v" followed by major.minor.patch, optionally a prerelease. A version
// this package cannot parse is never compared, because a comparison that quietly
// treats an unparseable version as zero is a downgrade attack with extra steps.
type Version struct {
	major, minor, patch int
	pre                 []string // the dot-separated prerelease identifiers, empty for a release
	raw                 string
}

// String returns the version as it was written.
func (v Version) String() string { return v.raw }

// IsPrerelease reports whether this version carries a prerelease suffix.
func (v Version) IsPrerelease() bool { return len(v.pre) > 0 }

// ParseVersion reads a version tag. It is strict on purpose.
func ParseVersion(s string) (Version, bool) {
	raw := s
	s = strings.TrimPrefix(s, "v")

	// Build metadata does not participate in precedence, and flynn does not tag with
	// it. Accepting it would mean two distinct tags that compare equal.
	if strings.Contains(s, "+") {
		return Version{}, false
	}

	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	v := Version{raw: raw}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		// A leading zero makes "01" and "1" two spellings of one version.
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return Version{}, false
		}
		switch i {
		case 0:
			v.major = n
		case 1:
			v.minor = n
		case 2:
			v.patch = n
		}
	}
	if hasPre {
		if pre == "" {
			return Version{}, false
		}
		v.pre = strings.Split(pre, ".")
		for _, id := range v.pre {
			if id == "" || strings.Trim(id, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-") != "" {
				return Version{}, false
			}
		}
	}
	return v, true
}

// Compare orders two versions by semantic-versioning precedence: -1 if v sorts before
// w, 0 if they are the same version, +1 if v sorts after w. A prerelease sorts before
// the release it leads to, so v0.2.0-rc.1 is older than v0.2.0, which is what makes
// "do not install something older than what I am running" mean the right thing.
func (v Version) Compare(w Version) int {
	for _, pair := range [][2]int{{v.major, w.major}, {v.minor, w.minor}, {v.patch, w.patch}} {
		if c := cmpInt(pair[0], pair[1]); c != 0 {
			return c
		}
	}
	switch {
	case len(v.pre) == 0 && len(w.pre) == 0:
		return 0
	case len(v.pre) == 0:
		return 1 // a release outranks any prerelease of the same core version
	case len(w.pre) == 0:
		return -1
	}
	for i := 0; i < len(v.pre) && i < len(w.pre); i++ {
		if c := cmpPreIdent(v.pre[i], w.pre[i]); c != 0 {
			return c
		}
	}
	// A longer prerelease outranks the prefix it extends: rc.1.1 comes after rc.1.
	return cmpInt(len(v.pre), len(w.pre))
}

// cmpPreIdent compares two prerelease identifiers. Numeric identifiers compare
// numerically and sort below alphanumeric ones, per the specification, so rc.9 comes
// before rc.10 rather than after it.
func cmpPreIdent(a, b string) int {
	an, aNum := numericIdent(a)
	bn, bNum := numericIdent(b)
	switch {
	case aNum && bNum:
		return cmpInt(an, bn)
	case aNum:
		return -1
	case bNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func numericIdent(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	return n, true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
