package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/closeerr"
)

// Extraction ceilings. The archive is digest-verified before it gets here, so these
// guard against a malformed release from the pipeline itself and against reuse of
// this code on an unpinned archive. A verifier that holds only when the caller did its
// job is no verifier.
const (
	maxArchiveEntries = 64
	maxBinaryBytes    = 512 << 20
)

// extractBinary writes exactly one member of a release archive, the flynn binary, to
// dst. It never creates a directory, a symlink or any file other than dst, so the
// archive cannot name a path outside the destination (zip slip), point a link at the
// host, or drop a second file beside the binary. The name it looks for comes from this
// process's GOOS, never from the archive.
func extractBinary(archivePath, binaryName, dst string, mode os.FileMode) error {
	switch {
	case strings.HasSuffix(archivePath, ".tar.gz"):
		return extractFromTarGz(archivePath, binaryName, dst, mode)
	case strings.HasSuffix(archivePath, ".zip"):
		return extractFromZip(archivePath, binaryName, dst, mode)
	default:
		return fault.New(fault.Terminal, CodeArchive, "unsupported release archive format: "+archivePath)
	}
}

func extractFromTarGz(archivePath, binaryName, dst string, mode os.FileMode) error {
	// #nosec G304 -- archivePath is built by this package from the destination directory
	// it created and the asset name it derived from its own GOOS; it is never user input.
	f, err := os.Open(archivePath)
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeArchive, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeArchive, fmt.Errorf("reading the release archive: %w", err))
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for range maxArchiveEntries {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fault.Wrap(fault.Terminal, CodeArchive, fmt.Errorf("reading the release archive: %w", err))
		}
		if !matchesEntry(h.Name, binaryName) {
			continue
		}
		// A link, a device node or a directory with the binary's name is no binary, and
		// following one is how an extractor is tricked into writing elsewhere.
		if h.Typeflag != tar.TypeReg {
			return fault.New(fault.Terminal, CodeArchive,
				"the release archive's "+binaryName+" entry is not a regular file")
		}
		if h.Size > maxBinaryBytes {
			return fault.New(fault.Terminal, CodeArchive,
				fmt.Sprintf("the release binary is %d bytes, over the %d-byte ceiling", h.Size, int64(maxBinaryBytes)))
		}
		return writeBinary(dst, tr, mode)
	}
	return fault.New(fault.Terminal, CodeArchive, "the release archive contains no "+binaryName)
}

func extractFromZip(archivePath, binaryName, dst string, mode os.FileMode) error {
	// #nosec G304 -- see extractFromTarGz: the path is this package's own.
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeArchive, fmt.Errorf("reading the release archive: %w", err))
	}
	defer func() { _ = zr.Close() }()

	if len(zr.File) > maxArchiveEntries {
		return fault.New(fault.Terminal, CodeArchive,
			fmt.Sprintf("the release archive holds %d entries, over the %d ceiling", len(zr.File), maxArchiveEntries))
	}
	for _, e := range zr.File {
		if !matchesEntry(e.Name, binaryName) {
			continue
		}
		if !e.Mode().IsRegular() {
			return fault.New(fault.Terminal, CodeArchive,
				"the release archive's "+binaryName+" entry is not a regular file")
		}
		// The uncompressed size is the archive's own claim, so it is a cheap first
		// refusal here and enforced again on the stream, where the bytes arrive.
		if e.UncompressedSize64 > maxBinaryBytes {
			return fault.New(fault.Terminal, CodeArchive,
				fmt.Sprintf("the release binary claims %d bytes, over the %d-byte ceiling", e.UncompressedSize64, int64(maxBinaryBytes)))
		}
		rc, err := e.Open()
		if err != nil {
			return fault.Wrap(fault.Terminal, CodeArchive, err)
		}
		defer func() { _ = rc.Close() }()
		return writeBinary(dst, rc, mode)
	}
	return fault.New(fault.Terminal, CodeArchive, "the release archive contains no "+binaryName)
}

// matchesEntry reports whether an archive entry is the binary sought, by base name. A
// name that climbs out of the archive or hides a separator never matches: an entry
// called "../../flynn" is an attempt to be found somewhere else.
func matchesEntry(entryName, binaryName string) bool {
	if strings.ContainsAny(entryName, `\`) || strings.Contains(entryName, "..") {
		return false
	}
	clean := path.Clean(entryName)
	if path.IsAbs(clean) {
		return false
	}
	return clean == binaryName
}

// writeBinary streams the entry to a new file, refusing to overwrite one, and caps the
// stream whatever size the archive header claimed.
func writeBinary(dst string, src io.Reader, mode os.FileMode) (err error) {
	// #nosec G304 -- dst is the staging path this package created next to the binary it
	// is replacing; O_EXCL means it refuses to write to a file that already exists.
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeArchive, err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(dst)
			err = fault.Wrap(fault.Terminal, CodeArchive, err)
		}
	}()
	defer closeerr.Keep(&err, f)
	// Read one byte past the ceiling, so a body that lied about its length is detected
	// instead of truncated into a binary that is almost right.
	n, err := io.Copy(f, io.LimitReader(src, maxBinaryBytes+1))
	if err == nil && n > maxBinaryBytes {
		err = fmt.Errorf("the release binary exceeds the %d-byte ceiling", int64(maxBinaryBytes))
	}
	if err == nil {
		// The bytes must be on disk before the file goes into service: a rename is
		// atomic for the directory entry, and says nothing about the file's content.
		err = f.Sync()
	}
	return err
}
