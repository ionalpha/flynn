// Package acquire obtains a pinned binary artifact and places it on disk, verified. It
// is the content-agnostic core shared by anything that has to fetch and install an
// external binary: a local inference runtime, an external command-line dependency. It
// downloads a pinned archive over the hardened fetch path, verifies its digest, extracts
// it with a path-traversal guard and a total-size ceiling, installs it atomically into a
// target directory, and locates the wanted executable inside it.
//
// It knows nothing about what the binary is or whether it is safe to run: the caller
// applies its own version gate before calling, and the caller runs the binary inside the
// sandbox afterward. This package only guarantees that the bytes installed at the target
// are the pinned, digest-verified release and that a hostile or corrupt archive can
// neither write outside the target nor exhaust the disk.
package acquire

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
	"github.com/ionalpha/flynn/internal/fetch"
)

// ArchiveKind is the container format a release ships in. Both are handled with the
// standard library, so acquiring a binary adds no decompression dependency.
type ArchiveKind int

const (
	// ArchiveZip is a .zip archive (the common Windows release form).
	ArchiveZip ArchiveKind = iota
	// ArchiveTarGz is a gzip-compressed tar (the common Linux and macOS release form).
	ArchiveTarGz
)

// maxExtractBytes caps the total uncompressed size written when extracting an archive,
// so a decompression bomb cannot fill the disk even though the compressed download was
// itself capped. Real release archives are tens of megabytes; this leaves headroom.
const maxExtractBytes = 4 << 30 // 4 GiB

// Release is a single pinned artifact for one platform: where to get it, the digest it
// must match, its container format, and which executable inside it the caller wants. A
// release is data, fixed at build time, so the set of artifacts that can be installed is
// auditable and cannot be redirected at runtime.
type Release struct {
	// URL is the https source of the release archive.
	URL string
	// SHA256 is the pinned digest the downloaded archive must match.
	SHA256 string
	// SizeBytes is the archive's known size, used as the download cap.
	SizeBytes int64
	// Archive is the archive's container format.
	Archive ArchiveKind
	// BinName is the executable to locate inside the extracted archive (for example
	// "flynn" or "flyctl.exe"). Its sibling files are extracted alongside it.
	BinName string
}

