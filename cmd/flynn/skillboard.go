package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/internal/fsatomic"
	"github.com/ionalpha/flynn/internal/procexec"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/skill/skillab"
	"github.com/ionalpha/flynn/usage"
)

// The arms a board can run. Each names one way of putting an agent to work on an
// exercise; the board compares them on the same verifier.
const (
	armFlynn       = "flynn"
	armFlynnClaude = "flynn-claude"
	armFlynnCodex  = "flynn-codex"
	armClaudeCode  = "claude-code"
	armCodex       = "codex"
)

var boardArmNames = []string{armFlynn, armFlynnClaude, armFlynnCodex, armClaudeCode, armCodex}

// boardFlags are the options of `skill board`.
type boardFlags struct {
	arms, only, skills      string
	repeats                 int
	root, prices, maxCost   string
	out, markdown, keep     string
	claudeModel, codexModel string
	runUSD                  string
	timeout                 time.Duration
	resume                  bool
}

func parseBoardArgs(args []string, out io.Writer) (boardFlags, error) {
	fs := flag.NewFlagSet("skill board", flag.ContinueOnError)
	fs.SetOutput(out)
	var f boardFlags
	fs.StringVar(&f.arms, "arms", strings.Join(boardArmNames, ","), "the arms to run, comma separated: "+strings.Join(boardArmNames, ", "))
	fs.StringVar(&f.only, "only", "", "run only these exercises, as skill/fixture, comma separated")
	fs.StringVar(&f.skills, "skills", "", "run every exercise of these skills, comma separated; every skill under --exercises when empty")
	fs.IntVar(&f.repeats, "repeats", 3, "runs per exercise per arm")
	fs.StringVar(&f.root, "exercises", defaultEvalRoot, "directory holding each skill's exercise set, one subdirectory per skill")
	fs.StringVar(&f.prices, "prices", "", "a JSON price table (usage.LoadTable format) pricing every arm's reported tokens")
	fs.StringVar(&f.maxCost, "max-cost", "", "stop starting attempts once this much has been spent, in USD; needs --prices")
	fs.StringVar(&f.out, "out", "", "write the board as JSON here")
	fs.StringVar(&f.markdown, "markdown", "", "write the board's tables as Markdown here")
	fs.StringVar(&f.keep, "keep", "", "a directory to save each attempt's report, transcript and verifier output in")
	fs.StringVar(&f.claudeModel, "claude-model", "claude-sonnet-5-5", "the model the claude-code and flynn-claude arms run")
	fs.StringVar(&f.codexModel, "codex-model", "gpt-5.5", "the model the codex and flynn-codex arms run")
	fs.StringVar(&f.runUSD, "run-max-usd", "2", "the spend cap handed to the claude-code arm for one attempt (its --max-budget-usd)")
	fs.DurationVar(&f.timeout, "timeout", 20*time.Minute, "the longest one attempt may run before it is stopped and failed")
	fs.BoolVar(&f.resume, "resume", false, "keep the attempts already in --out and run only the missing and unevaluated ones; --max-cost then covers this invocation alone")
	bare, err := parseAroundArgs(fs, args)
	if err != nil {
		return f, err
	}
	if len(bare) != 0 {
		return f, usageErr("skill board")
	}
	for _, a := range splitList(f.arms) {
		if !slices.Contains(boardArmNames, a) {
			return f, fmt.Errorf("--arms: unknown arm %q; want one of %s", a, strings.Join(boardArmNames, ", "))
		}
	}
	if len(splitList(f.arms)) == 0 {
		return f, errors.New("--arms names no arm")
	}
	if f.resume && f.out == "" {
		return f, errors.New("--resume needs --out, the board it continues")
	}
	return f, nil
}

