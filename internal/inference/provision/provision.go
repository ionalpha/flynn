// Package provision installs a local inference runtime that Flynn fetches itself, so a
// machine with no runtime can run a model without a manual setup step. Package
// inference decides which version is safe; this package obtains a pinned, gate-approved
// build and places it on disk, under a versioned directory so versions coexist. The
// generic acquire layer fetches, verifies, extracts and installs; this package adds the
// advisory gate before any network access. It never runs the runtime: launching the
// binary, the surface a malicious model targets, happens inside the sandbox.
package provision

import (
	"context"
	"path/filepath"

	"github.com/ionalpha/flynn/internal/acquire"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/inference"
)

// ArchiveKind is the container format a runtime release ships in, the same type the
// acquire layer uses.
type ArchiveKind = acquire.ArchiveKind

const (
	// ArchiveZip is a .zip archive (the Windows release form).
	ArchiveZip = acquire.ArchiveZip
	// ArchiveTarGz is a gzip-compressed tar (the Linux and macOS release form).
	ArchiveTarGz = acquire.ArchiveTarGz
)

// Release is one pinned runtime build for one OS and architecture: where to get it, the
// digest it must match, and which executable inside it is the server. Releases are
// fixed at build time, so the set of installable builds is auditable and cannot be
// redirected at runtime.
type Release struct {
	// Runtime matches inference.Runtime.Name.
	Runtime string
	// Version gates the build and names its install directory.
	Version inference.Version
	// GOOS and GOARCH are the platform this build targets, as Go names them.
	GOOS, GOARCH string
	// URL is the https source of the release archive.
	URL string
	// SHA256 is the pinned digest the downloaded archive must match.
	SHA256 string
	// SizeBytes is the archive's known size, used as the download cap.
	SizeBytes int64
	// Archive is the archive's container format.
	Archive ArchiveKind
	// BinName is the server executable inside the extracted archive, such as
	// "llama-server.exe". Its sibling libraries are extracted beside it, so it runs in
	// place.
	BinName string

	// Variant names the build flavor: empty for the CPU baseline, otherwise "cuda-13.3",
	// "vulkan" and so on. Each variant has its own digest and install directory, so a CPU
	// and a CUDA build of one version coexist.
	Variant string
	// Accel is the GPU API the variant needs; the zero value is the CPU baseline.
	Accel Accel
	// CUDA is the toolkit version a CUDA variant was built with. A driver that supports
	// only an older CUDA cannot load it.
	CUDA inference.Version
	// MinComputeCap is the lowest CUDA compute capability the variant has kernels for;
	// zero means no floor.
	MinComputeCap float64
	// Companions are further pinned archives unpacked beside the main one, such as the
	// CUDA runtime libraries upstream ships apart from the llama.cpp CUDA build.
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
// the layout from before variants existed, so an older Flynn's install is still found.
func (r Release) Dir(destDir string) string {
	v := r.Version.String()
	if r.Variant != "" {
		v += "-" + r.Variant
	}
	return filepath.Join(destDir, r.Runtime, v)
}

// Gate returns the version gate's verdict for this release, so a caller can refuse
// before fetching anything.
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

// Install ensures the release's build is present under destDir and returns its server
// binary. A build already at its versioned location is reused without a download. The
// release is gated before any network access, so a build that would be refused at run
// time is never fetched.
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

// Lookup reports the build of rel already installed under destDir, without fetching.
// It and [Install] both locate the build through [Release.Dir], so they cannot
// disagree about whether a runtime is installed.
func Lookup(rel Release, destDir string) (Installed, bool) {
	bin, ok := acquire.FindBinary(rel.Dir(destDir), rel.BinName)
	if !ok {
		return Installed{}, false
	}
	return Installed{BinPath: bin, Version: rel.Version, Variant: rel.Variant, FromCache: true}, true
}

// RuntimesDir is where a data directory keeps installed runtime builds.
func RuntimesDir(dataDir string) string { return filepath.Join(dataDir, "runtimes") }
