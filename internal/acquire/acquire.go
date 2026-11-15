// Package acquire fetches a pinned binary archive, verifies its digest, extracts it
// with a path-traversal guard and a total-size ceiling, installs it atomically and
// locates the wanted executable. A local inference runtime and external CLI
// dependencies share it.
//
// It guarantees only that the installed bytes are the pinned, digest-verified
// release and that a hostile archive can neither write outside the target nor fill
// the disk. The caller applies its own version gate before and runs the binary in
// the sandbox after.
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
	"github.com/ionalpha/flynn/internal/closeerr"
	"github.com/ionalpha/flynn/internal/fetch"
)

// ArchiveKind is a release's container format. Both kinds use the standard library,
// so acquire adds no decompression dependency.
type ArchiveKind int

const (
	// ArchiveZip is a .zip archive, the usual Windows release form.
	ArchiveZip ArchiveKind = iota
	// ArchiveTarGz is a gzip-compressed tar, the usual Linux and macOS form.
	ArchiveTarGz
)

// maxExtractBytes caps total uncompressed output, so a decompression bomb cannot
// fill the disk though the download was capped. Release archives are tens of MB.
const maxExtractBytes = 4 << 30 // 4 GiB

// Release is one pinned artifact for one platform: source, digest, container
// format and wanted executable. It is fixed at build time, so the installable set
// is auditable and cannot be redirected at runtime.
type Release struct {
	// URL is the https source of the release archive.
	URL string
	// SHA256 is the pinned digest the downloaded archive must match.
	SHA256 string
	// SizeBytes is the archive's known size, used as the download cap.
	SizeBytes int64
	// Archive is the archive's container format.
	Archive ArchiveKind
	// BinName is the executable to locate in the extracted archive ("flynn",
	// "flyctl.exe"). Its sibling files are extracted beside it.
	BinName string
	// Companions are archives unpacked into the same directory, for an upstream
	// that ships libraries separately (CUDA llama.cpp ships its runtime DLLs
	// apart). Each has its own digest and size cap, and the install is atomic over
	// all of them: a companion failing verification leaves nothing at the target.
	Companions []Companion
}

// Companion is an additional pinned archive installed beside a [Release]'s main one.
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

