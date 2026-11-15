// Package skillab measures whether a skill helps: on the exercises it claims, does a
// run with it beat a run without it? Each exercise ends in a command that passes or
// fails. The two conditions differ by one skill, and runs repeat because run-to-run
// variance exceeds the effect.
//
// [NoDifference] is a verdict, and the action is deleting the skill. A set where
// every run passes in both conditions cannot tell them apart, and [Report] says so.
//
// Exercises live outside the shipped pack, so the agent cannot read them through
// skill_resource; a check the agent can see is a target. The holdout half should be
// written by someone other than the skill's author.
package skillab

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Exercise is an objective and a Verify command whose exit status is the verdict.
// Verify runs after the agent stops, so nothing the model says about its work
// counts.
type Exercise struct {
	Objective string
	Verify    string
	// Fixture names the directory under [FixturesDir] copied in before the run;
	// empty starts in an empty directory.
	Fixture string
	// Holdout marks the held-out half. A gap between the halves shows a skill
	// written against its own eval.
	Holdout bool
	// Source and Line locate the row for error messages.
	Source string
	Line   int
}

// Name is the exercise's label in a report, its objective.
func (t Exercise) Name() string { return t.Objective }

// Set is one skill's exercise set, open half and holdout half together.
type Set struct {
	Skill     string
	Exercises []Exercise
	// fsys and dir locate the set for [Set.Materialise].
	fsys fs.FS
	dir  string
}

// Materialise copies e's fixture into dest, the trial's working directory.
func (s Set) Materialise(e Exercise, dest string) error {
	if e.Fixture == "" {
		return nil
	}
	return CopyFixture(s.fsys, s.dir, e.Fixture, dest)
}

// Holdout reports how many of the set's exercises are held out.
func (s Set) Holdout() int {
	n := 0
	for _, t := range s.Exercises {
		if t.Holdout {
			n++
		}
	}
	return n
}

const (
	// ExercisesFile is the open half of the set.
	ExercisesFile = "exercises.txt"
	// HoldoutFile is the half kept from the skill's author. It is optional, but a
	// skill measured only on its author's exercises was measured against itself.
	HoldoutFile = "holdout.txt"
)

// LoadSet reads [ExercisesFile] and, if present, [HoldoutFile] from dir. A missing
// ExercisesFile is an error, so the harness never reports on nothing.
func LoadSet(fsys fs.FS, dir, skill string) (Set, error) {
	set := Set{Skill: skill, fsys: fsys, dir: dir}
	open, err := fs.ReadFile(fsys, path(dir, ExercisesFile))
	if err != nil {
		return Set{}, fmt.Errorf("skillab: %s has no exercise set: %w", skill, err)
	}
	exercises, err := ParseExercises(open, ExercisesFile, false)
	if err != nil {
		return Set{}, err
	}
	set.Exercises = exercises

	held, err := fs.ReadFile(fsys, path(dir, HoldoutFile))
	switch {
	case err == nil:
		heldExercises, herr := ParseExercises(held, HoldoutFile, true)
		if herr != nil {
			return Set{}, herr
		}
		set.Exercises = append(set.Exercises, heldExercises...)
	case !errors.Is(err, fs.ErrNotExist):
		return Set{}, fmt.Errorf("skillab: %s: read the holdout: %w", skill, err)
	}
	if len(set.Exercises) == 0 {
		return Set{}, fmt.Errorf("skillab: %s has an empty exercise set", skill)
	}
	// A missing fixture is refused at load, before any model calls are paid for and
	// before it can fail one arm of a pair.
	for _, t := range set.Exercises {
		if t.Fixture == "" {
			continue
		}
		if _, err := fs.Stat(fsys, path(dir, fixturePath(t.Fixture))); err != nil {
			return Set{}, fmt.Errorf("skillab: %s line %d names fixture %s, which is not in %s: %w",
				t.Source, t.Line, t.Fixture, FixturesDir, err)
		}
	}
	return set, nil
}

// LoadDir reads a skill's exercise set from a directory on disk.
func LoadDir(dir, skill string) (Set, error) {
	return LoadSet(os.DirFS(dir), ".", skill)
}

// path joins with a slash, because fs.FS uses one on every host.
func path(dir, file string) string {
	dir = strings.TrimSuffix(filepath.ToSlash(dir), "/")
	if dir == "" || dir == "." {
		return file
	}
	return dir + "/" + file
}

const exerciseSep = "|"

// ParseExercises reads rows of `objective | verify command`, with `#` comments and
// blank lines. Both columns are required. A row may open with a fixture,
// `[broken-parser] objective | verify`, so the verifier stays last and can hold
// pipes.
func ParseExercises(data []byte, source string, holdout bool) ([]Exercise, error) {
	var out []Exercise
	for i, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line := raw
		if h := strings.Index(line, "#"); h >= 0 {
			line = line[:h]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		fixture, line, err := splitFixture(line, source, i+1)
		if err != nil {
			return nil, err
		}
		cols := strings.SplitN(line, exerciseSep, 2)
		if len(cols) != 2 {
			return nil, fmt.Errorf("skillab: %s line %d: no verifier; an exercise is `objective %s command`", source, i+1, exerciseSep)
		}
		t := Exercise{
			Objective: strings.TrimSpace(cols[0]),
			Verify:    strings.TrimSpace(cols[1]),
			Fixture:   fixture,
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

// splitFixture treats an unclosed [ as an error, because reading it as prose would
// run the exercise without its fixture.
func splitFixture(line, source string, num int) (fixture, rest string, err error) {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "[") {
		return "", line, nil
	}
	end := strings.Index(trimmed, "]")
	if end < 0 {
		return "", "", fmt.Errorf("skillab: %s line %d: a fixture opens with [ and is never closed", source, num)
	}
	fixture = strings.TrimSpace(trimmed[1:end])
	if verr := ValidateFixture(fixture); verr != nil {
		return "", "", fmt.Errorf("%s line %d: %w", source, num, verr)
	}
	return fixture, trimmed[end+1:], nil
}
