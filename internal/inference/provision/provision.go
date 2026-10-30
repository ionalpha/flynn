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

	// Variant names the build flavor within one runtime version and platform: empty for the
	// CPU baseline, otherwise "cuda-13.3", "vulkan" and so on. Each variant has its own
	// digest and its own install directory, so a CPU and a CUDA build of one version
	// coexist and neither is mistaken for the other.
	Variant string
	// Accel is the GPU API the variant needs; the zero value is the CPU baseline.
	Accel Accel
	// CUDA is the toolkit version a CUDA variant was built with. A driver that supports an
	// older CUDA cannot load it.
	CUDA inference.Version
	// MinComputeCap is the lowest CUDA compute capability the variant carries kernels for;
	// zero means no floor.
	MinComputeCap float64
	// Companions are further pinned archives unpacked beside the main one: the CUDA
	// runtime libraries upstream ships separately from the llama.cpp CUDA build.
	Companions []Companion
}

// Accel is the GPU API a runtime build offloads through.
type Accel int

const (
	// AccelNone is the CPU baseline, which runs everywhere.
	AccelNone Accel = iota
	// AccelCUDA needs an NVIDIA GPU and a driver at least as new as the build's CUDA.
	AccelCUDA
	// AccelVulkan runs on any GPU with a Vulkan driver, the fallback for AMD and Intel.
	AccelVulkan
)

// Companion is an additional pinned archive installed into the same directory as a
// release's main archive.
type Companion struct {
	// URL is the https source of the archive.
	URL string
	// SHA256 is the pinned digest the downloaded archive must match.
	SHA256 string
	// SizeBytes is the archive's known size, used as the download cap.
	SizeBytes int64
	// Archive is the archive's container format.
	Archive ArchiveKind
}

// Name is the variant as shown to a user: "cpu" for the baseline.
func (r Release) Name() string {
	if r.Variant == "" {
		return "cpu"
	}
	return r.Variant
}

// DownloadBytes is the total size of the archives an install of this release fetches.
func (r Release) DownloadBytes() int64 {
	n := r.SizeBytes
	for _, c := range r.Companions {
		n += c.SizeBytes
	}
	return n
}

// Dir is the directory under destDir the release installs into. The CPU baseline keeps
// the layout builds had before variants existed, so an install made by an earlier Flynn is
// still found; any other variant gets its own sibling.
func (r Release) Dir(destDir string) string {
	v := r.Version.String()
	if r.Variant != "" {
		v += "-" + r.Variant
	}
	return filepath.Join(destDir, r.Runtime, v)
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
	// Variant is the build flavor, empty for the CPU baseline (see [Release.Variant]).
	Variant string
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
	ar := acquire.Release{
		URL:       rel.URL,
		SHA256:    rel.SHA256,
		SizeBytes: rel.SizeBytes,
		Archive:   rel.Archive,
		BinName:   rel.BinName,
	}
	for _, c := range rel.Companions {
		ar.Companions = append(ar.Companions, acquire.Companion(c))
	}
	bin, reused, err := acquire.InstallTo(ctx, dl, ar, rel.Dir(destDir))
	if err != nil {
		return Installed{}, err
	}
	return Installed{BinPath: bin, Version: rel.Version, Variant: rel.Variant, FromCache: reused}, nil
}

// Lookup reports the build of rel already installed under destDir, without fetching
// anything. It finds exactly what [Install] places, because both locate it through
// [Release.Dir], so a command that asks whether a runtime is installed cannot disagree
// with the command that installed it.
func Lookup(rel Release, destDir string) (Installed, bool) {
	bin, ok := acquire.FindBinary(rel.Dir(destDir), rel.BinName)
	if !ok {
		return Installed{}, false
	}
	return Installed{BinPath: bin, Version: rel.Version, Variant: rel.Variant, FromCache: true}, true
}

// RuntimesDir is where a data directory keeps installed runtime builds.
func RuntimesDir(dataDir string) string { return filepath.Join(dataDir, "runtimes") }
