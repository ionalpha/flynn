package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/skill/skillab"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// defaultEvalRoot is where a skill's exercise set lives: outside the pack, so the agent
// cannot read its own eval through skill_resource mid-run, and outside the binary,
// because it is an authoring instrument rather than something an install needs.
const defaultEvalRoot = "evals"

// runSkillAB measures one skill: it runs every exercise in the skill's set under both
// conditions and prints the verdict. Each trial gets a fresh store and a fresh
// working directory, so nothing a run learns or writes reaches the next one and the
// only difference between the two arms is the skill.
//
// It spends real model calls, deliberately and on demand. There is no cheaper way to
// answer whether a procedure helps a model that has it: the retrieval table says
// whether a skill is reachable and costs nothing, and this says whether reaching it
// was worth the tokens.
func runSkillAB(args []string, modelSpec, dataDir string, out io.Writer) error {
	set, repeats, err := skillABArgs(args, out)
	if err != nil {
		return err
	}
	ctx := context.Background()
	model, plan, _, err := resolveModelOrOnboard(ctx, modelSpec, modelSpecExplicit, dataDir)
	if err != nil {
		return err
	}
	return measureSkill(ctx, out, set, repeats, func(ctx context.Context, exercise skillab.Exercise, repeat int, withSkill bool) (bool, error) {
		return runTrial(ctx, out, model, plan, set.Skill, exercise, repeat, withSkill)
	})
}

// skillABArgs parses the command's arguments and loads the exercise set. Everything it
// can refuse, it refuses here, before a model is resolved or a store is opened: a
// missing exercise set costs nothing to detect and would otherwise be found after the
// harness had already started charging for runs.
func skillABArgs(args []string, out io.Writer) (skillab.Set, int, error) {
	fs := flag.NewFlagSet("skill ab", flag.ContinueOnError)
	fs.SetOutput(out)
	repeats := fs.Int("repeats", 3, "runs per exercise per condition; the variance between runs is larger than the effect")
	root := fs.String("exercises", defaultEvalRoot, "directory holding each skill's exercise set, one subdirectory per skill")
	if err := fs.Parse(args); err != nil {
		return skillab.Set{}, 0, err
	}
	if fs.NArg() != 1 {
		return skillab.Set{}, 0, errors.New(`usage: flynn skill ab <skill> [--repeats n] [--exercises dir]`)
	}
	slug := fs.Arg(0)

	set, err := skillab.LoadDir(filepath.Join(*root, slug), slug)
	if err != nil {
		return skillab.Set{}, 0, err
	}
	if set.Holdout() == 0 {
		// Said out loud and not refused. A set with no holdout still measures something;
		// what it cannot do is tell a skill that helps from a skill written to pass the
		// exercises its own author chose.
		_, _ = fmt.Fprintf(out, "note: %s has no %s, so nothing here is held back from whoever wrote the skill\n",
			slug, skillab.HoldoutFile)
	}
	return set, *repeats, nil
}

// measureSkill runs the measurement and writes the report. It takes the attempt
// rather than the model, so the half that decides what a reader is told is separable
// from the half that spends money on model calls.
func measureSkill(ctx context.Context, out io.Writer, set skillab.Set, repeats int, attempt skillab.Attempt) error {
	if repeats < 1 {
		repeats = 1
	}
	_, _ = fmt.Fprintf(out, "measuring %s over %d exercise(s) x %d repeat(s), both conditions: %d runs\n",
		set.Skill, len(set.Exercises), repeats, 2*len(set.Exercises)*repeats)
	report, err := skillab.Measure(ctx, set, repeats, attempt)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprint(out, report.String())
	if h := report.Holdout(); len(h.Pairs) > 0 {
		// The held-out half gets its own line and is never folded into the total. A
		// skill that helps on the exercises its author wrote and does nothing on the ones
		// someone else wrote has been fitted to its own eval, and one averaged verdict
		// hides exactly that.
		_, _ = fmt.Fprintf(out, "\nheld-out exercises alone: %s (%+.1f points, p=%.3f)\n", h.Verdict, h.Gain, h.P)
	}
	return nil
}

// runTrial is one arm of one pair: a run of the exercise's objective in a throwaway
// workspace, graded by the exercise's verifier after the agent has stopped.
//
// Everything is fresh. A new data directory, so no skill learned or memory written
// by an earlier trial is recalled by this one; a new working directory, so the state
// a previous run left behind cannot make the next exercise easier. Learning is off for
// the same reason, and because a measurement that changed the thing it measures
// would be reporting on a library that no longer exists.
func runTrial(ctx context.Context, out io.Writer, model llm.Model, plan harness.Plan, slug string, exercise skillab.Exercise, repeat int, withSkill bool) (bool, error) {
	arm := "without"
	if withSkill {
		arm = "with"
	}
	_, _ = fmt.Fprintf(out, "  [%s, repeat %d] %s\n", arm, repeat, exercise.Objective)

	dataDir, err := os.MkdirTemp("", "flynn-ab-data-")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(dataDir) }()
	workdir, err := os.MkdirTemp("", "flynn-ab-work-")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(workdir) }()

	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return false, err
	}
	defer func() { _ = store.Close() }()
	if !withSkill {
		if err := pruneSkill(ctx, store, slug); err != nil {
			return false, err
		}
	}

	// The run's own error is not the measurement. A run that stopped short failed the
	// exercise, which is an observation; the verifier says so on its own, and it is the
	// only thing that does.
	if _, rerr := runLearningMission(ctx, io.Discard, model, plan, nil, workdir, exercise.Objective, "", store, nil, false, nil); rerr != nil {
		_, _ = fmt.Fprintf(out, "    (the run stopped: %v)\n", rerr)
	}
	return runVerification(ctx, workdir, exercise.Verify), nil
}

// pruneSkill removes one skill from the library, which is the whole of what
// separates the two arms. One skill and not the pack: a run stripped of every skill
// differs from a full one in more ways than the measurement can attribute, and the
// question is what this skill contributes to a library that otherwise stands.
//
// A slug the store does not hold is refused. It means the skill under test is not
// installed, and measuring the difference between two identical conditions would
// report "no measurable difference" about a skill that was never there.
func pruneSkill(ctx context.Context, store *sqlite.Store, slug string) error {
	sk, err := store.Skills().Get(ctx, slug)
	if errors.Is(err, state.ErrNotFound) {
		return fmt.Errorf("skill %s is not in the library, so there is nothing to measure the absence of", slug)
	}
	if err != nil {
		return err
	}
	// By id, never by slug: Delete resolves a slug across every scope and would take
	// the wrong record whenever a learned skill shares the name with a bundled one.
	return store.Skills().Delete(ctx, sk.ID)
}
