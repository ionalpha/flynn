// Package version holds build version metadata for the agent binary.
package version

import "runtime/debug"

// Version is the semantic version. Override at build time with:
//
//	go build -ldflags "-X github.com/ionalpha/flynn/internal/version.Version=v0.1.0" ./cmd/flynn
var Version = "0.0.0-dev"

// devVersion is the source default, present when no release version was stamped in.
const devVersion = "0.0.0-dev"

// IsDev reports whether this is an unstamped development build (no release version linked
// in). A dev build keeps its durable state apart from a release build's, so an
// in-progress schema change on a branch never touches a real installation.
func IsDev() bool { return Version == devVersion }

// String returns a human-readable version, appending the VCS revision when the
// binary was built from a git checkout and no explicit version was set.
func String() string {
	if Version != devVersion {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				rev := s.Value
				if len(rev) > 12 {
					rev = rev[:12]
				}
				return Version + "+" + rev
			}
		}
	}
	return Version
}