// InstallTo ensures rel's binary is present at targetDir and returns the absolute path
// to it. It is idempotent: if the binary is already present at targetDir it is reused
// without a download and reused is true. targetDir should be a version-specific path so
// a new version installs alongside an old one rather than over it.
//
// The archive is downloaded to a temporary file next to targetDir, verified against the
// pinned digest by the download path, then extracted into a sibling staging directory
// and moved into place only on success, so an interrupted or corrupt install never
// leaves a half-populated directory at targetDir.
func InstallTo(ctx context.Context, dl *fetch.Downloader, rel Release, targetDir string) (binPath string, reused bool, err error) {
	if bin, ok := FindBinary(targetDir, rel.BinName); ok {
		return bin, true, nil
	}
	parent := filepath.Dir(targetDir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return "", false, fault.Wrap(fault.Terminal, "acquire_dest", err)
	}

	tmpArchive, err := os.CreateTemp(parent, ".acquire-*.archive")
	if err != nil {
		return "", false, fault.Wrap(fault.Terminal, "acquire_tmp", err)
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
		return "", false, err
	}

	staging, err := os.MkdirTemp(parent, ".staging-*")
	if err != nil {
		return "", false, fault.Wrap(fault.Terminal, "acquire_stage", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := extract(rel.Archive, archivePath, staging); err != nil {
		return "", false, err
	}
	bin, ok := FindBinary(staging, rel.BinName)
	if !ok {
		return "", false, fault.New(fault.Terminal, "acquire_no_binary",
			"acquire: "+rel.BinName+" not found in the release archive")
	}
	//nolint:gosec // G302: the binary must be executable to run; it is the verified, pinned release
	if err := os.Chmod(bin, 0o755); err != nil {
		return "", false, fault.Wrap(fault.Terminal, "acquire_chmod", err)
	}

	if err := os.Rename(staging, targetDir); err != nil {
		return "", false, fault.Wrap(fault.Terminal, "acquire_install", err)
	}
	finalBin, ok := FindBinary(targetDir, rel.BinName)
	if !ok {
		return "", false, fault.New(fault.Terminal, "acquire_missing", "acquire: binary missing after install")
	}
	return finalBin, false, nil
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
		return fault.New(fault.Terminal, "acquire_archive", "acquire: unknown archive kind")
	}
}

func extractZip(src, destDir string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fault.Wrap(fault.Terminal, "acquire_zip", err)
	}
	defer func() { _ = zr.Close() }()
	var written int64
	for _, f := range zr.File {
		dst, ok := SafeJoin(destDir, f.Name)
		if !ok {
			return TraversalError(f.Name)
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
			return fault.Wrap(fault.Terminal, "acquire_zip_entry", err)
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
		return fault.Wrap(fault.Terminal, "acquire_targz", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fault.Wrap(fault.Terminal, "acquire_gzip", err)
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
			return fault.Wrap(fault.Terminal, "acquire_tar", err)
		}
		dst, ok := SafeJoin(destDir, hdr.Name)
		if !ok {
			return TraversalError(hdr.Name)
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
			// outside destDir, and a release archive needs none of them.
		}
	}
	return nil
}

// writeFile copies at most limit bytes from r into a new file at dst, creating parent
// directories, and refuses once the extraction ceiling is reached. It returns the number
// of bytes written.
func writeFile(dst string, r io.Reader, limit int64) (int64, error) {
	if limit <= 0 {
		return 0, fault.New(fault.Terminal, "acquire_too_big", "acquire: archive exceeds the extraction size limit")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return 0, err
	}
	// 0o600: extracted files are owner-only; the wanted binary is made executable after.
	//nolint:gosec // G304: dst is confined under destDir by SafeJoin before this write
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fault.Wrap(fault.Terminal, "acquire_create", err)
	}
	defer func() { _ = out.Close() }()
	// Copy one byte past the limit so an entry that exactly fills the remaining budget is
	// still detected as overflowing the ceiling rather than silently truncated.
	n, err := io.Copy(out, io.LimitReader(r, limit+1))
	if err != nil {
		return n, fault.Wrap(fault.Terminal, "acquire_copy", err)
	}
	if n > limit {
		return n, fault.New(fault.Terminal, "acquire_too_big", "acquire: archive exceeds the extraction size limit")
	}
	return n, nil
}

// SafeJoin resolves an archive or manifest entry name under base and confirms the result
// stays within base. An entry that is absolute or walks out of the tree with ".." is
// rejected outright rather than re-rooted, so a hostile archive or manifest cannot place a
// file outside the install directory and cannot disguise its intent by relying on path
// collapsing. The containment is then re-checked against the resolved path as defense in
// depth. It is exported so a caller writing files from an untrusted manifest (not only the
// archive extractor) can apply the same guard.
func SafeJoin(base, name string) (string, bool) {
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

// TraversalError is the error SafeJoin's callers return when an entry escapes the install
// directory, so the refusal reads the same whether it came from the archive extractor or a
// manifest writer.
func TraversalError(name string) error {
	return fault.New(fault.Forbidden, "acquire_traversal",
		"acquire: refusing entry that escapes the install directory: "+name)
}

// FindBinary searches the extracted tree under root for a file named binName and returns
// its path. A release archive may place the binary at the root or under a build
// directory depending on the platform, so it is located by name rather than a fixed path.
func FindBinary(root, binName string) (string, bool) {
	var found string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal to the search
		}
		if !d.IsDir() && d.Name() == binName {
			found = p
		}
		return nil
	})
	if found == "" {
		return "", false
	}
	return found, true
}
