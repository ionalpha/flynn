// Package provision installs a local inference runtime that Flynn fetches itself, so a
// machine with no runtime can still run a model with no manual setup step. It pairs
// with the pure inference core: that package decides which runtime version is safe to
// run, this one obtains a pinned, safe build and places it on disk.
//
// Every byte it installs is verified. A runtime build is fetched from a pinned URL and
// checked against a pinned sha256 before anything is unpacked (the download path
// already refuses non-https and non-public hosts and caps the stream). The archive is
// then extracted with a path-traversal guard and a total-size ceiling, so a hostile or
// corrupt archive cannot write outside the install directory or exhaust the disk. The
// install is versioned and idempotent: a build already present is reused, never
// re-fetched.
//
// It does not run the runtime. Launching the installed binary, which is the
// code-execution surface a malicious model targets, is the caller's job and happens
// inside the sandbox. This package only guarantees the bytes on disk are the pinned,
// gate-approved build.
package provision

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/fetch"
	"github.com/ionalpha/flynn/inference"
)

// ArchiveKind is the container format a runtime release ships in. Both are handled with
// the standard library, so provisioning adds no decompression dependency.
type ArchiveKind int

const (
	// ArchiveZip is a .zip archive (the Windows release form).
	ArchiveZip ArchiveKind = iota
	// ArchiveTarGz is a gzip-compressed tar (the Linux and macOS release form).
	ArchiveTarGz
)

// Release is a single pinned runtime build for one OS and architecture: where to get
// it, the digest it must match, and which executable inside it is the server to run.
// A release is data, fixed at build time, so the set of builds Flynn will install is
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
	// example "llama-server" or "llama-server.exe"). Its sibling libraries are
	// extracted alongside it, so the located binary is runnable in place.
	BinName string
}