// InstallTo ensures rel's binary is at targetDir and returns its absolute path. If
// it is already there it is reused without a download and reused is true, so
// targetDir should be version-specific to install new versions beside old ones.
//
// The archive downloads to a temp file beside targetDir, is digest-verified, is
// extracted into a sibling staging directory and is moved into place only on
// success, so an interrupted or corrupt install never leaves a half-populated
// targetDir.
func InstallTo(ctx context.Context, dl *fetch.Downloader, rel Release, targetDir string) (binPath string, reused bool, err error) {
	if bin, ok := FindBinary(targetDir, rel.BinName); ok {
		return bin, true, nil
	}
	parent := filepath.Dir(targetDir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return "", false, fault.Wrap(fault.Terminal, "acquire_dest", err)
	}

	staging, err := os.MkdirTemp(parent, ".staging-*")
	if err != nil {
		return "", false, fault.Wrap(fault.Terminal, "acquire_stage", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := fetchAndExtract(ctx, dl, parent, staging, rel.URL, rel.SHA256, rel.SizeBytes, rel.Archive); err != nil {
		return "", false, err
	}
	for _, c := range rel.Companions {
		if err := fetchAndExtract(ctx, dl, parent, staging, c.URL, c.SHA256, c.SizeBytes, c.Archive); err != nil {
			return "", false, err
		}
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

// fetchAndExtract downloads one archive to a temp file under parent, verifies it
// against sha256 and unpacks it into staging. The temp file is removed on every
// path, so a refused archive leaves no bytes behind.
func fetchAndExtract(ctx context.Context, dl *fetch.Downloader, parent, staging, url, sha256 string, size int64, kind ArchiveKind) error {
	tmpArchive, err := os.CreateTemp(parent, ".acquire-*.archive")
	if err != nil {
		return fault.Wrap(fault.Terminal, "acquire_tmp", err)
	}
	archivePath := tmpArchive.Name()
	_ = tmpArchive.Close()
	defer func() { _ = os.Remove(archivePath) }()

	if _, err := dl.Fetch(ctx, fetch.Request{
		URL:          url,
		Dest:         archivePath,
		ExpectSHA256: sha256,
		MaxBytes:     size + (1 << 20), // the pinned size plus a small margin
	}); err != nil {
		return err
	}
	return extract(kind, archivePath, staging)
}

// extract unpacks src into destDir, guarding against path traversal and oversized
// output. Only regular files and directories are written, so a symlink or device
// entry cannot redirect a later write outside destDir.
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
			// A link entry could redirect a write outside destDir, and a release
			// archive needs none.
		}
	}
	return nil
}

// writeFile copies at most limit bytes from r into a new file at dst, creating
// parent directories, and returns the bytes written. It refuses past the
// extraction ceiling.
func writeFile(dst string, r io.Reader, limit int64) (n int64, err error) {
	if limit <= 0 {
		return 0, fault.New(fault.Terminal, "acquire_too_big", "acquire: archive exceeds the extraction size limit")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return 0, err
	}
	// 0o600: extracted files are owner-only; the wanted binary is made executable later.
	out, err := openOut(dst)
	if err != nil {
		return 0, fault.Wrap(fault.Terminal, "acquire_create", err)
	}
	// A write that fails only at close leaves a truncated file the install would
	// put into service, so the close error fails the extraction.
	defer closeerr.Keep(&err, out)
	// Copy one byte past the limit, so an entry that exactly fills the budget is
	// detected as overflow rather than truncated.
	n, err = io.Copy(out, io.LimitReader(r, limit+1))
	if err != nil {
		return n, fault.Wrap(fault.Terminal, "acquire_copy", err)
	}
	if n > limit {
		return n, fault.New(fault.Terminal, "acquire_too_big", "acquire: archive exceeds the extraction size limit")
	}
	// The bytes must be on disk before the file goes into service.
	if err := out.Sync(); err != nil {
		return n, fault.Wrap(fault.Terminal, "acquire_sync", err)
	}
	return n, nil
}

// outFile is the part of *os.File writeFile uses, an interface so a test can make
// close fail.
type outFile interface {
	io.Writer
	Sync() error
	Close() error
}

// openOut creates the extraction target. It is a variable so a test can substitute
// a file whose close fails.
var openOut = func(dst string) (outFile, error) {
	//nolint:gosec // G304: dst is confined under destDir by SafeJoin before this write
	return os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
}

// SafeJoin resolves an archive or manifest entry name under base and confirms it
// stays within base. An absolute name or one that climbs out with ".." is refused
// rather than re-rooted, so a hostile entry cannot place a file outside the install
// directory. [filepath.IsLocal] also refuses reserved Windows device names such as
// NUL or COM1, which would open the device. Containment is re-checked on the
// resolved path. Callers writing files from an untrusted manifest use it too.
func SafeJoin(base, name string) (string, bool) {
	slashed := strings.ReplaceAll(name, `\`, "/")
	local := filepath.FromSlash(path.Clean(slashed))
	if local == "." || !filepath.IsLocal(local) || strings.Contains(name, ":") {
		return "", false
	}
	dst := filepath.Join(base, local)
	rel, err := filepath.Rel(base, dst)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return dst, true
}

// TraversalError is the error SafeJoin's callers return for an entry that escapes
// the install directory, so the refusal reads the same from every caller.
func TraversalError(name string) error {
	return fault.New(fault.Forbidden, "acquire_traversal",
		"acquire: refusing entry that escapes the install directory: "+name)
}

// FindBinary returns the path of the file named binName under root. Archives place
// the binary at the root or in a build directory depending on platform, so it is
// found by name.
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