// boardEntries loads the exercises a board runs, in skill order and then file order. A
// name in --only that matches nothing is refused before anything is spent, because a
// board that silently ran fewer exercises than asked would read as complete.
func boardEntries(root, skills, only string) ([]skillab.BoardEntry, error) {
	names := splitList(skills)
	wanted := map[string]bool{}
	for _, o := range splitList(only) {
		skill, _, ok := strings.Cut(o, "/")
		if !ok {
			return nil, fmt.Errorf("--only %q: want skill/fixture", o)
		}
		wanted[o] = false
		if !slices.Contains(names, skill) {
			names = append(names, skill)
		}
	}
	if len(names) == 0 {
		dirs, err := os.ReadDir(root)
		if err != nil {
			return nil, fmt.Errorf("--exercises: %w", err)
		}
		for _, d := range dirs {
			if _, err := os.Stat(filepath.Join(root, d.Name(), skillab.ExercisesFile)); d.IsDir() && err == nil {
				names = append(names, d.Name())
			}
		}
	}
	sort.Strings(names)
	var entries []skillab.BoardEntry
	for _, name := range names {
		set, err := skillab.LoadDir(filepath.Join(root, name), name)
		if err != nil {
			return nil, err
		}
		for _, e := range set.Exercises {
			entry := skillab.BoardEntry{Set: set, Exercise: e}
			skill, fixture := entry.Key()
			if len(wanted) > 0 {
				if _, ok := wanted[skill+"/"+fixture]; !ok {
					continue
				}
				wanted[skill+"/"+fixture] = true
			}
			entries = append(entries, entry)
		}
	}
	for o, found := range wanted {
		if !found {
			return nil, fmt.Errorf("--only %s: no such exercise under %s", o, root)
		}
	}
	if len(entries) == 0 {
		return nil, errors.New("the board has no exercises to run")
	}
	return entries, nil
}

// boardEnv is what the arms need from the machine: where to find a CLI, the credential
// store keys are read from, and how to resolve Flynn's own model. Tests replace all
// three, so no test reads the developer's credentials or finds their CLIs.
type boardEnv struct {
	lookPath func(string) (string, error)
	creds    secret.Source
	native   func(ctx context.Context) (llm.Model, harness.Plan, string, error)
	driven   func(ctx context.Context, name, model, workdir string) (*externAgent, error)
}

// boardArm is one arm ready to run, or skipped with the reason in its status.
type boardArm struct {
	status skillab.ArmStatus
	run    func(ctx context.Context, workdir, objective string) (boardAttempt, error)
}

// boardAttempt is what one arm's run left for the board to grade and record.
type boardAttempt struct {
	cost        skillab.Cost
	firstUseful int
	reportedUSD string
	ended       skillab.Ending
	runErr      error
	report      string
	transcript  string
	evidence    map[string]string
	// unevaluated says why the attempt measured nothing about the agent.
	unevaluated string
}

// apiKey reads a provider's key from the credential store, empty when none is stored.
func (e boardEnv) apiKey(ctx context.Context, providerName string) string {
	ref, ok := provider.KeyRef(providerName)
	if !ok || e.creds == nil {
		return ""
	}
	v, err := e.creds.Lookup(ctx, ref)
	if err != nil {
		return ""
	}
	return v.Expose()
}

// prepareArms decides which arms can run here. An arm that cannot is skipped with a
// reason a person can act on, and never fails the board: a CLI that is not installed
// says nothing about the agent behind it.
func prepareArms(ctx context.Context, env boardEnv, f boardFlags, table *usage.Table, shell governed.ShellPolicy) []boardArm {
	var arms []boardArm
	for _, name := range splitList(f.arms) {
		arms = append(arms, prepareArm(ctx, env, name, f, table, shell))
	}
	return arms
}

