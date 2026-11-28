// Command flynn is the standalone Flynn agent binary.
//
// Build:  go build -o flynn ./cmd/flynn
// Run a goal:  flynn goal "audit the repo for TODOs and write a summary to NOTES.md"
// --model provider:model picks the model; without it the saved default (flynn models
// use) applies, and with neither a terminal is asked and a script is refused. The
// provider's API key comes from its environment variable. Learned skills and memory
// persist under --data-dir; --no-learn skips capturing them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ionalpha/flynn/governed"

	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/diag"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/internal/selection"
	"github.com/ionalpha/flynn/internal/selfupdate"
	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/internal/version"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/observe"
	"github.com/ionalpha/flynn/procs"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/secret"
)

// staleSandboxProfileAge is how old a sandbox profile must be before the startup sweep
// collects it. A profile belongs to a live sandbox until it closes, and collecting one
// under a running command breaks the command, so the cutoff is far beyond any confined
// command's run time. Waiting a day to reclaim a directory costs nothing.
const staleSandboxProfileAge = 24 * time.Hour

// sweepStaleSandboxProfiles collects sandbox profiles that crashed or killed runs left
// behind. On Windows each survivor keeps a registered container identity whose access
// entries stay live. The sweep runs in the background and reports nothing: a short
// command may exit before it finishes, the next run continues it, and no sandbox
// depends on it.
func sweepStaleSandboxProfiles() {
	cutoff := clock.System{}.Now().Add(-staleSandboxProfileAge)
	go func() { _, _ = sandbox.CleanStaleProfiles(cutoff) }()
}

// sweepSupersededBinaries removes the binary an earlier `flynn upgrade` moved aside,
// because Windows cannot delete a running executable. The displaced file is inert, so
// a failure is ignored.
func sweepSupersededBinaries() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	go func() { _ = selfupdate.SweepSuperseded(exe) }()
}

// profileConfig assembles the diagnostics config from the flags and the environment,
// or returns a usage message for a combination that cannot be honoured. Flags win over
// the environment, but neither can disable a watchdog the other turned on.
func profileConfig(dir string, contention, leakWatch, leakRepeat bool) (diag.Config, string) {
	cfg := diag.FromEnv(diag.Config{
		Dir:        dir,
		Contention: contention,
		Args:       os.Args,
		Clock:      clock.System{},
		// The sandbox is the only spawner in this binary and records every child in the
		// process registry. Reading it is an atomic load, so a bundle left on for days costs
		// nothing per sample.
		Children: procs.Live,
	})
	if leakWatch && cfg.Leak == nil {
		cfg.Leak = &diag.LeakConfig{}
	}
	if cfg.Leak == nil {
		return cfg, ""
	}

	// The watchdog reads and dumps into the bundle, so without one it would watch
	// nothing for the whole soak.
	if cfg.Dir == "" {
		return diag.Config{}, "--leak-watch needs a bundle to watch; add --profile <dir> or set FLYNN_PROFILE"
	}
	if leakRepeat {
		cfg.Leak.Repeat = true
	}
	// A leak goes to stderr, where an unattended operator's log collector is already
	// looking, and never to stdout, which carries the command's own output.
	cfg.Leak.Logger = observe.NewWarnLogger(os.Stderr)
	return cfg, ""
}

