package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzExtract feeds arbitrary bytes to the extractor as both archive kinds. The
// invariants under any input, however malformed: extraction never panics, and it never
// writes a file outside the destination directory. A corrupt archive may error, that is
// fine; an escape or a crash is not.
func FuzzExtract(f *testing.F) {
	f.Add([]byte("not an archive"))
	f.Add([]byte{0x1f, 0x8b, 0x08, 0x00}) // a gzip magic with no body
	f.Add([]byte("PK\x03\x04"))           // a zip local-file magic with no body
	f.Fuzz(func(t *testing.T, data []byte) {
		parent := t.TempDir()
		dest := filepath.Join(parent, "install")
		if err := os.MkdirAll(dest, 0o750); err != nil {
			t.Fatal(err)
		}
		src := filepath.Join(parent, "in.bin")
		if err := os.WriteFile(src, data, 0o644); err != nil {
			t.Fatal(err)
		}
		// Neither call may panic. Errors are acceptable for malformed input.
		_ = extract(ArchiveZip, src, dest)
		_ = extract(ArchiveTarGz, src, dest)

		// Whatever was written must stay under dest; nothing may appear in parent
		// outside the install directory and the input file.
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != "install" && e.Name() != "in.bin" {
				t.Fatalf("extraction wrote outside the destination: %s", e.Name())
			}
		}
	})
}

// FuzzSafeJoin asserts the containment boundary holds for any base and entry name: an
// accepted result is always within base, and the function never panics.
func FuzzSafeJoin(f *testing.F) {
	f.Add("/base", "bin/llama-server")
	f.Add("/base", "../escape")
	f.Add("/base", `..\..\escape`)
	f.Add("/base", "/abs")
	f.Fuzz(func(t *testing.T, base, name string) {
		if base == "" {
			base = "/base"
		}
		base = filepath.Clean(base)
		dst, ok := safeJoin(base, name)
		if !ok {
			return
		}
		rel, err := filepath.Rel(base, dst)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			t.Fatalf("safeJoin(%q,%q)=%q escaped base (rel=%q,err=%v)", base, name, dst, rel, err)
		}
	})
}
