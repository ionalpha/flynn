package skillab_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ionalpha/flynn/skill/skillab"
)

// A fixture is copied whole into the trial's working directory, subdirectories
// included. An exercise that starts from an existing repository is measuring nothing
// if half the repository arrives.
func TestCopyFixtureWritesTheWholeTree(t *testing.T) {
	fsys := fstest.MapFS{
		"set/fixtures/broken-parser/main.go":          {Data: []byte("package main\n")},
		"set/fixtures/broken-parser/internal/p/p.go":  {Data: []byte("package p\n")},
		"set/fixtures/broken-parser/scripts/repro.sh": {Data: []byte("#!/bin/sh\nexit 1\n"), Mode: 0o755},
		"set/fixtures/other/unrelated.txt":            {Data: []byte("not this one\n")},
		"set/" + skillab.ExercisesFile:                {Data: []byte("[broken-parser] fix it | exit 0\n")},
	}
	dest := t.TempDir()
	if err := skillab.CopyFixture(fsys, "set", "broken-parser", dest); err != nil {
		t.Fatalf("copy: %v", err)
	}

	for _, rel := range []string{"main.go", filepath.Join("internal", "p", "p.go"), filepath.Join("scripts", "repro.sh")} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("%s did not arrive: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "unrelated.txt")); err == nil {
		t.Error("a file from a different fixture arrived; a fixture is one directory")
	}
	if runtime.GOOS != "windows" {
		// A fixture carrying a reproduction script is useless if the run cannot run it,
		// and Windows has no executable bit to preserve.
		info, err := os.Stat(filepath.Join(dest, "scripts", "repro.sh"))
		if err != nil {
			t.Fatalf("stat the script: %v", err)
		}
		if info.Mode()&0o100 == 0 {
			t.Errorf("repro.sh arrived as %v, want the executable bit kept", info.Mode())
		}
	}
}

// An empty fixture is always a mistake. The author meant to seed a state and seeded
// nothing, and the run cannot tell the difference between that and an exercise that
// was supposed to start from an empty directory.
func TestCopyFixtureRefusesTheCasesThatWouldSeedNothing(t *testing.T) {
	fsys := fstest.MapFS{
		"set/fixtures/empty/sub/.keep": {Data: []byte("")},
		"set/fixtures/real/main.go":    {Data: []byte("package main\n")},
	}
	for name, tc := range map[string]struct {
		fixture string
		want    string
	}{
		"a fixture that is not there":    {"missing", "missing"},
		"a name reaching out of the set": {"../../..", "path separator"},
		"the parent directory itself":    {"..", "not a fixture name"},
		"a name holding a separator":     {"nested/deeper", "path separator"},
		"an empty name":                  {"", "empty fixture name"},
	} {
		t.Run(name, func(t *testing.T) {
			err := skillab.CopyFixture(fsys, "set", tc.fixture, t.TempDir())
			if err == nil {
				t.Fatalf("fixture %q was accepted", tc.fixture)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A fixture holding a symbolic link is refused rather than followed. fs.FS closes
// lexical traversal, and os.DirFS follows a link once it has opened one, so a link
// to somewhere outside the set would otherwise be copied into the working directory
// of every trial in the run.
func TestCopyFixtureRefusesAnythingThatIsNotARegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link on Windows needs a privilege the test runner may not hold")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "set", "fixtures", "linked")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "secrets")); err != nil {
		t.Fatal(err)
	}
	err := skillab.CopyFixture(os.DirFS(root), "set", "linked", t.TempDir())
	if err == nil {
		t.Fatal("a fixture holding a symbolic link was copied")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("err = %v, want it to name the link", err)
	}
}

// The fixture is read off the front of the row, so the verifier stays the last field
// and can hold as many pipes as a shell command needs.
func TestParseExercisesReadsTheLeadingFixture(t *testing.T) {
	rows := []byte(
		"[broken-parser] the tests fail after my change | go test ./... 2>&1 | grep -q PASS\n" +
			"write me a parser | test -f parser.go\n")
	exercises, err := skillab.ParseExercises(rows, "exercises.txt", false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(exercises) != 2 {
		t.Fatalf("got %d exercises, want 2", len(exercises))
	}
	if exercises[0].Fixture != "broken-parser" {
		t.Errorf("fixture = %q, want broken-parser", exercises[0].Fixture)
	}
	if exercises[0].Objective != "the tests fail after my change" {
		t.Errorf("objective = %q, want the bracket stripped", exercises[0].Objective)
	}
	if want := "go test ./... 2>&1 | grep -q PASS"; exercises[0].Verify != want {
		t.Errorf("verify = %q, want %q: a pipe belongs to the shell, not to the row", exercises[0].Verify, want)
	}
	if exercises[1].Fixture != "" {
		t.Errorf("fixture = %q, want none: this exercise starts from nothing", exercises[1].Fixture)
	}
}

// An unclosed bracket is an error rather than an objective that happens to start with
// one. Reading it as prose would run the exercise in an empty directory and report
// the result as though the state had been there.
func TestParseExercisesRefusesAnUnclosedFixture(t *testing.T) {
	_, err := skillab.ParseExercises([]byte("[broken-parser the tests fail | exit 0\n"), "exercises.txt", false)
	if err == nil {
		t.Fatal("an unclosed fixture was read as prose")
	}
	if !strings.Contains(err.Error(), "never closed") {
		t.Errorf("err = %v, want it to name the bracket", err)
	}
}

// A named fixture that is not there is refused when the set loads, not when the trial
// that needs it starts. By then the harness has been spending on model calls for
// minutes, and the run it fails is one arm of one pair, which leaves the report
// unable to say what the missing half would have done.
func TestLoadSetRefusesAFixtureThatIsNotThere(t *testing.T) {
	fsys := fstest.MapFS{
		"set/" + skillab.ExercisesFile: {Data: []byte("[gone] fix it | exit 0\n")},
	}
	_, err := skillab.LoadSet(fsys, "set", "systematic-debugging")
	if err == nil {
		t.Fatal("a set naming a fixture that does not exist loaded")
	}
	if !strings.Contains(err.Error(), "gone") || !strings.Contains(err.Error(), skillab.FixturesDir) {
		t.Errorf("err = %v, want it to name the fixture and where it should be", err)
	}
}

// Materialise is the whole of what a trial needs: the state for this exercise, in this
// working directory. An exercise with no fixture starts in the empty directory it was
// given, which is what an exercise that builds something wants.
func TestMaterialiseSeedsOnlyWhatTheExerciseNames(t *testing.T) {
	fsys := fstest.MapFS{
		"set/fixtures/seeded/main.go":  {Data: []byte("package main\n")},
		"set/" + skillab.ExercisesFile: {Data: []byte("[seeded] fix it | exit 0\nbuild it | exit 0\n")},
	}
	set, err := skillab.LoadSet(fsys, "set", "systematic-debugging")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	seeded, empty := t.TempDir(), t.TempDir()
	if err := set.Materialise(set.Exercises[0], seeded); err != nil {
		t.Fatalf("materialise the seeded exercise: %v", err)
	}
	if err := set.Materialise(set.Exercises[1], empty); err != nil {
		t.Fatalf("materialise the exercise with no fixture: %v", err)
	}
	if _, err := os.Stat(filepath.Join(seeded, "main.go")); err != nil {
		t.Errorf("the fixture did not arrive: %v", err)
	}
	entries, err := os.ReadDir(empty)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("an exercise with no fixture started with %d file(s) in its directory", len(entries))
	}
}
