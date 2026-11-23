package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/skill/skillab"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
	"github.com/ionalpha/flynn/usage"
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
// It spends model calls, on demand only. There is no cheaper way to
// answer whether a procedure helps a model that has it: the retrieval table says
// whether a skill is reachable and costs nothing, and this says whether reaching it
// was worth the tokens.
func runSkillAB(args []string, modelSpec, dataDir string, out io.Writer) error {
	set, repeats, flags, err := skillABArgs(args, out)
	if err != nil {
		return err
	}
	limit, table, err := flags.spend()
	if err != nil {
		return err
	}
	ctx := context.Background()
	arms, err := resolveArms(ctx, set.Skill, flags.versionRefs)
	if err != nil {
		return err
	}
	defer arms.close()
	model, plan, spec, err := resolveModelOrOnboard(ctx, modelSpec, modelSpecExplicit, dataDir)
	if err != nil {
		return err
	}
	price := newPricer(table, spec)
	return measureSkill(ctx, out, set, repeats, arms.names, limit, func(ctx context.Context, exercise skillab.Exercise, repeat int, candidate bool) (skillab.Outcome, error) {
		return runTrial(ctx, out, model, price, plan, set, exercise, repeat, candidate, arms, flags.keep)
	})
}

// abArms is what each arm of a comparison runs. A nil version is the skill as
// installed; a baseline that is nil too is no skill at all, so the zero value is the
// skill against its absence.
type abArms struct {
	names     skillab.Arms
	candidate *skillVersion
	baseline  *skillVersion
}

func (a abArms) close() {
	a.candidate.close()
	a.baseline.close()
}

// resolveArms reads both versions before anything is spent.
func resolveArms(ctx context.Context, slug string, refs versionRefs) (abArms, error) {
	var arms abArms
	if refs.candidate == "" && refs.against == "" {
		return arms, nil
	}
	arms.names = skillab.Arms{Candidate: slug + " as installed", Baseline: "without the skill"}
	if refs.candidate != "" {
		v, err := resolveVersion(ctx, slug, refs.candidate)
		if err != nil {
			return abArms{}, err
		}
		arms.candidate, arms.names.Candidate = v, v.label
	}
	if refs.against != "" {
		v, err := resolveVersion(ctx, slug, refs.against)
		if err != nil {
			arms.close()
			return abArms{}, err
		}
		arms.baseline, arms.names.Baseline = v, v.label
	}
	return arms, nil
}

// skillABArgs parses the command's arguments and loads the exercise set. Everything it
// can refuse, it refuses here, before a model is resolved or a store is opened: a
// missing exercise set costs nothing to detect and would otherwise be found after the
// harness had already started charging for runs.
func skillABArgs(args []string, out io.Writer) (skillab.Set, int, abFlags, error) {
	fs := flag.NewFlagSet("skill ab", flag.ContinueOnError)
	fs.SetOutput(out)
	repeats := fs.Int("repeats", 3, "runs per exercise per condition; the variance between runs is larger than the effect")
	root := fs.String("exercises", defaultEvalRoot, "directory holding each skill's exercise set, one subdirectory per skill")
	var refs abFlags
	fs.StringVar(&refs.maxCost, "max-cost", "", "stop starting new attempts once this much has been spent, in USD (for example 2 or 0.50); needs --prices to know what a call cost")
	fs.StringVar(&refs.prices, "prices", "", "a JSON price table (usage.LoadTable format) pricing the model's reported tokens; without one, tokens are reported and cost is unknown")
	fs.StringVar(&refs.against, "against", "", "a SKILL.md, a directory holding one, or a git ref: the version the skill is compared against, instead of no skill")
	fs.StringVar(&refs.candidate, "candidate", "", "the version under test, as a path or git ref; the installed skill when empty")
	fs.StringVar(&refs.keep, "keep", "", "a directory to save each attempt's report, transcript and check output in, one subdirectory per attempt; nothing is kept when empty")
	// Parsed around the skill's name rather than up to it. Go's flag package stops at
	// the first argument that is not a flag, so the usage line this command prints,
	// with the name before the flags, is the one spelling it would otherwise refuse.
	bare, err := parseAroundArgs(fs, args)
	if err != nil {
		return skillab.Set{}, 0, refs, err
	}
	if len(bare) != 1 {
		return skillab.Set{}, 0, refs, errors.New(`usage: flynn skill ab <skill> [--repeats n] [--exercises dir] [--candidate version] [--against version] [--max-cost usd] [--prices file] [--keep dir]`)
	}
	slug := bare[0]

	set, err := skillab.LoadDir(filepath.Join(*root, slug), slug)
	if err != nil {
		return skillab.Set{}, 0, refs, err
	}
	if set.Holdout() == 0 {
		// Said out loud and not refused. A set with no holdout still measures something;
		// what it cannot do is tell a skill that helps from a skill written to pass the
		// exercises its own author chose.
		_, _ = fmt.Fprintf(out, "note: %s has no %s, so nothing here is held back from whoever wrote the skill\n",
			slug, skillab.HoldoutFile)
	}
	return set, *repeats, refs, nil
}

