package fsatomic

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestPropLastWriteWins drives a random sequence of writes (arbitrary contents,
// including empty) at one path and checks the atomicity contract from the
// outside: after every write the file contains exactly the bytes just written,
// and no temp file is ever left behind.
func TestPropLastWriteWins(tt *testing.T) {
	rapid.Check(tt, func(t *rapid.T) {
		dir, err := os.MkdirTemp(tt.TempDir(), "prop-*")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "f.json")
		writes := rapid.IntRange(1, 8).Draw(t, "writes")
		for range writes {
			data := rapid.SliceOfN(rapid.Byte(), 0, 1<<10).Draw(t, "data")
			if err := WriteFile(path, data, 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("content = %d bytes, want the %d just written", len(got), len(data))
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp-") {
				t.Fatalf("temp file left behind: %s", e.Name())
			}
		}
		if len(entries) != 1 {
			t.Fatalf("dir has %d entries, want only the target", len(entries))
		}
	})
}
