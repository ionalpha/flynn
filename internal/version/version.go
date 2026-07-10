// Package version holds build version metadata for the agent binary.
package version

import "runtime/debug"

// Version is the semantic version. Override at build time with:
//
//	go build -ldflags "-X github.com/ionalpha/flynn/internal/version.Version=v0.1.0" ./cmd/flynn
var Version = "0.0.0-dev"

// devVersion is the source default, present when no release version was stamped in.
const devVersion = "0.0.0-dev"

// released returns the release version this binary carries, or "" when it is an
// unstamped development build. A goreleaser build sets Version through ldflags; a
// `go install <module>@<version>` build does not, but Go records the module version in
// the build info, so that is used as the fallback. "(devel)" is Go's placeholder for a
// build from a local checkout and is treated as unversioned.
func released() string {
	if Version != devVersion {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		return moduleVersion(bi.Main.Version)
	}
	return ""
}

// moduleVersion returns v when it is a usable module version, or "" for an absent
// version or Go's "(devel)" placeholder (a build from a local checkout).
func moduleVersion(v string) string {
	if v == "" || v == "(devel)" {
		return ""
	}
	return v
}

// IsDev reports whether this is an unstamped development build (no release version linked
// in). A dev build keeps its durable state apart from a release build's, so an
// in-progress schema change on a branch never touches a real installation. A build
// installed with `go install ...@<version>` is a real release, not a dev build.
func IsDev() bool { return released() == "" }

// Revision returns the short VCS revision the binary was built from, or "" when it was
// not built from a checkout the toolchain could stamp (a released archive, a
// `go install`, or a build with VCS stamping disabled). A diagnostic artifact records it
// so a captured profile can be read against the source that produced it.
func Revision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			if len(s.Value) > 12 {
				return s.Value[:12]
			}
			return s.Value
		}
	}
	return ""
}

// String returns a human-readable version: the release version when one is present,
// otherwise the source default with the VCS revision appended when the binary was built
// from a git checkout.
func String() string {
	if v := released(); v != "" {
		return v
	}
	if rev := Revision(); rev != "" {
		return Version + "+" + rev
	}
	return Version
}