// abFlags are the options of `skill ab` beyond the exercise set: which versions to
// compare, what the run may spend and where its evidence is kept.
type abFlags struct {
	versionRefs
	maxCost string
	prices  string
	keep    string
}

// spend reads the spend options before any model is resolved. A table that is named
// and unreadable is refused. With a limit and no table the run still starts: it reports
// tokens and stops at its first limit check, which says the spend could not be read.
func (f abFlags) spend() (skillab.Limit, *usage.Table, error) {
	var table *usage.Table
	if f.prices != "" {
		file, err := os.Open(f.prices)
		if err != nil {
			return skillab.Limit{}, nil, fmt.Errorf("--prices: %w", err)
		}
		defer func() { _ = file.Close() }()
		if table, err = usage.LoadTable(file); err != nil {
			return skillab.Limit{}, nil, fmt.Errorf("--prices %s: %w", f.prices, err)
		}
	}
	if f.maxCost == "" {
		return skillab.Limit{}, table, nil
	}
	n, err := usage.ParseNanos(f.maxCost)
	if err != nil || n == 0 {
		return skillab.Limit{}, nil, fmt.Errorf("--max-cost %q: want a positive amount in USD such as 2 or 0.50", f.maxCost)
	}
	return skillab.Limit{Money: n, Currency: "USD"}, table, nil
}

// pricer prices one model's reported tokens from a table. A nil table prices nothing,
// so every turn is counted as unpriced and none is guessed.
type pricer struct {
	table           *usage.Table
	provider, model string
}

// newPricer splits spec, the canonical "provider:model", into the names a table keys on.
func newPricer(table *usage.Table, spec string) *pricer {
	provider, model, _ := strings.Cut(spec, ":")
	return &pricer{table: table, provider: provider, model: model}
}

// turn is the cost of one returned call: its tokens, and a price only if the table has one.
func (p *pricer) turn(u llm.Usage, at time.Time) skillab.Cost {
	c := skillab.Cost{Turns: 1, Tokens: usage.FromLLM(u)}
	if p == nil || p.table == nil {
		c.Unpriced = 1
		return c
	}
	priced := p.table.Price(usage.Record{Provider: p.provider, Model: p.model, At: at, Tokens: usage.FromLLM(u)})
	if !priced.Known {
		c.Unpriced = 1
		return c
	}
	c.Money, c.Currency = priced.Amount, priced.Currency
	return c
}

// endingOf classifies how a run that returned an error stopped.
func endingOf(runErr error) skillab.Ending {
	switch stoppedOutcome(runErr) {
	case resultBudget:
		return skillab.BudgetExhausted
	case resultStalled:
		return skillab.Stalled
	}
	return skillab.Aborted
}

