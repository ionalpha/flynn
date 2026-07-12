package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

// The archive is the one blob of foreign bytes this package takes apart itself, and an
// extractor is a classic place to find a memory bug or a path escape. The digest is
// verified before extraction in the real flow, so reaching this code with hostile bytes
// means the signing key is already lost; it still must not write outside the file it
// was told to write, and it must not panic.
func FuzzExtractBinary(f *testing.F) {
	// A well-formed archive of each kind, so the fuzzer starts from something that parses
	// rather than spending its budget rediscovering gzip.
	f.Add(packSeed(f, "flynn"), "flynn")
	f.Add(packSeed(f, "flynn.exe"), "flynn.exe")
	f.Add([]byte("not an archive"), "flynn")
	f.Add([]byte{0x1f, 0x8b, 0x08, 0x00}, "flynn")

	f.Fuzz(func(t *testing.T, archive []byte, binaryName string) {
		if binaryName != "flynn" && binaryName != "flynn.exe" {
			// The name is chosen by this process from its own GOOS, never by an input, so
			// only the two real values are worth spending the budget on.
			return
		}
		dir := t.TempDir()
		for _, ext := range []string{".tar.gz", ".zip"} {
			src := filepath.Join(dir, "release"+ext)
			if err := os.WriteFile(src, archive, 0o600); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(dir, "out"+ext)
			// A failure is the expected outcome for almost every input. What is being
			// asserted is that it fails rather than panics, and that a failure never leaves
			// a file behind for the caller to install.
			if err := extractBinary(src, binaryName, dst, 0o755); err != nil {
				if _, statErr := os.Stat(dst); statErr == nil {
					t.Fatalf("a failed extraction left a file at %s", dst)
				}
				continue
			}
			info, err := os.Stat(dst)
			if err != nil {
				t.Fatalf("extraction reported success but wrote nothing: %v", err)
			}
			if info.Size() > maxBinaryBytes {
				t.Fatalf("extraction wrote %d bytes, over the ceiling", info.Size())
			}
			if err := os.Remove(dst); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func packSeed(f *testing.F, binary string) []byte {
	f.Helper()
	// packArchive needs a *testing.T; the seed corpus only needs bytes, so a throwaway
	// archive is built through the same helper the tests use.
	var out []byte
	t := &testing.T{}
	goos := "linux"
	if binary == "flynn.exe" {
		goos = "windows"
	}
	out = packArchive(t, goos, []byte("binary"))
	return out
}