func prepareArm(ctx context.Context, env boardEnv, name string, f boardFlags, table *usage.Table, shell governed.ShellPolicy) boardArm {
	skip := func(model, reason string) boardArm {
		home, _ := os.UserHomeDir()
		return boardArm{status: skillab.ArmStatus{Name: name, Model: model, Skipped: true, Reason: withoutHome(reason, home)}}
	}
	switch name {
	case armFlynn:
		model, plan, spec, err := env.native(ctx)
		if err != nil {
			return skip("", err.Error())
		}
		price := newPricer(table, spec)
		return boardArm{
			status: skillab.ArmStatus{Name: name, Model: spec, Note: "Flynn's own loop with the bundled skill pack, configured as `skill ab` runs it"},
			run: func(ctx context.Context, workdir, objective string) (boardAttempt, error) {
				return runNativeAttempt(ctx, model, plan, price, shell, workdir, objective)
			},
		}
	case armFlynnClaude, armFlynnCodex:
		cli, model := "claude", f.claudeModel
		if name == armFlynnCodex {
			cli, model = "codex", f.codexModel
		}
		probe, err := os.MkdirTemp("", "flynn-board-probe-")
		if err != nil {
			return skip(model, err.Error())
		}
		defer func() { _ = os.RemoveAll(probe) }()
		ea, err := env.driven(ctx, cli, model, probe)
		if err != nil {
			return skip(model, err.Error())
		}
		ea.close()
		return boardArm{
			status: skillab.ArmStatus{Name: name, Model: model, Note: "tokens are not read back from the driven CLI, so cost is unpriced"},
			run: func(ctx context.Context, workdir, objective string) (boardAttempt, error) {
				return runDrivenAttempt(ctx, env, cli, model, shell, workdir, objective)
			},
		}
	case armClaudeCode:
		bin, err := env.lookPath("claude")
		if err != nil {
			return skip(f.claudeModel, "claude is not installed (not found on PATH)")
		}
		key := env.apiKey(ctx, "anthropic")
		if key == "" {
			return skip(f.claudeModel, "not signed in: bare mode authenticates only with an API key, and Flynn's credential store holds no anthropic key (`flynn auth set anthropic`)")
		}
		return boardArm{
			status: skillab.ArmStatus{Name: name, Model: f.claudeModel, Note: "`claude -p --bare` with stream-json output; turns are its reported num_turns"},
			run: func(ctx context.Context, workdir, objective string) (boardAttempt, error) {
				return runClaudeCodeAttempt(ctx, bin, key, f.claudeModel, f.runUSD, table, workdir, objective)
			},
		}
	case armCodex:
		bin, err := env.lookPath("codex")
		if err != nil {
			return skip(f.codexModel, "codex is not installed (not found on PATH)")
		}
		key := env.apiKey(ctx, "openai")
		if key == "" {
			return skip(f.codexModel, "not signed in: Flynn's credential store holds no openai key (`flynn auth set openai`)")
		}
		return boardArm{
			status: skillab.ArmStatus{Name: name, Model: f.codexModel, Note: "`codex exec --json`; codex reports no model-call count, so turns are completed actions and messages"},
			run: func(ctx context.Context, workdir, objective string) (boardAttempt, error) {
				return runCodexAttempt(ctx, bin, key, f.codexModel, table, workdir, objective)
			},
		}
	}
	return skip("", "unknown arm")
}

// withoutHome writes the home directory in s as ~, because a skip reason goes into a
// board that is published, and the operator's account name has no place there.
func withoutHome(s, home string) string {
	if home == "" {
		return s
	}
	return strings.ReplaceAll(s, home, "~")
}

// runSkillBoard runs the board and writes its JSON and Markdown. Only the arms that can
// run here spend anything; the board records the rest with the reason they were skipped.
func runSkillBoard(args []string, modelSpec, dataDir string, shell governed.ShellPolicy, out io.Writer) error {
	f, err := parseBoardArgs(args, out)
	if err != nil {
		return err
	}
	limit, table, err := abFlags{maxCost: f.maxCost, prices: f.prices}.spend()
	if err != nil {
		return err
	}
	entries, err := boardEntries(f.root, f.skills, f.only)
	if err != nil {
		return err
	}
	ctx := context.Background()
	env := boardEnv{
		lookPath: sandbox.LookPath,
		creds:    credentialSource(dataDir),
		native: func(ctx context.Context) (llm.Model, harness.Plan, string, error) {
			model, plan, choice, err := resolveModelOrOnboard(ctx, modelSpec, currentModelOrigin(), dataDir)
			return model, plan, choice.Spec, err
		},
		driven: resolveExternalAgent,
	}
	// Keys are read before the native model resolves, because resolving it clears the
	// provider variables from this process's environment.
	arms := prepareArms(ctx, env, f, table, shell)
	board := skillab.Board{Generated: clock.System{}.Now(), Prices: filepath.ToSlash(f.prices)}
	if f.resume {
		if board, err = readBoardFile(f.out); err != nil {
			return fmt.Errorf("--resume: %w", err)
		}
		board.Prices, board.Stopped = filepath.ToSlash(f.prices), ""
	}
	return playBoard(ctx, out, &board, entries, arms, f, limit)
}

