// Package provision installs a local inference runtime that Flynn fetches itself, so a
// machine with no runtime can still run a model with no manual setup step. It pairs with
// the pure inference core: that package decides which runtime version is safe to run,
// this one obtains a pinned, safe build and places it on disk.
//
// The mechanics of fetching, verifying, extracting, and atomically installing a pinned
// archive are the generic acquire layer's job; this package adds the model-runtime policy
// on top: a release is gated against the inference advisory floor before any network
// access, and a build is installed under a versioned directory so versions coexist.
//
// It does not run the runtime. Launching the installed binary, which is the
// code-execution surface a malicious model targets, is the caller's job and happens
// inside the sandbox. This package only guarantees the bytes on disk are the pinned,
// gate-approved build.
package provision

import (
	"context"
	"path/filepath"

	"github.com/ionalpha/flynn/internal/acquire"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/inference"
)

// ArchiveKind is the container format a runtime release ships in. Its values match
// acquire.ArchiveKind, so a runtime release maps to the generic acquire layer directly.
type ArchiveKind = acquire.ArchiveKind

const (
	// ArchiveZip is a .zip archive (the Windows release form).
	ArchiveZip = acquire.ArchiveZip
	// ArchiveTarGz is a gzip-compressed tar (the Linux and macOS release form).
	ArchiveTarGz = acquire.ArchiveTarGz
)

// Release is a single pinned runtime build for one OS and architecture: where to get it,
// the digest it must match, and which executable inside it is the server to run. A
// release is data, fixed at build time, so the set of builds Flynn will install is
// auditable and cannot be redirected at runtime.
type Release struct {
	// Runtime is the inference runtime this build is, matching inference.Runtime.Name.
	Runtime string
	// Version is the build's version, used to gate it and to name its install dir.
	Version inference.Version
	// GOOS and GOARCH are the platform this build targets (Go's runtime.GOOS/GOARCH).
	GOOS, GOARCH string
	// URL is the https source of the release archive.
	URL string
	// SHA256 is the pinned digest the downloaded archive must match.
	SHA256 string
	// SizeBytes is the archive's known size, used as the download cap.
	SizeBytes int64
	// Archive is the archive's container format.
	Archive ArchiveKind
	// BinName is the server executable to locate inside the extracted archive (for
	// example "llama-server" or "llama-server.exe"). Its sibling libraries are extracted
	// alongside it, so the located binary is runnable in place.
	BinName string
}

// Gate reports the error from the version gate for this release, or nil when the build
// is safe to run. A release should never be installed if it does not pass, so a caller
// can refuse before fetching anything.
func (r Release) Gate() error { return inference.SafeToRun(r.Runtime, r.Version) }

// Installed describes a runtime build present on disk after Install.
type Installed struct {
	// BinPath is the absolute path to the runnable server executable.
	BinPath string
	// Version is the build's version.
	Version inference.Version
	// FromCache is true when the build was already installed and was reused.
	FromCache bool
}

// Install ensures the release's runtime build is present under destDir and returns the
// path to its server binary. It is idempotent: a build already extracted at its versioned
// location is reused without a download. The release is gated before any network access,
// so a build that would be refused at run time is never fetched; the download, digest
// verification, traversal-guarded extraction, and atomic install are the acquire layer's.
func Install(ctx context.Context, dl *fetch.Downloader, rel Release, destDir string) (Installed, error) {
	if err := rel.Gate(); err != nil {
		return Installed{}, err
	}
	buildDir := filepath.Join(destDir, rel.Runtime, rel.Version.String())
	bin, reused, err := acquire.InstallTo(ctx, dl, acquire.Release{
		URL:       rel.URL,
		SHA256:    rel.SHA256,
		SizeBytes: rel.SizeBytes,
		Archive:   rel.Archive,
		BinName:   rel.BinName,
	}, buildDir)
	if err != nil {
		return Installed{}, err
	}
	return Installed{BinPath: bin, Version: rel.Version, FromCache: reused}, nil
}