// parseAroundArgs parses fs over args and returns the arguments that are not flags,
// in the order they appeared. It re-parses from just after each one, which is what
// lets a flag follow a positional argument: the flag set keeps deciding which tokens
// are flag values, so a value that looks like a name is still read as a value.
func parseAroundArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var bare []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return bare, nil
		}
		bare = append(bare, fs.Arg(0))
		rest = fs.Args()[1:]
	}
}

// measureSkill runs the measurement and writes the report. It takes the trial
// rather than the model, so the half that decides what a reader is told is separable
// from the half that spends money on model calls.
func measureSkill(ctx context.Context, out io.Writer, set skillab.Set, repeats int, arms skillab.Arms, limit skillab.Limit, trial skillab.Trial) error {
	if repeats < 1 {
		repeats = 1
	}
	_, _ = fmt.Fprintf(out, "measuring %s over %d exercise(s) x %d repeat(s), both conditions: %d runs\n",
		set.Skill, len(set.Exercises), repeats, 2*len(set.Exercises)*repeats)
	report, err := skillab.CompareWithin(ctx, set, repeats, arms, trial, limit)
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
// workspace, graded after the agent has stopped by the exercise's verifier and its
// named checks.
//
// Everything is fresh. A new data directory, so no skill learned or memory written
// by an earlier trial is recalled by this one; a new working directory, so the state
// a previous run left behind cannot make the next exercise easier. Learning is off for
// the same reason, and because a measurement that changed the thing it measures
// would be reporting on a library that no longer exists.
//
// An arm that runs a named version installs it in its own store and reads its
// references from its own tree, so the baseline never sees the candidate's files.
// With keep set, [keepAttempt] saves the evidence before the directory is removed.
func runTrial(ctx context.Context, out io.Writer, model llm.Model, price *pricer, plan harness.Plan, set skillab.Set, exercise skillab.Exercise, repeat int, candidate bool, arms abArms, keep string) (skillab.Outcome, error) {
	slug := set.Skill
	arm, version := "without", arms.baseline
	if candidate {
		arm, version = "with", arms.candidate
	}
	if arms.candidate != nil || arms.baseline != nil {
		arm = "baseline"
		if candidate {
			arm = "candidate"
		}
	}
	_, _ = fmt.Fprintf(out, "  [%s, repeat %d] %s\n", arm, repeat, exercise.Objective)

	dataDir, err := os.MkdirTemp("", "flynn-ab-data-")
	if err != nil {
		return skillab.Outcome{}, err
	}
	defer func() { _ = os.RemoveAll(dataDir) }()
	workdir, err := os.MkdirTemp("", "flynn-ab-work-")
	if err != nil {
		return skillab.Outcome{}, err
	}
	defer func() { _ = os.RemoveAll(workdir) }()
	// The state the exercise starts from, copied fresh for every trial. Both arms get
	// the same one, and neither inherits what the previous run did to it.
	if err := set.Materialise(exercise, workdir); err != nil {
		return skillab.Outcome{}, err
	}

	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return skillab.Outcome{}, err
	}
	defer func() { _ = store.Close() }()
	opts, err := armOptions(ctx, store, slug, version, candidate)
	if err != nil {
		return skillab.Outcome{}, err
	}

	// The run's own error is not the measurement. A run that stopped short failed the
	// exercise, which is an observation; the verifier says so on its own, and it is the
	// only thing that does.
	var clk clock.Clock = clock.System{}
	metered := newMeteredModel(model, price, clk)
	start := clk.Now()
	report, rerr := runLearningMission(ctx, io.Discard, metered, plan, nil, workdir, exercise.Objective, "", store, nil, false, nil, opts...)
	wall := clk.Now().Sub(start)
	cost, transcript, last := metered.snapshot()
	cost.Wall = wall
	var ended skillab.Ending
	if rerr != nil {
		if ctx.Err() != nil {
			return skillab.Outcome{}, ctx.Err()
		}
		if errors.Is(rerr, errSkillNotLoaded) {
			return skillab.Outcome{}, rerr
		}
		ended = endingOf(rerr)
		_, _ = fmt.Fprintf(out, "    (the run stopped, %s: %v)\n", ended, rerr)
	}
	defer func() { _, _ = fmt.Fprintf(out, "    cost: %s\n", cost) }()
	if report == "" {
		report = last
	}
	outcome := skillab.Outcome{Ended: ended, Cost: cost, Checks: map[string]bool{}}
	for _, c := range exercise.Checks {
		outcome.Checks[c.Name] = false
	}
	// A directory the harness cannot place its files in is the agent's doing, and the
	// attempt fails every check; aborting would discard the paid runs that came before.
	if err := set.Stage(exercise, workdir, report, transcript); err != nil {
		_, _ = fmt.Fprintf(out, "    (the harness files could not be placed, so the attempt failed every check: %v)\n", err)
		return outcome, nil
	}
	var said strings.Builder
	var verifyOut string
	outcome.Passed, verifyOut = verifyWithOutput(ctx, workdir, exercise.Verify)
	fmt.Fprintf(&said, "== verify (passed: %t)\n%s\n", outcome.Passed, verifyOut)
	for _, c := range exercise.Checks {
		var checkOut string
		outcome.Checks[c.Name], checkOut = verifyWithOutput(ctx, workdir, c.Command)
		fmt.Fprintf(&said, "== %s (passed: %t)\n%s\n", c.Name, outcome.Checks[c.Name], checkOut)
	}
	if keep != "" {
		if ended != skillab.Finished {
			fmt.Fprintf(&said, "== the run stopped: %s: %v\n", ended, rerr)
		}
		dir, err := keepAttempt(keep, exercise.Fixture, arm, repeat, map[string]string{
			"report.txt":     report,
			"transcript.txt": transcript,
			"system.txt":     metered.firstSystem(),
			"checks.txt":     said.String(),
		})
		if err != nil {
			// Not fatal: the attempt is paid for and scored, and losing its evidence
			// should not throw away the pairs already run.
			_, _ = fmt.Fprintf(out, "    (the attempt's evidence could not be kept: %v)\n", err)
		} else {
			_, _ = fmt.Fprintf(out, "    kept: %s\n", dir)
		}
	}
	return outcome, nil
}

