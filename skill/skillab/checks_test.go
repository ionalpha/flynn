package skillab_test

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/skill/skillab"
)

// Continuation lines add named checks to the row above, and the row's own fields do
// not change.
func TestParseExercisesReadsNamedChecks(t *testing.T) {
	src := "[parser] fix the parser | python3 -m pytest -q  # primary\n" +
		"+ heldout | python3 heldout.py\n" +
		"  + scope | diff -rq . .flynn-ab/pristine | grep -v flynn-ab\n" +
		"\n" +
		"second | exit 0\n"
	got, err := skillab.ParseExercises([]byte(src), skillab.ExercisesFile, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d exercises, want 2", len(got))
	}
	first := got[0]
	if first.Fixture != "parser" || first.Verify != "python3 -m pytest -q" {
		t.Errorf("row fields changed: %+v", first)
	}
	if len(first.Checks) != 2 || first.Checks[0].Name != "heldout" || first.Checks[1].Name != "scope" {
		t.Fatalf("checks = %+v", first.Checks)
	}
	if first.Checks[1].Command != "diff -rq . .flynn-ab/pristine | grep -v flynn-ab" {
		t.Errorf("a check's command was cut at its own pipe: %q", first.Checks[1].Command)
	}
	if len(got[1].Checks) != 0 {
		t.Errorf("a check leaked onto the next exercise: %+v", got[1].Checks)
	}
}

func TestParseExercisesRefusesAMalformedCheck(t *testing.T) {
	for name, src := range map[string]string{
		"orphan":          "+ heldout | exit 0\n",
		"after a comment": "# nothing yet\n+ heldout | exit 0\nrow | exit 0\n",
		"duplicate":       "row | exit 0\n+ a | exit 0\n+ a | exit 1\n",
		"reserved":        "row | exit 0\n+ verify | exit 0\n",
		"upper case":      "row | exit 0\n+ Held | exit 0\n",
		"leading hyphen":  "row | exit 0\n+ -a | exit 0\n",
		"space in name":   "row | exit 0\n+ held out | exit 0\n",
		"no name":         "row | exit 0\n+ | exit 0\n",
		"no command":      "row | exit 0\n+ a |\n",
		"no separator":    "row | exit 0\n+ a exit 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := skillab.ParseExercises([]byte(src), skillab.ExercisesFile, false); err == nil {
				t.Fatalf("accepted %q", src)
			}
		})
	}
	// Two exercises may use the same check name: uniqueness is per exercise.
	if _, err := skillab.ParseExercises([]byte("a | exit 0\n+ x | exit 0\nb | exit 0\n+ x | exit 0\n"), "f", false); err != nil {
		t.Errorf("a name reused across exercises was refused: %v", err)
	}
}

// The set that was already in the repository, written before checks existed, reads
// with none.
func TestAnExistingSetReadsWithoutChecks(t *testing.T) {
	set, err := skillab.LoadDir(filepath.Join("..", "..", "evals", "systematic-debugging"), "systematic-debugging")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			t.Skip("no checked-in exercise set")
		}
		t.Fatal(err)
	}
	for _, e := range set.Exercises {
		if len(e.Checks) != 0 {
			t.Errorf("%s gained checks: %+v", e.Name(), e.Checks)
		}
	}
	if len(set.Exercises) == 0 || strings.TrimSpace(set.Exercises[0].Verify) == "" {
		t.Errorf("the set did not load: %+v", set.Exercises)
	}
}
