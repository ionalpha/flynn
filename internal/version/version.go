// Package version holds build version metadata for the agent binary.
package version

import "runtime/debug"

// Version is the semantic version. Override at build time with:
//
//	go build -ldflags "-X github.com/ionalpha/flynn/internal/version.Version=v0.1.0" ./cmd/flynn
var Version = "0.0.0-dev"

// devVersion is the source default, present when no release version was stamped in.
const devVersion = "0.0.0-dev"

// released returns the release version, or "" for an unstamped development build.
// goreleaser sets Version through ldflags; a `go install <module>@<version>` build does
// not, so the module version in the build info is the fallback. Go's "(devel)" marks a
// local checkout build and counts as unversioned.
func released() string {
	if Version != devVersion {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		return moduleVersion(bi.Main.Version)
	}
	return ""
}

func moduleVersion(v string) string {
	if v == "" || v == "(devel)" {
		return ""
	}
	return v
}

// IsDev reports whether this is an unstamped development build. A dev build keeps its
// durable state apart from a release build's, so a schema change on a branch never
// touches a real installation. A `go install ...@<version>` build counts as a release.
func IsDev() bool { return released() == "" }

// Revision returns the short VCS revision the binary was built from, or "" when the
// toolchain stamped none (a released archive, a `go install`, VCS stamping disabled).
// Diagnostic artifacts record it so a profile can be read against its source.
func Revision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return revisionFrom(bi.Settings)
}

// revisionFrom returns the stamped VCS revision shortened to the 12 characters used for
// display, or "" when none was stamped.
func revisionFrom(settings []debug.BuildSetting) string {
	for _, s := range settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			if len(s.Value) > 12 {
				return s.Value[:12]
			}
			return s.Value
		}
	}
	return ""
}

// buildRevision is a variable so a test binary, which carries no VCS stamp, can
// exercise both forms of a development version string.
var buildRevision = Revision

// String returns the release version when present, otherwise the source default with
// the VCS revision appended when the binary was built from a git checkout.
func String() string {
	if v := released(); v != "" {
		return v
	}
	if rev := buildRevision(); rev != "" {
		return Version + "+" + rev
	}
	return Version
}
