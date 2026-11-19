package skillab_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/skill/skillab"
)

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stagedSet is a set with one fixture and a held-out overlay for it.
func stagedSet(t *testing.T) skillab.Set {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, skillab.ExercisesFile), "[f] fix it | exit 0\n")
	write(t, filepath.Join(dir, skillab.FixturesDir, "f", "a.txt"), "original")
	write(t, filepath.Join(dir, skillab.ChecksDir, "f", "tests", "held.sh"), "held out")
	set, err := skillab.LoadDir(dir, "s")
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestStagePlacesHarnessFilesAndTheOverlay(t *testing.T) {
	set := stagedSet(t)
	work := t.TempDir()
	if err := set.Materialise(set.Exercises[0], work); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(work, "a.txt"), "changed by the agent")

	if err := set.Stage(set.Exercises[0], work, "all done", "turn 1"); err != nil {
		t.Fatal(err)
	}
	h := filepath.Join(work, skillab.HarnessDir)
	if got := read(t, filepath.Join(h, skillab.ReportFile)); got != "all done" {
		t.Errorf("report = %q", got)
	}
	if got := read(t, filepath.Join(h, skillab.TranscriptFile)); got != "turn 1" {
		t.Errorf("transcript = %q", got)
	}
	if got := read(t, filepath.Join(h, skillab.PristineDir, "a.txt")); got != "original" {
		t.Errorf("pristine = %q, want the fixture as it was given", got)
	}
	if got := read(t, filepath.Join(work, "tests", "held.sh")); got != "held out" {
		t.Errorf("overlay = %q", got)
	}
	if got := read(t, filepath.Join(work, "a.txt")); got != "changed by the agent" {
		t.Errorf("the overlay touched a file it does not carry: %q", got)
	}
}

// An agent can leave a symlink where the harness writes. Following it would put the
// harness's files, or the held-out tests, wherever the agent pointed.
func TestStageDoesNotFollowASymlinkTheAgentPlanted(t *testing.T) {
	set := stagedSet(t)
	work, outside := t.TempDir(), t.TempDir()
	write(t, filepath.Join(outside, "victim.txt"), "untouched")

	if err := os.Symlink(outside, filepath.Join(work, skillab.HarnessDir)); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if err := set.Stage(set.Exercises[0], work, "report", "transcript"); err != nil {
		t.Fatalf("a planted %s link was not replaced: %v", skillab.HarnessDir, err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Errorf("the harness wrote through the link into %v", entries)
	}

	if err := os.Remove(filepath.Join(work, "tests", "held.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "victim.txt"), filepath.Join(work, "tests", "held.sh")); err != nil {
		t.Fatal(err)
	}
	if err := set.Stage(set.Exercises[0], work, "report", "transcript"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(outside, "victim.txt")); got != "untouched" {
		t.Errorf("the overlay wrote through a planted link: %q", got)
	}
	if got := read(t, filepath.Join(work, "tests", "held.sh")); got != "held out" {
		t.Errorf("the overlay file was not put in place of the link: %q", got)
	}
}

// A directory the agent turned into a link that leaves the tree is refused, and the
// refusal is an error the caller sees.
func TestStageRefusesADirectoryLinkedOutsideTheTree(t *testing.T) {
	set := stagedSet(t)
	work, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(work, "tests")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	err := set.Stage(set.Exercises[0], work, "r", "t")
	if err == nil {
		t.Fatal("staged through a directory linked outside the tree")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("wrote into %v: %v", outside, err)
	}
}

func TestStageWithoutAFixtureStillLeavesTheReport(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, skillab.ExercisesFile), "bare | exit 0\n")
	set, err := skillab.LoadDir(dir, "s")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := set.Stage(set.Exercises[0], work, "said so", ""); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(work, skillab.HarnessDir, skillab.ReportFile)); !strings.Contains(got, "said so") {
		t.Errorf("report = %q", got)
	}
}
