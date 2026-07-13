package selfupdate

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Windows cannot overwrite a running executable, so the swap is two renames: the
// outgoing binary is moved aside, and the incoming one takes its name. If there is
// nothing at the install path to move aside, there is no upgrade to perform, and the
// staged binary is cleaned up rather than left in the install directory looking like a
// half-finished one.
func TestReplaceRefusesWhenThereIsNothingToMoveAside(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, stagedPrefix+"new")
	if err := os.WriteFile(staged, []byte("the new binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	tgt := installTarget{Path: filepath.Join(dir, "no-flynn-here"), Dir: dir, Mode: 0o755}

	err := tgt.replace(staged)
	if err == nil {
		t.Fatal("a binary was installed over a path that holds nothing")
	}
	if codeOf(t, err) != CodeInstall {
		t.Fatalf("code = %q, want %q", codeOf(t, err), CodeInstall)
	}
	if !strings.Contains(err.Error(), "moving the running binary aside") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(staged); statErr == nil {
		t.Fatal("a failed replacement left the staged binary behind")
	}
}

// An earlier upgrade's leftover sits at the name the outgoing binary is about to be
// moved to. When it cannot be cleared, the upgrade must not be blocked by it: a unique
// name is chosen instead, and the swap goes through.
func TestReplaceWorksAroundALeftoverItCannotClear(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flynn.exe")
	if err := os.WriteFile(path, []byte("the old binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, stagedPrefix+"new")
	if err := os.WriteFile(staged, []byte("the new binary"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A non-empty directory at the superseded name cannot be removed, which is the same
	// refusal a still-running outgoing binary produces, and is producible on any machine.
	blocked := path + supersededSuffix
	if err := os.MkdirAll(filepath.Join(blocked, "in the way"), 0o750); err != nil {
		t.Fatal(err)
	}

	tgt := installTarget{Path: path, Dir: dir, Mode: 0o755}
	if err := tgt.replace(staged); err != nil {
		t.Fatalf("a leftover from an earlier upgrade blocked this one: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the new binary" {
		t.Fatalf("the installed binary holds %q", got)
	}
	// The outgoing binary went somewhere that is not the blocked name, and it is still
	// readable: the sweep at the next start is what takes it away.
	aside := filepath.Join(dir, "flynn.exe."+strconv.Itoa(os.Getpid())+supersededSuffix)
	if body, err := os.ReadFile(aside); err != nil || string(body) != "the old binary" {
		t.Fatalf("the outgoing binary is not at %s: %v", aside, err)
	}
}