// playBoard runs the ready arms over entries and writes the board, also when the run
// stopped early, so the attempts already paid for are kept. The JSON is also rewritten
// after every attempt, marked as in progress, so a process that is killed mid-board
// leaves the attempts it finished. A board that arrives with rows is being resumed:
// its evaluated attempts are kept and not paid for again.
func playBoard(ctx context.Context, out io.Writer, board *skillab.Board, entries []skillab.BoardEntry, arms []boardArm, f boardFlags, limit skillab.Limit) error {
	ready := map[string]boardArm{}
	var names []string
	for _, a := range arms {
		if a.status.Skipped {
			_, _ = fmt.Fprintf(out, "skipping %s: %s\n", a.status.Name, a.status.Reason)
			continue
		}
		ready[a.status.Name] = a
		names = append(names, a.status.Name)
	}
	home, _ := os.UserHomeDir()
	kept, done := resumeRows(board.Runs, ready)
	board.Arms = mergeArms(board.Arms, arms, kept, home)
	repeats := max(f.repeats, 1)
	ran := 0
	for _, a := range board.Arms {
		if !a.Skipped {
			ran++
		}
	}
	board.Runs, board.Planned = kept, len(entries)*repeats*ran
	_, _ = fmt.Fprintf(out, "board: %d exercise(s) x %d repeat(s) x %d arm(s): %d attempts, %d already recorded\n", len(entries), repeats, ran, board.Planned, len(kept))
	var runErr error
	if len(names) > 0 {
		partial := *board
		var fresh []skillab.BoardRun
		fresh, board.Stopped, runErr = skillab.RunBoard(ctx, entries, names, repeats, limit,
			func(e skillab.BoardEntry, arm string, repeat int) bool {
				skill, fixture := e.Key()
				return done[attemptKey(skill, fixture, arm, repeat)]
			},
			func(ctx context.Context, e skillab.BoardEntry, arm string, repeat int) (skillab.BoardRun, skillab.Cost, error) {
				row, cost, err := boardTrial(ctx, out, ready[arm], e, repeat, f)
				if err == nil && f.out != "" {
					partial.Runs = append(partial.Runs, row)
					partial.Stopped = "in progress, or killed before it finished"
					if cerr := checkpointBoard(partial, f.out); cerr != nil {
						_, _ = fmt.Fprintf(out, "    (the board could not be checkpointed: %v)\n", cerr)
					}
				}
				return row, cost, err
			})
		board.Runs = append(board.Runs, fresh...)
	}
	if board.Stopped != "" {
		_, _ = fmt.Fprintf(out, "stopped: %s\n", board.Stopped)
	}
	if err := writeBoard(*board, f, out); err != nil {
		return err
	}
	return runErr
}

func attemptKey(skill, fixture, arm string, repeat int) string {
	return fmt.Sprintf("%s/%s %s %d", skill, fixture, arm, repeat)
}

// resumeRows splits the rows a resumed board already has. An unevaluated row of an arm
// that can run now is dropped so the attempt is made again; the same row of an arm that
// is not running stays, because it is the record of why that arm has no figure.
func resumeRows(rows []skillab.BoardRun, ready map[string]boardArm) (kept []skillab.BoardRun, done map[string]bool) {
	done = map[string]bool{}
	for _, r := range rows {
		if _, again := ready[r.Arm]; again && r.Unevaluated != "" {
			continue
		}
		kept = append(kept, r)
		done[attemptKey(r.Skill, r.Fixture, r.Arm, r.Repeat)] = true
	}
	return kept, done
}

