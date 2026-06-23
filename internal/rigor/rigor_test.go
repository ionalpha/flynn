package rigor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRigorCoverage is the gate: it fails if any production package is missing a
// required property or fuzz test. Running `go test ./...` (via dev/test, dev/check,
// or CI) therefore enforces the rigor floor with no extra wiring, and a new
// package without its tests turns this red.
func TestRigorCoverage(t *testing.T) {
	root, modPath, err := moduleRoot()
	if err != nil {
		t.Skipf("rigor: %v (skipping outside a source checkout)", err)
	}
	vs, err := Check(root, modPath, DefaultPolicy())
	if err != nil {
		t.Fatalf("rigor check: %v", err)
	}
	for _, v := range vs {
		t.Errorf("%s: %s", v.Pkg, v.Reason)
	}
	if len(vs) > 0 {
		t.Logf("%d rigor violation(s); add the missing tests, or (only to shrink it) adjust the grandfather allowlist in rigor.go", len(vs))
	}
}

// moduleRoot finds the module root by walking up from the working directory to the
// go.mod, and returns the directory plus the declared module path.
func moduleRoot() (root, modPath string, err error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			return dir, parseModulePath(b), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", os.ErrNotExist
		}
		dir = parent
	}
}

func parseModulePath(gomod []byte) string {
	for _, line := range strings.Split(string(gomod), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}