// main only turns run's exit code into a process exit. os.Exit runs no defers, so
// every path returns through run, where a --profile bundle's deferred Stop writes it.
func main() {
	os.Exit(run(flag.NewFlagSet("flynn", flag.ContinueOnError), os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches the command line and returns the exit code: 0 on success, 1 on a
// command error, 2 on a usage error, and exitChangesRequested for a review that asked
// for changes. The flag set, arguments and streams are parameters so the dispatch is
// testable in-process.
func run(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) int {
	var (
		model       = fs.String("model", "anthropic:claude-opus-5-5", "model as `provider:model`; if unset, the saved one, else you are asked")
		dataDir     = fs.String("data-dir", defaultDataDir(), "`dir`ectory for the durable state database")
		noLearn     = fs.Bool("no-learn", false, "do not capture skills/memory from this run")
		noBundled   = fs.Bool("no-bundled-skills", false, "run without the bundled skills, removing them from the store")
		projFiles   = fs.String("project-files", "", "load AGENTS.md and CLAUDE.md: `mode` is ask (default), trust or skip")
		verbose     = fs.Bool("v", false, "show tool arguments, outputs and per-turn detail")
		verboseLong = fs.Bool("verbose", false, "alias for -v")
		plain       = fs.Bool("plain", false, "use the line-based interactive session, not the full-screen one")
		verify      = fs.String("verify", "", "a `command` run after the agent stops that checks the goal succeeded")
		reqProof    = fs.Bool("require-proof", false, "refuse success over a plan item the record shows no passing check for")
		goalSpec    = fs.String("goal-spec", "", "read the run's terms from a JSON `file` (\"-\" is stdin)")
		reqApproval = &stringList{}
		outside     = &stringList{}
		allowed     = &stringList{}
		fanout      = fs.Bool("fanout", false, "let the goal delegate sub-tasks to concurrent child agents")
		maxCost     = fs.Float64("max-cost", 0, "cap the run's model and tool spend at this `amount`; 0 is unlimited")
		maxTokens   = fs.Int64("max-tokens", 0, "cap the run's metered tokens at `n`; 0 is unlimited")
		shellFlags  = registerShellFlags(fs)
		selFlags    = registerSelectionFlags(fs)
		showVersion = fs.Bool("version", false, "print the version and exit")
		profileDir  = fs.String("profile", "", "write a runtime profile bundle into this `dir` for the command's life")
		profileCont = fs.Bool("profile-contention", false, "add block and mutex profiles to the --profile bundle")
		leakWatch   = fs.Bool("leak-watch", false, "dump profiles into the --profile bundle when a resource keeps growing")
		leakRepeat  = fs.Bool("leak-watch-repeat", false, "let --leak-watch dump the same counter more than once")
	)
	fs.Var(reqApproval, "require-approval", "pause this `action` until a person authorizes it (repeatable)")
	fs.Var(outside, "irreversible", "mark this `action` as not undoable, held until --allow (repeatable)")
	fs.Var(allowed, "allow", "permit this --irreversible `action` in advance (repeatable)")
	// Parse errors and -h are reported here, in the same form as every other usage error,
	// rather than by the flag package's own printer.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeTopHelp(stdout, fs)
			return 0
		}
		return reportError(stderr, usageErrf("flags", "%v", err))
	}
	if topic, ok := helpTarget(fs.Args()); ok {
		return showHelp(stdout, stderr, fs, topic)
	}
	rest, err := goalArgs(fs, fs.Args())
	if errors.Is(err, flag.ErrHelp) {
		return showHelp(stdout, stderr, fs, []string{"goal"})
	}
	if err != nil {
		return reportError(stderr, err)
	}
	vrb := *verbose || *verboseLong

	// Set before any store opens, because opening one reconciles the bundled skills.
	bundledSkillsDisabled = *noBundled

	// A misspelt trust setting is refused, because the default would run a session in a
	// mode its operator did not choose.
	projectFilesFlag = *projFiles
	if _, err := projectFilesMode(); err != nil {
		return reportError(stderr, usageErrf("flags", "%v", err))
	}

	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version.String())
		return 0
	}

	// Checked against the stored skills and the tools that exist once a run opens its store;
	// a malformed list or selection file is refused here, before anything starts.
	sel, err := selFlags.resolve(fs, *dataDir)
	if errors.Is(err, selection.ErrUnreadable) {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err != nil {
		return reportError(stderr, usageErrf("flags", "%v", err))
	}
	selectionConfig = sel

	// The bundle spans the whole command and closes on the single exit path below. With
	// no --profile and no FLYNN_PROFILE, Start returns a nil bundle whose Stop is a no-op.
	profileCfg, usage := profileConfig(*profileDir, *profileCont, *leakWatch, *leakRepeat)
	if usage != "" {
		return reportError(stderr, usageErrf("flags", "%s", usage))
	}

	bundle, err := diag.Start(profileCfg)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer func() {
		// The command already ran, so a bundle that failed to seal is only a warning.
		if err := bundle.Stop(); err != nil {
			warn(stderr, "profile bundle: %v", err)
		}
	}()

	sweepStaleSandboxProfiles()
	sweepSupersededBinaries()

	// Print owed notices (a security advisory for this version above all) before the
	// command runs, and fetch a newer feed in the background. Neither can fail the command.
	startupNotices(context.Background(), *dataDir)

	// Catalog reads include this data directory's overlay, so a `flynn models add` entry
	// is selectable by --model.
	prevCatalogDir := catalogDataDir
	setCatalogDataDir(*dataDir)
	defer setCatalogDataDir(prevCatalogDir)

	// An explicit --model wins, then the recorded default. The flag's own default is only
	// a suggestion: with neither of the others, the resolver asks or refuses.
	modelSpecExplicit = flagPassed(fs, "model")
	modelSpec := effectiveModelSpec(*model, modelSpecExplicit, *dataDir)
	_, saved := readActiveModel(*dataDir)
	modelSpecSaved = saved && !modelSpecExplicit

	// No subcommand is named "", so the branches below compare against cmd directly.
	cmd := ""
	if len(rest) >= 1 {
		cmd = rest[0]
	}

	return routeCommand(cmd, rest, invocation{
		stdout:       stdout,
		stderr:       stderr,
		modelSpec:    modelSpec,
		dataDir:      *dataDir,
		verbose:      vrb,
		learn:        !*noLearn,
		plain:        *plain,
		verify:       *verify,
		fanout:       *fanout,
		requireProof: *reqProof,
		goalSpec:     *goalSpec,
		reqApproval:  reqApproval.values,
		outside:      outside.values,
		allowed:      allowed.values,
		maxCost:      *maxCost,
		maxTokens:    *maxTokens,
		shell:        shellFlags.policy(*dataDir),
		selection:    selFlags.named(fs),
		flags:        fs,
	})
}