// mergeArms updates old's statuses with this run's. An arm with rows keeps its old
// status when it cannot run now, so a missing key does not relabel attempts it made.
func mergeArms(old []skillab.ArmStatus, now []boardArm, rows []skillab.BoardRun, home string) []skillab.ArmStatus {
	merged := append([]skillab.ArmStatus(nil), old...)
	for i := range merged {
		merged[i].Reason = withoutHome(merged[i].Reason, home)
	}
	for _, a := range now {
		i := slices.IndexFunc(merged, func(s skillab.ArmStatus) bool { return s.Name == a.status.Name })
		switch {
		case i < 0:
			merged = append(merged, a.status)
		case !a.status.Skipped || !slices.ContainsFunc(rows, func(r skillab.BoardRun) bool { return r.Arm == a.status.Name }):
			merged[i] = a.status
		}
	}
	return merged
}

func readBoardFile(path string) (skillab.Board, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return skillab.Board{}, err
	}
	defer func() { _ = file.Close() }()
	return skillab.ReadBoard(file)
}

func checkpointBoard(board skillab.Board, path string) error {
	var js bytes.Buffer
	if err := board.WriteJSON(&js); err != nil {
		return err
	}
	return fsatomic.WriteFile(path, js.Bytes(), 0o644)
}

func writeBoard(board skillab.Board, f boardFlags, out io.Writer) error {
	var md bytes.Buffer
	if err := board.Markdown(&md); err != nil {
		return err
	}
	if f.out != "" {
		if err := checkpointBoard(board, f.out); err != nil {
			return err
		}
	}
	if f.markdown != "" {
		if err := fsatomic.WriteFile(f.markdown, md.Bytes(), 0o644); err != nil {
			return err
		}
	}
	_, err := io.Copy(out, &md)
	return err
}

// boardTrial runs one attempt in a fresh copy of the exercise's fixture and grades it
// with the exercise's verifier alone. The run's own account of success is never read:
// a run that stopped short, timed out or claimed a fix it did not make is judged by
// what it left in the directory.
func boardTrial(ctx context.Context, out io.Writer, arm boardArm, e skillab.BoardEntry, repeat int, f boardFlags) (skillab.BoardRun, skillab.Cost, error) {
	skill, fixture := e.Key()
	row := skillab.BoardRun{Skill: skill, Fixture: fixture, Arm: arm.status.Name, Repeat: repeat}
	_, _ = fmt.Fprintf(out, "  [%s, %s/%s, repeat %d]\n", row.Arm, skill, fixture, repeat)
	workdir, err := os.MkdirTemp("", "flynn-board-work-")
	if err != nil {
		return row, skillab.Cost{}, err
	}
	defer func() { _ = os.RemoveAll(workdir) }()
	if err := e.Set.Materialise(e.Exercise, workdir); err != nil {
		return row, skillab.Cost{}, err
	}

	actx, cancel := context.WithTimeout(ctx, f.timeout)
	clk := clock.System{}
	start := clk.Now()
	a, err := arm.run(actx, workdir, e.Exercise.Objective)
	wall := clk.Now().Sub(start)
	timedOut := errors.Is(actx.Err(), context.DeadlineExceeded)
	cancel()
	if ctx.Err() != nil {
		return row, a.cost, ctx.Err()
	}
	if err != nil {
		return row, a.cost, err
	}
	if timedOut {
		a.ended, a.runErr = skillab.Aborted, fmt.Errorf("timed out after %s", f.timeout)
	}
	a.cost.Wall = wall

	var said strings.Builder
	if err := e.Set.Stage(e.Exercise, workdir, a.report, a.transcript); err != nil {
		fmt.Fprintf(&said, "== the harness files could not be placed, so the attempt failed: %v\n", err)
	} else {
		var verifyOut string
		row.Solved, verifyOut = verifyWithOutput(ctx, workdir, e.Exercise.Verify)
		fmt.Fprintf(&said, "== verify (passed: %t)\n%s\n", row.Solved, verifyOut)
		if a.unevaluated == "" && !verifierStarted(verifyOut) {
			a.unevaluated = "the verifier could not start: " + clipLine(verifyOut, 200)
		}
	}
	// A pass counts for nothing here either: an exercise whose answer is to change
	// nothing passes for an agent that was never served.
	home, _ := os.UserHomeDir()
	if a.unevaluated != "" {
		row.Solved, row.Unevaluated = false, clipLine(withoutHome(a.unevaluated, home), 300)
	}
	row.Ended, row.Tokens, row.Turns, row.FirstUseful = a.ended, a.cost.Tokens, a.cost.Turns, a.firstUseful
	row.USD, row.Priced, row.ReportedUSD = a.cost.Money, a.cost.Known(), a.reportedUSD
	// A run that was refused before any model call spent nothing, which is known.
	row.Priced = row.Priced || (a.unevaluated != "" && a.cost == skillab.Cost{})
	row.WallSeconds = wall.Seconds()
	if a.runErr != nil {
		row.Error = clipLine(withoutHome(a.runErr.Error(), home), 300)
		fmt.Fprintf(&said, "== the run stopped: %s: %v\n", a.ended, a.runErr)
	}
	if row.Unevaluated != "" {
		_, _ = fmt.Fprintf(out, "    could not evaluate: %s (%s)\n", row.Unevaluated, a.cost)
	} else {
		_, _ = fmt.Fprintf(out, "    solved: %t, %s\n", row.Solved, a.cost)
	}
	if f.keep != "" {
		files := map[string]string{"report.txt": a.report, "transcript.txt": a.transcript, "checks.txt": said.String()}
		for k, v := range a.evidence {
			files[k] = v
		}
		if f.resume {
			// An attempt made again replaces its row, so it replaces its evidence too.
			_ = os.RemoveAll(filepath.Join(f.keep, skill, fixture, fmt.Sprintf("%s-%d", row.Arm, repeat)))
		}
		if dir, err := keepAttempt(filepath.Join(f.keep, skill), fixture, row.Arm, repeat, files); err != nil {
			_, _ = fmt.Fprintf(out, "    (the attempt's evidence could not be kept: %v)\n", err)
		} else {
			_, _ = fmt.Fprintf(out, "    kept: %s\n", dir)
		}
	}
	return row, a.cost, nil
}

