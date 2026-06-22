// Package version holds build version metadata for the agent binary.
package version

import "runtime/debug"

// Version is the semantic version. Override at build time with:
//
//	go build -ldflags "-X github.com/ionalpha/flynn/internal/version.Version=v0.1.0" ./cmd/flynn
var Version = "0.0.0-dev"

// String returns a human-readable version, appending the VCS revision when the
// binary was built from a git checkout and no explicit version was set.
func String() string {
	if Version != "0.0.0-dev" {
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
