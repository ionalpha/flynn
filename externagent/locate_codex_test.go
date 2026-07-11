package externagent

import (
	"runtime"
	"strings"
	"testing"
)

// TestCodexPlatformPkg checks the per-platform dependency name codex vendors its native
// binary in, so the launcher resolves to the binary that newer releases ship under the
// package's node_modules rather than beside the launcher. The npm platform suffix names
// Windows "win32" and amd64 "x64", which differ from Go's GOOS/GOARCH.
func TestCodexPlatformPkg(t *testing.T) {
	got := codexPlatformPkg()
	if !strings.HasPrefix(got, "codex-") {
		t.Fatalf("platform package %q should be named codex-<os>-<arch>", got)
	}
	if runtime.GOOS == "windows" && !strings.Contains(got, "-win32-") {
		t.Errorf("windows must map to win32, got %q", got)
	}
	if strings.Contains(got, "windows") {
		t.Errorf("the go OS name windows must be translated to win32, got %q", got)
	}
	if runtime.GOARCH == "amd64" && !strings.HasSuffix(got, "-x64") {
		t.Errorf("amd64 must map to x64, got %q", got)
	}
	if strings.HasSuffix(got, "-amd64") {
		t.Errorf("the go arch amd64 must be translated to x64, got %q", got)
	}
}