// goalArgs parses the run flags written after `flynn goal`, where the usage text puts
// them, and returns the command line with only the objective after "goal". Other
// command lines come back unchanged.
//
// The flag package stops at the first non-flag, so without this `flynn goal
// --goal-spec terms.json` loads no spec and runs with the flag text as its objective,
// dropping --require-approval, --irreversible and every other control the operator
// typed. A flag after the free-text objective is refused as a usage error that names
// it. "--" ends the flags, so an objective can start with a dash.
func goalArgs(fs *flag.FlagSet, rest []string) ([]string, error) {
	if len(rest) == 0 || rest[0] != "goal" {
		return rest, nil
	}
	literal := slices.Contains(rest[1:], "--")
	if err := fs.Parse(rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, usageErrf("goal", "%v", err)
	}
	objective := fs.Args()
	if !literal {
		for _, arg := range objective {
			if name, ok := flagName(arg); ok && fs.Lookup(name) != nil {
				return nil, usageErrf("goal", "%s is a flag, and it came after the objective, where it would have been read as part of it; put it before: flynn goal %s ... \"<objective>\"", arg, arg)
			}
		}
	}
	return append([]string{"goal"}, objective...), nil
}

// flagName returns the flag name an argument spells ("-v", "--goal-spec",
// "--max-cost=2"), and false for anything that is not a flag.
func flagName(arg string) (string, bool) {
	if len(arg) < 2 || arg[0] != '-' {
		return "", false
	}
	name := strings.TrimLeft(arg, "-")
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	return name, name != ""
}

