package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/testkit"
)

// archiveAt writes bytes to a file named with the given extension, which is what tells
// the extractor which format to expect, and returns its path.
func archiveAt(t *testing.T, ext string, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "flynn_release"+ext)
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// tarGz builds a gzipped tar from headers and bodies exactly as given, so a test can
// produce an entry a well-behaved writer would refuse to produce.
func tarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := e.header
		if h.Size == 0 && !e.declaredSizeOnly {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if !e.declaredSizeOnly && len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A declared-size-only entry has a header promising bytes that are never written, so
	// closing the writer reports the shortfall. That header is the point of the fixture,
	// and it is already in the stream.
	_ = tw.Close()
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type tarEntry struct {
	header tar.Header
	body   []byte
	// declaredSizeOnly writes the header and no body, so the header's Size is a claim the
	// stream does not back up.
	declaredSizeOnly bool
}

// An archive whose name is neither of the two formats the release pipeline produces is
// not opened at all. The format is decided by this package, never by the archive's
// content, so an attacker cannot get a different parser than the one that was chosen.
func TestExtractRefusesAnUnknownArchiveFormat(t *testing.T) {
	err := extractBinary(archiveAt(t, ".rar", []byte("whatever")), "flynn", filepath.Join(t.TempDir(), "out"), 0o755)
	if err == nil {
		t.Fatal("an archive in an unsupported format was opened")
	}
	if !strings.Contains(err.Error(), "unsupported release archive format") {
		t.Fatalf("err = %v", err)
	}
}

// Every way a tar.gz can be wrong, including the ways only a hostile or broken pipeline
// produces: none of them may yield a file for the caller to install.
func TestTarGzExtractionRefusals(t *testing.T) {
	good := packArchive(t, "linux", []byte("the new binary"))

	tests := []struct {
		name    string
		archive []byte
		want    string
	}{
		{
			name:    "bytes that are not gzip at all",
			archive: []byte("this is not an archive"),
			want:    "reading the release archive",
		},
		{
			name: "a gzip stream that stops in the middle",
			// The gzip header parses, so the failure lands in the tar reader, which is where
			// a truncated download or a mirror that hung up would land it.
			archive: good[:len(good)/2],
			want:    "reading the release archive",
		},
		{
			name:    "an archive holding everything except the binary",
			archive: tarGz(t, tarEntry{header: tar.Header{Name: "LICENSE", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte("Apache-2.0")}),
			want:    "contains no flynn",
		},
		{
			name: "a symlink wearing the binary's name",
			archive: tarGz(t, tarEntry{header: tar.Header{
				Name: "flynn", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd",
			}}),
			want: "is not a regular file",
		},
		{
			name: "a directory wearing the binary's name",
			archive: tarGz(t, tarEntry{header: tar.Header{
				Name: "flynn", Mode: 0o755, Typeflag: tar.TypeDir,
			}}),
			want: "is not a regular file",
		},
		{
			name: "an entry claiming more bytes than the ceiling allows",
			archive: tarGz(t, tarEntry{
				header:           tar.Header{Name: "flynn", Mode: 0o755, Typeflag: tar.TypeReg, Size: maxBinaryBytes + 1},
				declaredSizeOnly: true,
			}),
			want: "over the",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "staged")
			err := extractBinary(archiveAt(t, ".tar.gz", tc.archive), "flynn", dst, 0o755)
			if err == nil {
				t.Fatal("the archive was extracted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if _, statErr := os.Stat(dst); statErr == nil {
				t.Fatal("a refused extraction still wrote the destination file")
			}
		})
	}
}

// A tar.gz the extractor cannot even open, because there is no file there, is refused
// rather than treated as an empty archive.
func TestExtractRefusesAnArchiveThatIsNotOnDisk(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nothing-here.tar.gz")
	if err := extractBinary(missing, "flynn", filepath.Join(t.TempDir(), "out"), 0o755); err == nil {
		t.Fatal("a missing archive extracted successfully")
	}
}

// The same refusals for the zip the Windows release ships, which goes through an
// entirely separate reader and so has to be proved separately.
func TestZipExtractionRefusals(t *testing.T) {
	manyEntries := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for i := range maxArchiveEntries + 1 {
			w, err := zw.Create("file" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	dirEntry := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		// A directory entry whose name cleans to the binary's: it is matched by name and
		// must then be refused on its mode, because a directory is not a binary.
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: "flynn.exe/"}); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	noBinary := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, err := zw.Create("README.md")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("# flynn")); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	tests := []struct {
		name    string
		archive []byte
		want    string
	}{
		{"bytes that are not a zip", []byte("PK is all it takes, apparently not"), "reading the release archive"},
		{"more entries than the ceiling allows", manyEntries(), "over the"},
		{"a directory wearing the binary's name", dirEntry(), "is not a regular file"},
		{"an archive holding everything except the binary", noBinary(), "contains no flynn.exe"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "staged")
			err := extractBinary(archiveAt(t, ".zip", tc.archive), "flynn.exe", dst, 0o755)
			if err == nil {
				t.Fatal("the archive was extracted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if _, statErr := os.Stat(dst); statErr == nil {
				t.Fatal("a refused extraction still wrote the destination file")
			}
		})
	}
}

// The writer refuses to overwrite a file that is already at the destination. The
// staging path is created with a fresh name, so a file already sitting there is
// something this process did not put there, and writing through it is how an extractor
// gets talked into clobbering a file it was never pointed at.
func TestWriteBinaryRefusesAnExistingDestination(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "staged")
	if err := os.WriteFile(dst, []byte("someone got here first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeBinary(dst, bytes.NewReader([]byte("the new binary")), 0o755); err == nil {
		t.Fatal("the extractor overwrote a file that was already there")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "someone got here first" {
		t.Fatal("the existing file was overwritten anyway")
	}
}

// A read that fails halfway through leaves a partial binary on the disk, and a partial
// binary that is left where the installer will find it is the one outcome this path
// must never produce.
func TestWriteBinaryRemovesTheFileWhenTheStreamFails(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "staged")
	boom := errors.New("the connection went away")
	src := testkit.FaultyReader(bytes.NewReader(bytes.Repeat([]byte("x"), 4096)), testkit.FailOnCall(2, boom))

	err := writeBinary(dst, src, 0o755)
	if err == nil {
		t.Fatal("a stream that failed mid-read was accepted")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the read failure to survive wrapping", err)
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Fatal("a failed write left a partial binary at the destination")
	}
}
