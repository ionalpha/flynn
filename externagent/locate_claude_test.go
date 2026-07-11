package externagent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestLocateClaude holds on a host with Claude Code installed and on one without (CI), so
// it asserts the invariants of each outcome rather than a fixed path. When claude is
// installed it must resolve to a real, native executable the confined child can launch,
// with its own directory granted readable; when it is absent it must report
// ErrProgramNotFound so detection can onboard the install rather than fail the command.
func TestLocateClaude(t *testing.T) {
	prog, err := LocateClaude("")
	if err != nil {
		if !errors.Is(err, ErrProgramNotFound) {
			t.Fatalf("LocateClaude: unexpected error kind: %v", err)
		}
		return
	}
	if !filepath.IsAbs(prog.Path) {
		t.Errorf("resolved path must be absolute: %q", prog.Path)
	}
	if st, statErr := os.Stat(prog.Path); statErr != nil || st.IsDir() {
		t.Errorf("resolved path must be an existing file: %q (%v)", prog.Path, statErr)
	}
	if !isNativeExecutable(prog.Path) {
		t.Errorf("resolved path must be a native executable, not a launcher script: %q", prog.Path)
	}
	if len(prog.ReadableDirs) == 0 {
		t.Errorf("a resolved program must name at least one readable directory")
	}
	for _, dir := range prog.ReadableDirs {
		if st, statErr := os.Stat(dir); statErr != nil || !st.IsDir() {
			t.Errorf("readable dir must exist: %q (%v)", dir, statErr)
		}
	}
}

// TestLocateClaudeMissing confirms an absolute path to a non-existent binary is reported
// as not-installed, not as an unexpected error, so onboarding stays actionable.
func TestLocateClaudeMissing(t *testing.T) {
	_, err := LocateClaude(filepath.Join(t.TempDir(), "no-such-claude"))
	if !errors.Is(err, ErrProgramNotFound) {
		t.Errorf("a missing binary must report ErrProgramNotFound, got %v", err)
	}
}
