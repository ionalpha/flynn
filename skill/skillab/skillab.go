// Package skillab measures whether a skill helps.
//
// Every published skill library was written by people confident their skills
// worked, and none of them measured it. The strongest sourcing anyone offers is a
// count of remembered failures, which is anecdote. A library that wants to claim
// more than taste needs an instrument that existed before the content, because an
// instrument built afterwards is built to agree with what was already written.
//
// The question it answers is narrow on purpose: for this skill, on tasks it claims
// to cover, does a run with it beat a run without it? Everything here serves that.
// A task set per skill, each task ending in a command that passes or fails on its
// own. Two conditions that differ by one skill. Repeats, because the variance
// between runs is larger than the effect being measured. One verdict.
//
// # No measurable difference is a verdict
//
// It has to be able to say a skill did nothing, and that has to be a result someone
// acts on by deleting the skill. A harness whose only outcomes are "helped" and
// "helped a lot" measures nothing, and a task set where every run passes in both
// conditions is a task set that cannot tell the two apart. Report says so rather
// than reporting a clean pass.
//
// # The task set is not in the pack
//
// A skill's tasks live outside the tree the pack ships, so the agent cannot read
// them through skill_resource while working. A check the agent can see is a target
// rather than a measurement, and the same is true of an eval: a skill authored
// against its own tasks passes its own tasks. The holdout half exists for the same
// reason, and is worth writing by someone other than the skill's author.
package skillab

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Task is one measurable job: an objective in the words a user would give it, and a
// command whose exit status is the whole verdict on whether the run did it.
//
// The verifier is deterministic and is run after the agent stops, so nothing the
// model says about its own work counts. That is the same instrument the skill
// `check` field uses and the same shape the published benchmarks use, which is what
// makes a result here comparable to one there.
type Task struct {
	Objective string
	Verify    string
	// Holdout marks a task from the held-out half of the set: written to measure the
	// skill rather than by whoever wrote it. A gap between the two halves is the
	// signature of a skill authored against its own eval.
	Holdout bool
	// Source and Line locate the row, so a failure to parse or a task that never
	// passes in either condition names somewhere to edit.
	Source string
	Line   int
}

// Name is the short label a report uses for a task: the objective, since that is
// what distinguishes one row from another to a reader.
func (t Task) Name() string { return t.Objective }

// Set is one skill's task set, open half and holdout half together.
type Set struct {
	Skill string
	Tasks []Task
}

// Holdout reports how many of the set's tasks are held out.
func (s Set) Holdout() int {
	n := 0
	for _, t := range s.Tasks {
		if t.Holdout {
			n++
		}
	}
	return n
}

const (
	// TasksFile is the open half of a skill's task set, written alongside the skill.
	TasksFile = "tasks.txt"
	// HoldoutFile is the half kept back from the skill's author. It is optional here
	// and required by judgement: a skill measured only on the tasks its author chose
	// has been measured against itself.
	HoldoutFile = "holdout.txt"
)

// LoadSet reads a skill's task set from dir: TasksFile, plus HoldoutFile when it is
// there. A missing TasksFile is an error, because a skill with no task set has not
// been measured and the harness will not pretend otherwise by reporting on nothing.
func LoadSet(fsys fs.FS, dir, skill string) (Set, error) {
	set := Set{Skill: skill}
	open, err := fs.ReadFile(fsys, path(dir, TasksFile))
	if err != nil {
		return Set{}, fmt.Errorf("skillab: %s has no task set: %w", skill, err)
	}
	tasks, err := ParseTasks(open, TasksFile, false)
	if err != nil {
		return Set{}, err
	}
	set.Tasks = tasks

	held, err := fs.ReadFile(fsys, path(dir, HoldoutFile))
	switch {
	case err == nil:
		heldTasks, herr := ParseTasks(held, HoldoutFile, true)
		if herr != nil {
			return Set{}, herr
		}
		set.Tasks = append(set.Tasks, heldTasks...)
	case !errors.Is(err, fs.ErrNotExist):
		return Set{}, fmt.Errorf("skillab: %s: read the holdout: %w", skill, err)
	}
	if len(set.Tasks) == 0 {
		return Set{}, fmt.Errorf("skillab: %s has an empty task set", skill)
	}
	return set, nil
}

// LoadDir reads a skill's task set from a directory on disk, which is where an
// author keeps one while writing the skill.
func LoadDir(dir, skill string) (Set, error) {
	return LoadSet(os.DirFS(dir), ".", skill)
}

// path joins a directory and a file for an fs.FS, whose separator is always a slash
// regardless of the host.
func path(dir, file string) string {
	dir = strings.TrimSuffix(filepath.ToSlash(dir), "/")
	if dir == "" || dir == "." {
		return file
	}
	return dir + "/" + file
}

// taskSep separates a row's two columns. A pipe, so an objective and a shell command
// can each hold whatever punctuation they need.
const taskSep = "|"

// ParseTasks reads task rows: `objective | verify command`, one per line, with `#`
// opening a comment and blank lines ignored. Both columns are required, because a
// task with no verifier has no outcome and a verifier with no objective measures a
// run nobody asked for.
func ParseTasks(data []byte, source string, holdout bool) ([]Task, error) {
	var out []Task
	for i, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line := raw
		if h := strings.Index(line, "#"); h >= 0 {
			line = line[:h]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		cols := strings.SplitN(line, taskSep, 2)
		if len(cols) != 2 {
			return nil, fmt.Errorf("skillab: %s line %d: no verifier; a task is `objective %s command`", source, i+1, taskSep)
		}
		t := Task{
			Objective: strings.TrimSpace(cols[0]),
			Verify:    strings.TrimSpace(cols[1]),
			Holdout:   holdout,
			Source:    source,
			Line:      i + 1,
		}
		if t.Objective == "" {
			return nil, fmt.Errorf("skillab: %s line %d: no objective", source, i+1)
		}
		if t.Verify == "" {
			return nil, fmt.Errorf("skillab: %s line %d: no verifier, so nothing decides whether the run worked", source, i+1)
		}
		out = append(out, t)
	}
	return out, nil
}
