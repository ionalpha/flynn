package skillab_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ionalpha/flynn/skill/skillab"
)

func TestParseTasks(t *testing.T) {
	tasks, err := skillab.ParseTasks([]byte(`
# the objectives this skill claims to cover

the tests fail after my change and I cannot see why | go test ./... # a trailing comment
migrate the users table without downtime | ./verify-migration.sh
`), skillab.TasksFile, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("%d tasks, want 2", len(tasks))
	}
	if tasks[0].Objective != "the tests fail after my change and I cannot see why" {
		t.Errorf("objective = %q", tasks[0].Objective)
	}
	if tasks[0].Verify != "go test ./..." {
		t.Errorf("verifier = %q, want the command without the comment", tasks[0].Verify)
	}
	// The line is the file's, so a task that never decides anything names a row to
	// rewrite rather than a string to search for.
	if tasks[0].Line != 4 || tasks[0].Source != skillab.TasksFile {
		t.Errorf("located at %s:%d, want %s:4", tasks[0].Source, tasks[0].Line, skillab.TasksFile)
	}
	if tasks[0].Holdout {
		t.Error("a task from the open half was marked held out")
	}
}

// Both columns are required and the refusal says which is missing. A task with no
// verifier has no outcome, so admitting one would put a run in the tally that
// nothing graded.
func TestParseTasksRefusesARowWithNoOutcome(t *testing.T) {
	for name, in := range map[string]string{
		"no verifier at all": "the tests fail after my change",
		"empty verifier":     "the tests fail after my change |   ",
		"no objective":       "  | go test ./...",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := skillab.ParseTasks([]byte(in), skillab.TasksFile, false); err == nil {
				t.Fatalf("parsed %q, want a refusal", in)
			}
		})
	}
}

func TestLoadSet(t *testing.T) {
	fsys := fstest.MapFS{
		"evals/systematic-debugging/tasks.txt": &fstest.MapFile{Data: []byte(
			"the tests fail after my change | go test ./...\nthe build broke overnight | go build ./...\n")},
		"evals/systematic-debugging/holdout.txt": &fstest.MapFile{Data: []byte(
			"a flaky test passes on a rerun | ./flaky-check.sh\n")},
	}
	got, err := skillab.LoadSet(fsys, "evals/systematic-debugging", "systematic-debugging")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Tasks) != 3 {
		t.Fatalf("%d tasks, want both halves", len(got.Tasks))
	}
	if got.Holdout() != 1 {
		t.Errorf("%d held out, want 1", got.Holdout())
	}
	if !got.Tasks[2].Holdout || got.Tasks[2].Source != skillab.HoldoutFile {
		t.Errorf("the held-out task was not tagged: %+v", got.Tasks[2])
	}
}

// A skill with no task set is an error rather than an empty report. The harness
// exists to say whether a skill earns its place, and reporting "no measurable
// difference" over zero runs would be that sentence with nothing behind it.
func TestLoadSetRefusesAMissingOrEmptyTaskSet(t *testing.T) {
	if _, err := skillab.LoadSet(fstest.MapFS{}, "evals/nothing", "nothing"); err == nil {
		t.Fatal("a skill with no task set loaded")
	} else if !strings.Contains(err.Error(), "no task set") {
		t.Errorf("err = %v, want it to say the task set is missing", err)
	}

	empty := fstest.MapFS{"evals/blank/tasks.txt": &fstest.MapFile{Data: []byte("# nothing yet\n")}}
	if _, err := skillab.LoadSet(empty, "evals/blank", "blank"); err == nil {
		t.Fatal("a task set of comments loaded")
	}
}

// The holdout is optional to load and not optional to have: a set without one still
// measures, and the report shows a holdout of zero rather than the harness inventing
// tasks or refusing to run.
func TestLoadSetWithoutAHoldout(t *testing.T) {
	fsys := fstest.MapFS{"evals/x/tasks.txt": &fstest.MapFile{Data: []byte("do the thing | exit 0\n")}}
	got, err := skillab.LoadSet(fsys, "evals/x", "x")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Tasks) != 1 || got.Holdout() != 0 {
		t.Fatalf("got %d tasks, %d held out; want one open task", len(got.Tasks), got.Holdout())
	}
}

// LoadDir is the same load from a directory on disk, which is where an author keeps
// a task set while writing the skill.
func TestLoadDirReadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(skillab.TasksFile, "the tests fail after my change | go test ./...\n")
	write(skillab.HoldoutFile, "a flaky test passes on a rerun | ./flaky-check.sh\n")

	got, err := skillab.LoadDir(dir, "systematic-debugging")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Tasks) != 2 || got.Holdout() != 1 {
		t.Fatalf("loaded %d tasks with %d held out, want 2 and 1", len(got.Tasks), got.Holdout())
	}
}

// A malformed row in either half stops the load, and the message names which file.
// Guessing at what a broken row meant would put a task in the tally that nobody
// wrote.
func TestLoadSetRefusesAMalformedRowInEitherHalf(t *testing.T) {
	good := "the tests fail after my change | go test ./...\n"
	bad := "a row with no verifier at all\n"
	for name, files := range map[string]map[string]string{
		"in the open half":     {skillab.TasksFile: bad},
		"in the held-out half": {skillab.TasksFile: good, skillab.HoldoutFile: bad},
	} {
		t.Run(name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for file, body := range files {
				fsys["evals/x/"+file] = &fstest.MapFile{Data: []byte(body)}
			}
			_, err := skillab.LoadSet(fsys, "evals/x", "x")
			if err == nil {
				t.Fatal("a malformed row loaded")
			}
			want := skillab.TasksFile
			if len(files) == 2 {
				want = skillab.HoldoutFile
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to name %s", err, want)
			}
		})
	}
}