// maxExtractBytes caps the total uncompressed size written when extracting an archive,
// so a decompression bomb cannot fill the disk even though the compressed download was
// itself capped. Runtime builds are tens of megabytes; this leaves generous headroom.
const maxExtractBytes = 4 << 30 // 4 GiB

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
// path to its server binary. It is idempotent: a build already extracted at its
// versioned location is reused without a download. The release is gated before any
// network access, so a build that would be refused at run time is never fetched.
//
// The archive is downloaded to a temporary file, verified against the pinned digest by
// the download path, then extracted with a path-traversal guard and a size ceiling. On
// any failure nothing partial is left at the build's final location.
func Install(ctx context.Context, dl *fetch.Downloader, rel Release, destDir string) (Installed, error) {
	if err := rel.Gate(); err != nil {
		return Installed{}, err
	}
	buildDir := filepath.Join(destDir, rel.Runtime, rel.Version.String())
	if bin, ok := findBinary(buildDir, rel.BinName); ok {
		return Installed{BinPath: bin, Version: rel.Version, FromCache: true}, nil
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return Installed{}, fault.Wrap(fault.Terminal, "provision_dest", err)
	}

	tmpArchive, err := os.CreateTemp(destDir, ".runtime-*.archive")
	if err != nil {
		return Installed{}, fault.Wrap(fault.Terminal, "provision_tmp", err)
	}
	archivePath := tmpArchive.Name()
	_ = tmpArchive.Close()
	defer func() { _ = os.Remove(archivePath) }()

	if _, err := dl.Fetch(ctx, fetch.Request{
		URL:          rel.URL,
		Dest:         archivePath,
		ExpectSHA256: rel.SHA256,
		MaxBytes:     rel.SizeBytes + (1 << 20), // the pinned size plus a small margin
	}); err != nil {
		return Installed{}, err
	}

	// Extract into a sibling staging dir and move it into place only on success, so an
	// interrupted extraction never leaves a half-populated build at buildDir.
	if err := os.MkdirAll(filepath.Dir(buildDir), 0o750); err != nil {
		return Installed{}, fault.Wrap(fault.Terminal, "provision_mkdir", err)
	}
	staging, err := os.MkdirTemp(filepath.Dir(buildDir), ".staging-*")
	if err != nil {
		return Installed{}, fault.Wrap(fault.Terminal, "provision_stage", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := extract(rel.Archive, archivePath, staging); err != nil {
		return Installed{}, err
	}
	bin, ok := findBinary(staging, rel.BinName)
	if !ok {
		return Installed{}, fault.New(fault.Terminal, "provision_no_binary",
			"provision: "+rel.BinName+" not found in the "+rel.Runtime+" release archive")
	}
	//nolint:gosec // G302: the server binary must be executable to launch; it is the verified, pinned runtime build
	if err := os.Chmod(bin, 0o755); err != nil {
		return Installed{}, fault.Wrap(fault.Terminal, "provision_chmod", err)
	}

	if err := os.Rename(staging, buildDir); err != nil {
		return Installed{}, fault.Wrap(fault.Terminal, "provision_install", err)
	}
	finalBin, ok := findBinary(buildDir, rel.BinName)
	if !ok {
		return Installed{}, fault.New(fault.Terminal, "provision_missing", "provision: binary missing after install")
	}
	return Installed{BinPath: finalBin, Version: rel.Version}, nil
}

// extract unpacks the archive at src into destDir using the standard library for the
// kind, guarding against path traversal and oversized output. Only regular files and
// directories are written; anything else (a symlink, a device) is skipped, so an entry
// cannot redirect a later write outside destDir through a link.
func extract(kind ArchiveKind, src, destDir string) error {
	switch kind {
	case ArchiveZip:
		return extractZip(src, destDir)
	case ArchiveTarGz:
		return extractTarGz(src, destDir)
	default:
		return fault.New(fault.Terminal, "provision_archive", "provision: unknown archive kind")
	}
}

func extractZip(src, destDir string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fault.Wrap(fault.Terminal, "provision_zip", err)
	}
	defer func() { _ = zr.Close() }()
	var written int64
	for _, f := range zr.File {
		dst, ok := safeJoin(destDir, f.Name)
		if !ok {
			return traversalError(f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dst, 0o750); err != nil {
				return err
			}
			continue
		}
		if !f.Mode().IsRegular() {
			continue // skip symlinks and other non-regular entries
		}
		rc, err := f.Open()
		if err != nil {
			return fault.Wrap(fault.Terminal, "provision_zip_entry", err)
		}
		n, err := writeFile(dst, rc, maxExtractBytes-written)
		_ = rc.Close()
		if err != nil {
			return err
		}
		written += n
	}
	return nil
}

func extractTarGz(src, destDir string) error {
	f, err := os.Open(src) //nolint:gosec // G304: src is Flynn's own temp archive path, already digest-verified by the download
	if err != nil {
		return fault.Wrap(fault.Terminal, "provision_targz", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fault.Wrap(fault.Terminal, "provision_gzip", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	var written int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fault.Wrap(fault.Terminal, "provision_tar", err)
		}
		dst, ok := safeJoin(destDir, hdr.Name)
		if !ok {
			return traversalError(hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			n, err := writeFile(dst, tr, maxExtractBytes-written)
			if err != nil {
				return err
			}
			written += n
		default:
			// skip symlinks, hardlinks, devices: a link entry could redirect a write
			// outside destDir, and the runtime needs none of them.
		}
	}
	return nil
}

// writeFile copies at most limit bytes from r into a new file at dst, creating parent
// directories, and refuses once the extraction ceiling is reached. It returns the
// number of bytes written.
func writeFile(dst string, r io.Reader, limit int64) (int64, error) {
	if limit <= 0 {
		return 0, fault.New(fault.Terminal, "provision_too_big", "provision: archive exceeds the extraction size limit")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return 0, err
	}
	// 0o600: extracted files are owner-only; the server binary is made executable after.
	//nolint:gosec // G304: dst is confined under destDir by safeJoin before this write
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fault.Wrap(fault.Terminal, "provision_create", err)
	}
	defer func() { _ = out.Close() }()
	// Copy one byte past the limit so an entry that exactly fills the remaining budget
	// is still detected as overflowing the ceiling rather than silently truncated.
	n, err := io.Copy(out, io.LimitReader(r, limit+1))
	if err != nil {
		return n, fault.Wrap(fault.Terminal, "provision_copy", err)
	}
	if n > limit {
		return n, fault.New(fault.Terminal, "provision_too_big", "provision: archive exceeds the extraction size limit")
	}
	return n, nil
}

// safeJoin resolves an archive entry name under base and confirms the result stays
// within base. An entry that is absolute or walks out of the tree with ".." is rejected
// outright rather than re-rooted, so a hostile archive cannot place a file outside the
// install directory and cannot disguise its intent by relying on path collapsing. The
// containment is then re-checked against the resolved path as defense in depth.
func safeJoin(base, name string) (string, bool) {
	slashed := strings.ReplaceAll(name, `\`, "/")
	norm := path.Clean(slashed)
	if norm == "." || norm == ".." || strings.HasPrefix(norm, "../") || path.IsAbs(norm) || strings.Contains(name, ":") {
		return "", false
	}
	dst := filepath.Join(base, filepath.FromSlash(norm))
	rel, err := filepath.Rel(base, dst)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return dst, true
}

func traversalError(name string) error {
	return fault.New(fault.Forbidden, "provision_traversal",
		"provision: refusing archive entry that escapes the install directory: "+name)
}

// findBinary searches the extracted tree for a file named binName and returns its path.
// llama.cpp release archives place the server next to its shared libraries, at the root
// or under a build directory depending on the platform, so the binary is located by
// name rather than a fixed relative path.
func findBinary(root, binName string) (string, bool) {
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal to the search
		}
		if !d.IsDir() && d.Name() == binName {
			found = path
		}
		return nil
	})
	if found == "" {
		return "", false
	}
	return found, true
}