// keepAttempt writes one attempt's evidence, files by name, to
// keep/<fixture>/<arm>-<repeat>. A directory already there is refused rather than
// merged, so files from two runs never mix.
func keepAttempt(keep, fixture, arm string, repeat int, files map[string]string) (string, error) {
	if fixture == "" {
		fixture = "no-fixture"
	}
	dir := filepath.Join(keep, fixture, fmt.Sprintf("%s-%d", arm, repeat))
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		return "", err
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// armOptions prepares one arm's store and returns the run options that make it that
// arm: a named version installed with its own resources, or the skill removed for the
// arm without it.
//
// Every arm that has the skill starts with its full text. Only offered, it reaches the
// model when search ranks it among the few skills listed and the model then opens it,
// and either miss makes the arm the same as one without the skill.
func armOptions(ctx context.Context, store *sqlite.Store, slug string, version *skillVersion, candidate bool) ([]driveOption, error) {
	var opts []driveOption
	switch {
	case version != nil:
		if err := version.install(ctx, store.Skills()); err != nil {
			return nil, err
		}
		opts = append(opts, withSkills(skilltool.New(store.Skills(), skilltool.WithPack(version.fsys, version.root))))
	case !candidate:
		return nil, pruneSkill(ctx, store, slug)
	}
	return append(opts, withLoadedSkills(slug)), nil
}

// pruneSkill removes one skill from the library for the arm without it, so the model
// can neither be given it nor find it. One skill and not the pack: a run stripped of every skill
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