// runNativeAttempt is Flynn's own loop on one exercise, set up as `skill ab` sets up a
// trial: a fresh store holding the bundled pack, learning off, and the operator's shell
// policy, without which Windows refuses every command the agent runs.
func runNativeAttempt(ctx context.Context, model llm.Model, plan harness.Plan, price *pricer, shell governed.ShellPolicy, workdir, objective string) (boardAttempt, error) {
	dataDir, err := os.MkdirTemp("", "flynn-board-data-")
	if err != nil {
		return boardAttempt{}, err
	}
	defer func() { _ = os.RemoveAll(dataDir) }()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return boardAttempt{}, err
	}
	defer func() { _ = store.Close() }()
	metered := newMeteredModel(model, price, clock.System{})
	report, rerr := runLearningMission(ctx, io.Discard, metered, plan, nil, workdir, objective, "", store, nil, false, nil, withShellPolicy(shell))
	cost, transcript, last := metered.snapshot()
	if report == "" {
		report = last
	}
	a := boardAttempt{
		cost: cost, firstUseful: metered.firstUseful(), report: report, transcript: transcript,
		runErr: rerr, evidence: map[string]string{"system.txt": metered.firstSystem()},
	}
	if rerr != nil {
		a.ended = endingOf(rerr)
		a.unevaluated = apiRefusal(rerr.Error())
	}
	return a, nil
}

