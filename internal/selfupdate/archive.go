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
)

// Extraction ceilings. The archive is already digest-verified before it gets here, so
// these are not defending against a hostile mirror: they defend against the case where
// the release pipeline itself produces something malformed, and against the day this
// code is reused on an archive that was not pinned. A verifier that only holds when
// the caller did its job is not a verifier.
const (
	maxArchiveEntries = 64
	maxBinaryBytes    = 512 << 20
)

// extractBinary pulls exactly one member, the flynn binary, out of a release archive
// and writes it to dst. Nothing else in the archive is written anywhere.
//
// It is deliberately not a general-purpose extractor. It never creates a directory, a
// symlink, or any file other than dst, so the archive cannot name a path outside the
// destination (zip slip), cannot point a link at something on the host, and cannot
// drop a second file next to the binary. The name it looks for is chosen by this
// process from its own GOOS, never read from the archive.
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
		// A link, a device node, or a directory wearing the binary's name is not the
		// binary, and following one is how an extractor gets talked into writing
		// somewhere it was never asked to write.
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
		// The uncompressed size is the archive's own claim, so it is checked here as a
		// cheap first refusal and enforced again on the stream below, where the bytes
		// actually arrive and cannot lie.
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

// matchesEntry reports whether an archive entry is the binary being looked for. The
// comparison is on the entry's base name, but a name that climbs out of the archive
// or hides a separator never matches at all: an entry called "../../flynn" is not a
// flynn to be found, it is an attempt to be found somewhere else.
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

// writeBinary streams the entry to a fresh file, refusing to overwrite one that is
// already there, and caps the stream regardless of what the archive's header claimed
// the size would be.
func writeBinary(dst string, src io.Reader, mode os.FileMode) error {
	// #nosec G304 -- dst is the staging path this package created next to the binary it
	// is replacing; O_EXCL means it refuses to write to a file that already exists.
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeArchive, err)
	}
	// Read one byte past the ceiling, so a body that lied about its length is detected
	// rather than silently truncated into a binary that is almost right.
	n, err := io.Copy(f, io.LimitReader(src, maxBinaryBytes+1))
	if err == nil && n > maxBinaryBytes {
		err = fmt.Errorf("the release binary exceeds the %d-byte ceiling", int64(maxBinaryBytes))
	}
	if err == nil {
		// The bytes have to be on the disk before the file is put into service: a rename
		// is atomic with respect to the directory, not with respect to the file's content.
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		return fault.Wrap(fault.Terminal, CodeArchive, err)
	}
	return nil
}