// invocation is the command-line state run parses once and hands to routeCommand, so
// the router does no parsing.
type invocation struct {
	stdout, stderr io.Writer
	modelSpec      string
	dataDir        string
	verbose        bool
	learn          bool
	plain          bool
	verify         string
	fanout         bool
	requireProof   bool
	// goalSpec is the spec file path, or "". Loading it can fail, and the router reports
	// that as a usage error.
	goalSpec    string
	reqApproval []string
	outside     []string
	allowed     []string
	maxCost     float64
	maxTokens   int64
	// shell is built from the flags once, so every entry point that drives a shell passes
	// the same policy and none restates the defaults.
	shell governed.ShellPolicy
	// selection is the skill and tool selection flags the command line set, so the router
	// can refuse them on a command that would ignore them.
	selection []string
	// flags is the parsed root flag set, which the top-level page is rendered from.
	flags *flag.FlagSet
}

// exit maps an outcome to its exit code with [reportError].
func (inv invocation) exit(err error) int { return reportError(inv.stderr, err) }

// routeCommand routes a parsed command line and returns the exit code under the
// contract documented on run.
func routeCommand(cmd string, rest []string, inv invocation) int {
	// A goal spec on any other command is refused: ignoring it would let an operator
	// believe terms were in force that never were.
	if inv.goalSpec != "" && cmd != "goal" {
		what := "flynn " + cmd
		if cmd == "" {
			what = "an interactive session"
		}
		return inv.exit(usageErrf("goal", "--goal-spec states the terms of one run, so it applies to `flynn goal` and not to %s", what))
	}
	c := findCLI(cmd)
	name := cmd
	if c != nil {
		name = c.name
	}
	if len(inv.selection) > 0 && !selectionCommands[name] {
		return inv.exit(usageErrf("flags", "%s chooses what a run is offered, so it applies to an interactive session, `flynn goal`, `flynn resume` and `flynn watch`, and not to flynn %s", inv.selection[0], cmd))
	}
	switch {
	case c != nil && c.invoke != nil:
		return c.invoke(inv, rest)
	case c != nil && c.local != nil:
		return inv.exit(c.local(rest[1:], inv.dataDir))
	case c != nil:
		// help has neither: run answered it before routing.
		return showHelp(inv.stdout, inv.stderr, inv.flags, rest[1:])
	case cmd != "":
		return inv.exit(unknownCommand("", cmd))
	}

	// With no command a terminal gets an interactive session; redirected stdin has no one
	// to prompt, so it gets the top-level page and a usage exit.
	if stdinIsTerminal() {
		inv.noteShellPosture()
		return inv.exit(runInteractive(inv.modelSpec, inv.dataDir, inv.learn, inv.verbose, inv.plain, inv.reqApproval, inv.shell))
	}
	writeTopHelp(inv.stderr, inv.flags)
	return 2
}

// routeGoal runs `flynn goal`. It loads and validates the spec before any store or
// credential, so a bad spec is a usage error the operator sees before the run starts
// without their terms.
func routeGoal(inv invocation, rest []string) int {
	var spec goalSpecFile
	if inv.goalSpec != "" {
		loaded, err := loadGoalSpecFile(inv.goalSpec)
		if err != nil {
			return inv.exit(asUsage("goal", err))
		}
		spec = loaded
	}
	// An objective stated nowhere, or twice differently, is a command-line error.
	objective, err := mergeGoalSpec(spec, strings.Join(rest[1:], " "))
	if err != nil {
		return inv.exit(asUsage("goal", err))
	}
	inv.noteShellPosture()
	return inv.exit(runGoal(inv.modelSpec, objective, inv.verify, inv.dataDir, spec, inv.learn, inv.verbose, inv.fanout, inv.requireProof, inv.reqApproval, inv.outside, inv.allowed, inv.maxCost, inv.maxTokens, inv.shell))
}

func routeInspect(inv invocation, rest []string) int {
	if len(rest) < 2 {
		return inv.exit(usageErr("inspect"))
	}
	return inv.exit(inspectRun(inv.stdout, inv.dataDir, rest[1], inv.verbose))
}