// runDrivenAttempt is Flynn driving an external agent CLI on one exercise. The driver
// gives back no per-call usage, so the attempt is recorded as unpriced rather than free.
func runDrivenAttempt(ctx context.Context, env boardEnv, cli, model string, shell governed.ShellPolicy, workdir, objective string) (boardAttempt, error) {
	unpriced := skillab.Cost{Unpriced: 1}
	ea, err := env.driven(ctx, cli, model, workdir)
	if err != nil {
		return boardAttempt{ended: skillab.Aborted, runErr: err, unevaluated: "the driven CLI could not start: " + err.Error()}, nil
	}
	defer ea.close()
	dataDir, err := os.MkdirTemp("", "flynn-board-data-")
	if err != nil {
		return boardAttempt{}, err
	}
	defer func() { _ = os.RemoveAll(dataDir) }()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return boardAttempt{}, err
	}
	defer func() { _ = store.Close() }()
	report, rerr := runLearningMission(ctx, io.Discard, nil, harness.Plan{}, nil, workdir, objective, "", store, nil, false, nil,
		withShellPolicy(shell), withExternalAgent(ea))
	a := boardAttempt{cost: unpriced, report: report, runErr: rerr}
	if rerr != nil {
		a.ended = endingOf(rerr)
		a.unevaluated = providerRefusal(rerr.Error())
	}
	return a, nil
}

// cliEnv is the environment a bare CLI arm runs with: this process's, minus every
// variable that would point the CLI at the developer's own account or config, plus set.
// Without the removal a run could be billed to, or configured by, someone else's login.
func cliEnv(base []string, set map[string]string) []string {
	drop := map[string]bool{
		"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true, "CLAUDE_CONFIG_DIR": true, "CLAUDE_CODE_OAUTH_TOKEN": true,
		"OPENAI_API_KEY": true, "CODEX_API_KEY": true, "CODEX_HOME": true,
	}
	var env []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[strings.ToUpper(k)] {
			env = append(env, kv)
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+set[k])
	}
	return env
}

