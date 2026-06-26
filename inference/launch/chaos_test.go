package launch

import (
	"os"
	"path/filepath"
	"testing"
)

// TestInspectTemplateRejectsBadWeights is the robustness invariant for the file read:
// garbage, truncated, or absent weights make InspectTemplate return an error, never
// panic and never a false template decision.
func TestInspectTemplateRejectsBadWeights(t *testing.T) {
	dir := t.TempDir()

	cases := map[string][]byte{
		"not gguf":        []byte("this is not a model file"),
		"truncated magic": {0x47, 0x47},
		"empty":           {},
		// A valid magic but a header that claims more metadata than the bytes provide.
		"lying header": {0x47, 0x47, 0x55, 0x46, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, "bad.gguf")
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := InspectTemplate(p, "chatml"); err == nil {
				t.Fatalf("%s should be rejected", name)
			}
		})
	}

	// An absent file is an error, not a panic.
	if _, err := InspectTemplate(filepath.Join(dir, "missing.gguf"), "chatml"); err == nil {
		t.Fatal("a missing weights file should error")
	}
}