func routeResume(inv invocation, rest []string) int {
	if len(rest) < 2 {
		return inv.exit(usageErr("resume"))
	}
	inv.noteShellPosture()
	return inv.exit(resumeRun(inv.modelSpec, rest[1], inv.dataDir, inv.verbose, inv.shell))
}

// routeMemory runs `flynn memory`. Consolidation distils through a model, so it needs
// the model spec and is an invoke route rather than a local one.
func routeMemory(inv invocation, rest []string) int {
	return inv.exit(dispatchMemory(rest[1:], inv.modelSpec, inv.dataDir, inv.stdout))
}

func routeSkill(inv invocation, rest []string) int {
	if len(rest) > 1 && rest[1] == "ab" {
		inv.noteShellPosture()
	}
	return inv.exit(dispatchSkill(rest[1:], inv.modelSpec, inv.dataDir, inv.shell, inv.stdout))
}

// routeReview runs `flynn review`. A review that requests changes exits with
// exitChangesRequested.
func routeReview(inv invocation, rest []string) int {
	switch err := runReview(rest[1:], inv.modelSpec, inv.dataDir, inv.verbose); {
	case errors.Is(err, errChangesRequested):
		return exitChangesRequested
	default:
		return inv.exit(err)
	}
}

// effectiveModelSpec resolves the model: an explicit --model, else the default
// recorded under dataDir (by onboarding, /model or `flynn models use`), else the
// flag's built-in default.
func effectiveModelSpec(flagValue string, explicit bool, dataDir string) string {
	if explicit {
		return flagValue
	}
	if saved, ok := readActiveModel(dataDir); ok {
		return saved
	}
	return flagValue
}

// modelSpecExplicit records whether --model was named. Only a named spec says which
// provider may see the work, so with it set the resolver refuses a missing credential
// instead of falling back.
var modelSpecExplicit bool

// bundledSkillsDisabled records --no-bundled-skills. It is a global because every
// command that opens the store reconciles the pack there and has to agree.
var bundledSkillsDisabled bool

// flagPassed reports whether the named flag was set, telling a default from an
// explicit value.
func flagPassed(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// defaultDataDir is the per-user state directory, so learning carries across
// projects, or a local directory when the user config dir is unavailable.
func defaultDataDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, dataDirName())
	}
	return "." + dataDirName()
}

// dataDirName is "flynn" for a release build and "flynn-dev" for an unstamped build,
// so a branch's schema work never migrates or is blocked by a real database.
func dataDirName() string {
	if version.IsDev() {
		return "flynn-dev"
	}
	return "flynn"
}