// runAgentCLI runs bin in dir with the prompt on stdin, so an objective is never parsed as a
// flag or split by a Windows command shell.
func runAgentCLI(ctx context.Context, bin string, args, env []string, dir, prompt string) (stdout, stderr []byte, err error) {
	cmd := procexec.CommandContext(ctx, bin, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader(prompt)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.Bytes(), e.Bytes(), err
}

// claudeCodeArgs runs Claude Code headless and bare: no hooks, plugins, CLAUDE.md,
// memory or keychain, so the run sees the exercise and nothing of the developer's setup.
func claudeCodeArgs(model, maxUSD string) []string {
	args := []string{
		"-p", "--bare", "--output-format", "stream-json", "--verbose",
		"--dangerously-skip-permissions", "--no-session-persistence", "--strict-mcp-config",
		"--model", model,
	}
	if maxUSD != "" {
		args = append(args, "--max-budget-usd", maxUSD)
	}
	return args
}

func runClaudeCodeAttempt(ctx context.Context, bin, key, model, maxUSD string, table *usage.Table, workdir, objective string) (boardAttempt, error) {
	home, err := os.MkdirTemp("", "flynn-board-claude-")
	if err != nil {
		return boardAttempt{}, err
	}
	defer func() { _ = os.RemoveAll(home) }()
	env := cliEnv(os.Environ(), map[string]string{"ANTHROPIC_API_KEY": key, "CLAUDE_CONFIG_DIR": home})
	stdout, stderr, runErr := runAgentCLI(ctx, bin, claudeCodeArgs(model, maxUSD), env, workdir, objective)
	parsed := parseClaudeCodeStream(stdout, model)
	a := boardAttempt{
		cost:        priceClaudeCode(parsed, table, clock.System{}.Now()),
		firstUseful: parsed.firstUseful,
		report:      parsed.result,
		transcript:  clip(string(stdout)),
		evidence:    map[string]string{"stream.jsonl": string(stdout), "stderr.txt": string(stderr)},
	}
	if parsed.hasCost {
		a.reportedUSD = fmt.Sprintf("%.6f", parsed.reportedUSD)
	}
	switch {
	case !parsed.finished:
		a.ended, a.runErr = skillab.Aborted, fmt.Errorf("claude printed no result: %s: %s", fmt.Sprint(runErr), clipLine(string(stderr), 200))
		a.unevaluated = providerRefusal(string(stderr))
		if parsed.turns == 0 {
			// No model response, so nothing was spent and nothing was tried.
			a.cost, a.unevaluated = skillab.Cost{}, a.runErr.Error()
		}
	case parsed.isError || parsed.subtype != "success":
		a.ended, a.runErr = skillab.Aborted, fmt.Errorf("claude ended with %s: %s", parsed.subtype, clipLine(parsed.result, 200))
		if parsed.isError {
			a.unevaluated = providerRefusal(parsed.result)
		}
		if strings.Contains(parsed.subtype, "max_turns") || strings.Contains(parsed.subtype, "budget") {
			a.ended = skillab.BudgetExhausted
		}
	}
	return a, nil
}

// priceClaudeCode prices each model Claude Code reported at its own row. One model the
// table cannot price makes the whole attempt unpriced, so no partial sum reads as the
// whole cost.
func priceClaudeCode(run claudeCodeRun, table *usage.Table, at time.Time) skillab.Cost {
	c := skillab.Cost{Turns: max(run.turns, 1)}
	names := make([]string, 0, len(run.perModel))
	for name := range run.perModel {
		names = append(names, name)
	}
	sort.Strings(names)
	priced := len(names) > 0
	for _, name := range names {
		tk := run.perModel[name]
		c.Tokens = c.Tokens.Add(tk)
		p := table.Price(usage.Record{Provider: "anthropic", Model: name, At: at, Tokens: tk})
		if !p.Known {
			priced = false
			continue
		}
		c.Money, c.Currency = c.Money+p.Amount, p.Currency
	}
	if !priced {
		c.Unpriced = c.Turns
	}
	return c
}

func runCodexAttempt(ctx context.Context, bin, key, model string, table *usage.Table, workdir, objective string) (boardAttempt, error) {
	home, err := os.MkdirTemp("", "flynn-board-codex-")
	if err != nil {
		return boardAttempt{}, err
	}
	defer func() { _ = os.RemoveAll(home) }()
	env := cliEnv(os.Environ(), map[string]string{"CODEX_HOME": home, "CODEX_API_KEY": key, "OPENAI_API_KEY": key})
	// The fresh home holds no login, so the key is stored in it first; codex exec reads
	// CODEX_API_KEY too, and the login makes the arm independent of which one a version honours.
	if _, lerr, err := runAgentCLI(ctx, bin, []string{"login", "--with-api-key"}, env, workdir, key); err != nil {
		failed := fmt.Errorf("codex login: %w: %s", err, clipLine(string(lerr), 200))
		return boardAttempt{ended: skillab.Aborted, runErr: failed, unevaluated: failed.Error()}, nil
	}
	args := []string{"exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "-C", workdir, "-m", model, "-"}
	stdout, stderr, runErr := runAgentCLI(ctx, bin, args, env, workdir, objective)
	parsed := parseCodexStream(stdout)
	c := skillab.Cost{Turns: max(parsed.steps, 1), Tokens: parsed.tokens}
	p := table.Price(usage.Record{Provider: "openai", Model: model, At: clock.System{}.Now(), Tokens: parsed.tokens})
	if p.Known {
		c.Money, c.Currency = p.Amount, p.Currency
	} else {
		c.Unpriced = c.Turns
	}
	a := boardAttempt{
		cost: c, firstUseful: parsed.firstUseful, report: parsed.lastMessage, transcript: clip(string(stdout)),
		evidence: map[string]string{"stream.jsonl": string(stdout), "stderr.txt": string(stderr)},
	}
	switch {
	case parsed.failure != "":
		a.ended, a.runErr = skillab.Aborted, errors.New(parsed.failure)
	case !parsed.finished:
		a.ended, a.runErr = skillab.Aborted, fmt.Errorf("codex finished no turn: %s: %s", fmt.Sprint(runErr), clipLine(string(stderr), 200))
	}
	if !parsed.finished {
		a.unevaluated = providerRefusal(parsed.failure + "\n" + string(stderr))
		if a.unevaluated == "" && parsed.steps == 0 && parsed.tokens.Input.N == 0 {
			a.cost, a.unevaluated = skillab.Cost{}, a.runErr.Error()
		}
	}
	return a, nil
}