// runGoal drives one objective to completion in the current directory, recalling
// past learning into the prompt and, unless disabled, distilling the result back.
// Ctrl-C cancels the run.
func runGoal(modelSpec, objective, verify, dataDir string, spec goalSpecFile, learnEnabled, verbose, fanout, requireProof bool, reqApproval, outside, allowed []string, maxCost float64, maxTokens int64, shell governed.ShellPolicy) error {
	if err := checkAllowedAreMarked(outside, allowed); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	// A `--model codex:<model>` spec selects an external agent CLI whose harness drives
	// the loop; anything else resolves a hosted or local model.
	var (
		model    llm.Model
		plan     harness.Plan
		extAgent *externAgent
		choice   modelChoice
	)
	if name, cliModel, ok := externalAgentSpec(modelSpec); ok {
		// An external harness owns its loop, so fan-out with one is refused.
		if fanout {
			return fmt.Errorf("--fanout is not supported with the %s external agent backend: it drives its own loop", name)
		}
		extAgent, err = resolveExternalAgent(ctx, name, cliModel, cwd)
		if err != nil {
			return err
		}
		// The harness home holds the CLI's own conversation, so it lives as long as the run.
		defer extAgent.close()
	} else {
		model, plan, choice, err = resolveModelOrOnboard(ctx, modelSpec, currentModelOrigin(), dataDir)
		if err != nil {
			return err
		}
	}

	// The signer is best effort: without an identity the run proceeds unsigned. It loads
	// before the store so resource snapshots are sealed under the same key, verified and
	// written as the stream grows.
	signer, serr := runSigner(ctx, dataDir)
	if serr != nil {
		signer = nil
	}
	store, err := openDataStore(ctx, dataDir, snapshotOptions(signer)...)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// Checked against what is stored, so a misspelt slug or tool name stops the run here
	// naming the valid ones.
	selPlan, err := resolveSelection(ctx, selectionConfig, store.Skills(), extAgent != nil, fanout, requireProof)
	if err != nil {
		return err
	}

	// An external agent exposes no model to distill through, so its run is sealed but
	// skips learning.
	var distiller learn.Distiller
	if learnEnabled && extAgent == nil {
		distiller = governed.Distiller(model)
	}

	// Fan-out lets the model delegate sub-goals to concurrent children, each on the model
	// its archetype pins and resolved through the root's credential chain, all folded
	// into this run's sealed record.
	var fc *fanoutConfig
	if fanout {
		fc = &fanoutConfig{resolveModel: childModelResolver(ctx, dataDir)}
	}

	opts := []driveOption{
		withBudget(budgetpkg.Limits{Tokens: maxTokens, Cost: maxCost}),
		withShellPolicy(shell),
		// No operator is at a prompt, so the gate has no prompter and refuses a listed action.
		withApproval(reqApproval, nil),
		// The actions that reach outside the workspace and those this run may take. Only the
		// person who started the run sets them; the run cannot mark an action safe or
		// authorize itself.
		withAllowance(outside, mergeAllowances(spec, allowed)),
		// The operator's terms, with their stop condition if they wrote one.
		withGoalSpec(spec),
		// Nil unless this install names an embedding model, which keeps lexical recall order.
		withMemoryEmbedder(configuredEmbedder(ctx, dataDir, noticeLine(os.Stdout))),
		withSelection(selPlan),
	}
	if extAgent != nil {
		opts = append(opts, withExternalAgent(extAgent))
	} else {
		opts = append(opts, withModelChoice(choice))
		// A native goal writes a visible plan ledger before it builds. An external agent CLI
		// plans in its own loop.
		opts = append(opts, withPlanning())
		// The repository's instruction files, if the operator allows them. An external agent
		// CLI reads its own.
		opts = append(opts, withProjectFiles(projectDecisionFor(cwd, dataDir)))
		// Planning records each item's verdict; this makes an unproven item stop the run.
		if requireProof {
			opts = append(opts, withLedgerProof())
		}
	}

	// The objective and answer render from the run's own events, so the live transcript
	// and a later `flynn inspect` read identically.
	if _, err := runLearningMission(ctx, os.Stdout, model, plan, distiller, cwd, objective, verify, store, signer, verbose, fc, opts...); err != nil {
		return err
	}
	return nil
}

// resolveModel resolves the credential through [credentialSource], so a key stored
// with `flynn auth set` needs no export. The key is then unset from the process
// environment: the model holds it as a secret.Text, and unsetting keeps it out of
// os.Environ(), crash dumps and any child that reads the parent env.
func resolveModel(ctx context.Context, modelSpec, dataDir string) (llm.Model, harness.Plan, error) {
	// A local catalog model is provisioned and served on demand, so selecting it needs
	// no manual install, download or start.
	if isLocalModelID(modelSpec) {
		return resolveLocalModel(ctx, modelSpec, dataDir)
	}
	src, err := modelSource(dataDir)
	if err != nil {
		return nil, harness.Plan{}, err
	}
	model, err := provider.ResolveWith(ctx, modelSpec, src)
	if err != nil {
		return nil, harness.Plan{}, err
	}
	for _, k := range provider.CredentialEnvVars() {
		_ = os.Unsetenv(k)
	}
	// The zero plan adds no scaffolding, keeping a frontier model's full schemas and
	// single-pass convergence.
	return model, harness.Plan{}, nil
}

// credentialSource is the credential lookup order: the OS keychain, the
// passphrase-sealed file, then the environment. Model resolution and provider
// auto-detection share it.
func credentialSource(dataDir string) secret.Source {
	return secret.Chain(vault.New(dataDir, vault.WithPassphrase(terminalPassphrase)), secret.EnvSource{})
}

// resumeRun re-drives run runID's goal from where it stopped, onto the same durable
// stream: the prior conversation replays, then the run continues to a terminal phase.
// Ctrl-C detaches and the run can be resumed again. A resume skips learning, since it
// starts no fresh run to distill.
func resumeRun(modelSpec, runID, dataDir string, verbose bool, shell governed.ShellPolicy) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	model, plan, choice, err := resolveModelOrOnboard(ctx, modelSpec, currentModelOrigin(), dataDir)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	// The same best-effort signer as a goal, so a resumed run ends verifiable.
	signer, serr := runSigner(ctx, dataDir)
	if serr != nil {
		signer = nil
	}
	store, err := openDataStore(ctx, dataDir, snapshotOptions(signer)...)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	reg, err := governed.Registry()
	if err != nil {
		return err
	}
	req, err := driveRequest{
		out:      os.Stdout,
		model:    model,
		plan:     plan,
		workdir:  cwd,
		system:   defaultSystemPrompt,
		rstore:   store.Resources(reg),
		jq:       store.Jobs(),
		log:      store.Log(),
		verbose:  verbose,
		resumeID: runID,
	}.withOptions(withModelChoice(choice)).resuming(ctx, dataDir, shell)
	if err != nil {
		return err
	}
	// A resume rebuilds the run's posture from its record and the selection from this
	// invocation. The tool grant matters here; a resumed run offers no skills, so the
	// skill rules have nothing to filter.
	selPlan, err := resolveSelection(ctx, selectionConfig, store.Skills(), false, req.fanout != nil, req.cfg.proof)
	if err != nil {
		return err
	}
	req = req.withOptions(withSelection(selPlan))
	_, _, _, err = req.execute(ctx)
	if signer != nil {
		sealResumed(ctx, os.Stdout, store, runID, signer, err)
	}
	return err
}

// resuming completes a request that continues r.resumeID with the guards its first
// assembly recorded (see [resumeGuards]), its fan-out, and this invocation's shell
// policy.
func (r driveRequest) resuming(ctx context.Context, dataDir string, shell governed.ShellPolicy) (driveRequest, error) {
	posture, _, err := governed.ReadPosture(ctx, r.log, r.resumeID)
	if err != nil {
		return r, fmt.Errorf("resume: read the run's posture: %w", err)
	}
	guards, fanout, err := resumeGuards(posture)
	if err != nil {
		return r, err
	}
	if fanout {
		r.fanout = &fanoutConfig{resolveModel: childModelResolver(ctx, dataDir)}
	}
	return r.withOptions(append(guards, withShellPolicy(shell))...), nil
}

// resumeGuards turns a run's recorded posture into the options that rebuild it and
// reports whether it was a fan-out. A resume reads only shell flags, so without these
// it would lose the item verifier, spend cap, approval gate and outside-action gate.
// A zero posture, recorded before postures existed, yields none. An external agent's
// run is refused, because resume would continue it on a native model.
func resumeGuards(p governed.Posture) (opts []driveOption, fanout bool, err error) {
	if p.Mode == governed.ModeExternal {
		return nil, false, errors.New("resume: this run was driven by an external agent CLI, and resume can only continue a native run")
	}
	opts = []driveOption{
		withBudget(p.BudgetLimits),
		// Nobody is at a prompt, so the gate refuses a listed action.
		withApproval(p.ApprovalActions, nil),
		// The run's declared allowances are on its goal, which a resume keeps.
		withAllowance(p.AllowanceActions, nil),
	}
	// Fan-out always wires the verifier, so only a planned single run needs it set.
	if p.Verifier && p.Mode != governed.ModeFanout {
		opts = append(opts, withPlanning())
	}
	if p.LedgerProof {
		opts = append(opts, withLedgerProof())
	}
	return opts, p.Mode == governed.ModeFanout, nil
}
