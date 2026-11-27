// Command flynn is the standalone Flynn agent binary.
//
// Build:  go build -o flynn ./cmd/flynn
// Run a goal:  flynn goal "audit the repo for TODOs and write a summary to NOTES.md"
// --model provider:model picks the model (default anthropic:claude-opus-5-5) and the
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

// dataDirCommands are the subcommands whose only state is the data directory.
var dataDirCommands = map[string]func(args []string, dataDir string) error{
	"auth":         runAuth,
	"integrations": runIntegrations,
	"extensions":   runExtensions,
	"deps":         runDeps,
	"playbook":     runPlaybook,
	"deploy":       runDeploy,
	"services":     runServices,
	"models":       dispatchModels,
	"init":         runInit,
	"notices":      runNotices,
	"get":          dispatchGet,
	"mcp":          runMCP,
	"describe":     dispatchDescribe,
	"diff":         dispatchDiff,
	"ps":           dispatchPs,
	"status":       dispatchStatus,
	"spine":        dispatchSpine,
	"steer":        dispatchSteer,
	"kill":         dispatchKill,
	"db":           runDB,
	"version":      runVersion,
	"upgrade":      runUpgrade,
	"sandbox":      runSandbox,
}

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
		return diag.Config{}, "usage: --leak-watch needs a bundle to watch; add --profile <dir> or set FLYNN_PROFILE"
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
func main() { os.Exit(run(flag.CommandLine, os.Args[1:], os.Stdout, os.Stderr)) }

// run dispatches the command line and returns the exit code: 0 on success, 1 on a
// command error, 2 on a usage error, and exitChangesRequested for a review that asked
// for changes. The flag set, arguments and streams are parameters so the dispatch is
// testable in-process.
func run(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) int {
	var (
		model       = fs.String("model", "anthropic:claude-opus-5-5", "model as provider:model")
		dataDir     = fs.String("data-dir", defaultDataDir(), "directory for the durable state database")
		noLearn     = fs.Bool("no-learn", false, "do not capture skills/memory from this run")
		noBundled   = fs.Bool("no-bundled-skills", false, "run without the skills shipped in the binary: the bundled set is removed from the store rather than merely left unseeded, so a run measures the learning loop against a clean baseline. Your own and learned skills are untouched.")
		projFiles   = fs.String("project-files", "", "whether to load the repository's AGENTS.md, AGENTS.override.md and CLAUDE.md into the model's instructions: ask (the default: load files you approved before, otherwise ask, and skip when no one can answer), trust (load them) or skip (never). Loaded files are untrusted input: they are fenced in the prompt, the session is tainted by them, and the decision is recorded on the run. Also set by FLYNN_PROJECT_FILES.")
		verbose     = fs.Bool("v", false, "verbose: show tool arguments, outputs, and per-turn detail")
		verboseLong = fs.Bool("verbose", false, "alias for -v")
		plain       = fs.Bool("plain", false, "interactive session: use the line-based interface, not the full-screen one")
		verify      = fs.String("verify", "", "a command that independently checks the goal succeeded; run after the agent stops, its result grounds the run's success in the verifiable record")
		reqProof    = fs.Bool("require-proof", false, "hold the run to its own plan: the goal will not report success over a ledger item the record cannot show a passing check for. Needs a host that can contain semi-trusted work, since the check is a model-authored command; where it cannot be run the item stays unproven and the run stops saying so.")
		goalSpec    = fs.String("goal-spec", "", `a JSON file stating the terms of the run: what must stay true while it works, each with the command that checks it, plus optionally the objective, the stop condition, and the irreversible actions it may take ("-" reads stdin). A term is audited on every step before the goal is asked whether it is done, and a breach stops the run naming the term, so it is the one thing a run under completion pressure cannot trade away. A term's check is run as semi-trusted work, so it needs a host that can contain it; where it cannot be run the run stops saying so rather than carrying on unaudited. See `+"`flynn help`"+` for the shape.`)
		reqApproval = &stringList{}
		outside     = &stringList{}
		allowed     = &stringList{}
		fanout      = fs.Bool("fanout", false, "let the goal delegate sub-tasks to concurrent child agents (each routed to the model its archetype pins), all folded into one verifiable record")
		maxCost     = fs.Float64("max-cost", 0, "cap the run's total model+tool spend in the provider's currency unit; 0 (default) is unlimited. A fan-out's children share the one ceiling, and an action is refused once it is reached.")
		maxTokens   = fs.Int64("max-tokens", 0, "cap the run's total metered tokens; 0 (default) is unlimited. Shares one ceiling across a fan-out.")
		shellFlags  = registerShellFlags(fs)
		selFlags    = registerSelectionFlags(fs)
		showVersion = fs.Bool("version", false, "print version and exit")
		profileDir  = fs.String("profile", "", "capture a runtime profile bundle (cpu, heap, goroutines, a sampled timeline, and a hashed manifest) into this directory for the life of the command")
		profileCont = fs.Bool("profile-contention", false, "add block and mutex profiles to the --profile bundle; both slow every blocking operation, so they are off by default")
		leakWatch   = fs.Bool("leak-watch", false, "watch the --profile bundle's timeline for sustained growth in goroutines, live heap, open descriptors, or child processes, and dump a labelled goroutine profile, a heap profile, and the offending window into the bundle when one of them grows; requires --profile")
		leakRepeat  = fs.Bool("leak-watch-repeat", false, "let --leak-watch dump a counter more than once; by default a counter dumps once per process, because the second dump of a leak that is still leaking says what the first already said")
	)
	fs.Var(reqApproval, "require-approval", "require a person to authorize this action before the run takes it (repeat for more than one, e.g. --require-approval bash; a name the run does not offer is refused). The action pauses at the dispatch waist and the interactive session asks; a run with nobody to ask refuses it rather than taking it. Every decision, allowed or refused, is recorded on the run's own stream. With no --require-approval nothing pauses, and the run's grant and sandbox are its controls as before.")
	fs.Var(outside, "irreversible", "treat this action as reaching outside the workspace and not undoable (repeat for more than one, e.g. --irreversible bash). This adds a check on top of the run's normal grant and sandbox and takes none away: an action the grant refuses stays refused, and one it permits is now also held until --allow declares it. A run that reaches an undeclared one stops with the ask instead of being handed a refusal it would look for a way around. With no --irreversible nothing is marked and the run's grant and sandbox are its controls as before.")
	fs.Var(allowed, "allow", "declare in advance that this run may take an action marked --irreversible (repeat for more than one, e.g. --allow bash). It only lifts the hold that --irreversible puts on that action, so it needs a matching --irreversible and grants nothing the run's normal grant and sandbox do not already permit. It is the standing form of a decision nobody is present to make: an irreversible action outside the workspace is authorized by a person who wrote it down, never inferred from the objective.")
	if err := fs.Parse(args); err != nil {
		// Only a flag set that returns parse errors reaches here; flag.CommandLine exits.
		// A help request, already answered by the flag package, is not a usage error.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	// The flag package stops at the first non-flag word, so a help flag after a
	// subcommand reaches here unseen. Answer it before it becomes an objective or runs
	// the command it asked about.
	if helpRequested(fs.Args()) {
		printUsage(stdout)
		return 0
	}
	rest, usageErr := goalArgs(fs, fs.Args())
	if usageErr == flag.ErrHelp.Error() {
		printUsage(stdout)
		return 0
	}
	if usageErr != "" {
		_, _ = fmt.Fprintln(stderr, usageErr)
		return 2
	}
	vrb := *verbose || *verboseLong

	// Set before any store opens, because opening one reconciles the bundled skills.
	bundledSkillsDisabled = *noBundled

	// A misspelt trust setting is refused, because the default would run a session in a
	// mode its operator did not choose.
	projectFilesFlag = *projFiles
	if _, err := projectFilesMode(); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
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
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	selectionConfig = sel

	// The bundle spans the whole command and closes on the single exit path below. With
	// no --profile and no FLYNN_PROFILE, Start returns a nil bundle whose Stop is a no-op.
	profileCfg, usage := profileConfig(*profileDir, *profileCont, *leakWatch, *leakRepeat)
	if usage != "" {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}

	bundle, err := diag.Start(profileCfg)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer func() {
		// The command already ran, so a bundle that failed to seal is only a warning.
		if err := bundle.Stop(); err != nil {
			_, _ = fmt.Fprintln(stderr, "warning: profile bundle:", err)
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

	// An explicit --model wins, then the recorded default, then the flag's default.
	modelSpecExplicit = flagPassed(fs, "model")
	modelSpec := effectiveModelSpec(*model, modelSpecExplicit, *dataDir)

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
func goalArgs(fs *flag.FlagSet, rest []string) ([]string, string) {
	if len(rest) == 0 || rest[0] != "goal" {
		return rest, ""
	}
	literal := slices.Contains(rest[1:], "--")
	if err := fs.Parse(rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, flag.ErrHelp.Error()
		}
		return nil, fmt.Sprintf("usage: %v", err)
	}
	objective := fs.Args()
	if !literal {
		for _, arg := range objective {
			if name, ok := flagName(arg); ok && fs.Lookup(name) != nil {
				return nil, fmt.Sprintf("usage: %s is a flag, and it came after the objective, where it would have been read as part of it; put it before: flynn goal %s ... \"<objective>\"", arg, arg)
			}
		}
	}
	return append([]string{"goal"}, objective...), ""
}

// helpRequested reports whether a known subcommand's first argument is a help flag.
// Later arguments are the subcommand's own text, so an objective mentioning "-h" is
// still an objective. The help command is an ordinary route and is not matched.
func helpRequested(rest []string) bool {
	if len(rest) < 2 || !slices.Contains([]string{"-h", "--help", "-help"}, rest[1]) {
		return false
	}
	name := rest[0]
	if target, ok := commandAliases[name]; ok {
		name = target
	}
	_, invoked := invocationCommands[name]
	_, dataDirOnly := dataDirCommands[name]
	return invoked || dataDirOnly
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
}

// exit maps an ok-or-error outcome to 0, or to 1 with the message on stderr.
func (inv invocation) exit(err error) int {
	if err != nil {
		_, _ = fmt.Fprintln(inv.stderr, "error:", err)
		return 1
	}
	return 0
}

// invocationCommands are the subcommands that need the model spec, the streams or the
// run flags. Each returns its own exit code, because several have outcomes other than
// ok-or-error.
var invocationCommands = map[string]func(inv invocation, rest []string) int{
	"goal":    routeGoal,
	"inspect": routeInspect,
	"runs":    func(inv invocation, _ []string) int { return inv.exit(listRuns(inv.stdout, inv.dataDir)) },
	"resume":  routeResume,
	"regrade": func(inv invocation, _ []string) int { return inv.exit(regradeSkills(inv.stdout, inv.dataDir)) },
	"memory":  routeMemory,
	"skill":   routeSkill,
	"serve": func(inv invocation, rest []string) int {
		inv.noteShellPosture()
		return inv.exit(runServe(rest[1:], inv.modelSpec, inv.dataDir, serveGovernance{
			approve: inv.reqApproval, outside: inv.outside, allowed: inv.allowed,
			budget: budgetpkg.Limits{Tokens: inv.maxTokens, Cost: inv.maxCost},
			shell:  inv.shell,
		}))
	},
	"watch": func(inv invocation, _ []string) int {
		inv.noteShellPosture()
		return inv.exit(runWatch(inv.modelSpec, inv.dataDir, inv.learn, inv.verbose, inv.shell))
	},
	"review": routeReview,
	"help":   func(inv invocation, _ []string) int { printUsage(inv.stdout); return 0 },
}

// commandAliases maps a second name to the subcommand it runs. Usage lists only the
// subcommand.
var commandAliases = map[string]string{
	"replay":   "inspect",
	"sessions": "runs",
}

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
		_, _ = fmt.Fprintf(inv.stderr, "usage: --goal-spec states the terms of one run, so it applies to `flynn goal` and not to %s\n", what)
		return 2
	}
	name := cmd
	if target, ok := commandAliases[cmd]; ok {
		name = target
	}
	if len(inv.selection) > 0 && !selectionCommands[name] {
		_, _ = fmt.Fprintf(inv.stderr, "usage: %s chooses what a run is offered, so it applies to an interactive session, `flynn goal`, `flynn resume` and `flynn watch`, and not to flynn %s\n", inv.selection[0], cmd)
		return 2
	}
	if fn, ok := invocationCommands[name]; ok {
		return fn(inv, rest)
	}

	if fn, ok := dataDirCommands[name]; ok {
		if err := fn(rest[1:], inv.dataDir); err != nil {
			// An empty error reports an outcome through the exit code alone: `flynn version
			// check` exits non-zero when an upgrade is waiting.
			if err.Error() != "" {
				_, _ = fmt.Fprintln(inv.stderr, "error:", err)
			}
			return 1
		}
		return 0
	}

	// With no subcommand a terminal gets an interactive session; redirected stdin has no
	// one to prompt, so it gets usage.
	if len(rest) == 0 && stdinIsTerminal() {
		inv.noteShellPosture()
		return inv.exit(runInteractive(inv.modelSpec, inv.dataDir, inv.learn, inv.verbose, inv.plain, inv.reqApproval, inv.shell))
	}

	printUsage(inv.stderr)
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
			_, _ = fmt.Fprintln(inv.stderr, "error:", err)
			return 2
		}
		spec = loaded
	}
	// An objective stated nowhere, or twice differently, is a command-line error and is
	// printed without the error prefix.
	objective, err := mergeGoalSpec(spec, strings.Join(rest[1:], " "))
	if err != nil {
		_, _ = fmt.Fprintln(inv.stderr, err)
		return 2
	}
	inv.noteShellPosture()
	return inv.exit(runGoal(inv.modelSpec, objective, inv.verify, inv.dataDir, spec, inv.learn, inv.verbose, inv.fanout, inv.requireProof, inv.reqApproval, inv.outside, inv.allowed, inv.maxCost, inv.maxTokens, inv.shell))
}

func routeInspect(inv invocation, rest []string) int {
	if len(rest) < 2 {
		_, _ = fmt.Fprintln(inv.stderr, "usage: flynn inspect <run-id>")
		return 2
	}
	return inv.exit(inspectRun(inv.stdout, inv.dataDir, rest[1], inv.verbose))
}

func routeResume(inv invocation, rest []string) int {
	if len(rest) < 2 {
		_, _ = fmt.Fprintln(inv.stderr, "usage: flynn resume <run-id>")
		return 2
	}
	inv.noteShellPosture()
	return inv.exit(resumeRun(inv.modelSpec, rest[1], inv.dataDir, inv.verbose, inv.shell))
}

// routeMemory runs `flynn memory`. Consolidation distils through a model, so it needs
// the model spec and cannot be a dataDirCommands entry.
func routeMemory(inv invocation, rest []string) int {
	if err := dispatchMemory(rest[1:], inv.modelSpec, inv.dataDir, inv.stdout); err != nil {
		if errors.Is(err, errMemoryUsage) {
			_, _ = fmt.Fprintln(inv.stderr, err)
			return 2
		}
		_, _ = fmt.Fprintln(inv.stderr, "error:", err)
		return 1
	}
	return 0
}

func routeSkill(inv invocation, rest []string) int {
	if len(rest) > 1 && rest[1] == "ab" {
		inv.noteShellPosture()
	}
	if err := dispatchSkill(rest[1:], inv.modelSpec, inv.dataDir, inv.shell, inv.stdout); err != nil {
		if errors.Is(err, errSkillUsage) {
			_, _ = fmt.Fprintln(inv.stderr, err)
			return 2
		}
		_, _ = fmt.Fprintln(inv.stderr, "error:", err)
		return 1
	}
	return 0
}

// routeReview runs `flynn review`. A review that requests changes exits with
// exitChangesRequested.
func routeReview(inv invocation, rest []string) int {
	switch err := runReview(rest[1:], inv.modelSpec, inv.dataDir, inv.verbose); {
	case errors.Is(err, errChangesRequested):
		return exitChangesRequested
	case err != nil:
		_, _ = fmt.Fprintln(inv.stderr, "error:", err)
		return 1
	}
	return 0
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

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, `flynn: an autonomous software agent. Usage:
  flynn                      start an interactive session (chat turn by turn)
  flynn goal "<objective>"   drive a goal to completion in the current directory
  flynn goal --goal-spec f.json  the same, with the terms of the run stated in a file (see below)
  flynn runs                 list past runs (id, phase, objective)
  flynn get <kind>           list resources of a kind (instances, agents, runs, ...)
  flynn describe <kind> <id> show one resource's fields and recent change history
  flynn diff <kind> <a> <b>  show the fields that differ between two resources of a kind
  flynn ps                   list instances with their live, heartbeat-aware state
  flynn status [<run>]       show the live overview, or one run's phase and progress
  flynn resume <run-id>      continue a parked or interrupted run by id
  flynn steer <run-id> "..."  redirect a run that is still going; it keeps its objective and cannot report success until it says what it did about the redirect
  flynn kill <run-id> ["..."] stop a run that is still going; it halts at its next model or tool call, and the stop cannot be taken back
  flynn watch                watch the working tree for ai!/ai? comment markers and run each as a governed turn
  flynn review <pr>          review a pull request and submit a formal verdict (APPROVE gated behind --approve --as); exits 3 on changes requested
  flynn inspect <run-id>     replay a past run's recorded events (alias: replay)
  flynn spine verify <run>   report a run's record tier by tier: integrity, governance, ground truth (or --file <path> for an exported record)
  flynn spine export <run>   write a sealed run's portable record to a file (--out <path>) for third-party verification
  flynn auth set <provider>  store an API key in the encrypted vault
  flynn integrations         list the integrations, show one, or call an operation (ls, show, call)
  flynn extensions           link a local extension, list, call one confined, or unlink (dev, ls, call, rm)
  flynn deps                 list the external tools an integration declares; 'deps install <name>' fetches a pinned one
  flynn playbook             list the playbooks; 'playbook run <name> [json-input]' runs one
  flynn deploy <extension>   deploy through a hosting extension and track the result as a managed service
  flynn services             list the managed services; 'services rm <name>' removes one
  flynn models               browse the model catalog (filter with --local, --fit, --vram, ...)
  flynn init --local         set up a model on this machine: detect the hardware, recommend what fits, fetch or adopt one, probe it and make it the default
  flynn models add <hf:owner/repo | ollama:name | path>  add a model to your own catalog (unverified until blessed)
  flynn models remove <id>   remove a model you added
  flynn models bless <ref>   resolve a Hugging Face model into a verified catalog entry, print it for review and save it to the user overlay
  flynn models fetch <id>    download and verify a model's weights (does not run it)
  flynn models check         report installed local runtimes and any known parser advisories
  flynn models install [rt]  fetch and verify a pinned local runtime (default: llama.cpp)
  flynn models inspect <id>  show a model source's trust, isolation, and integrity (no run)
  flynn models run <id> [q]  provision, serve, and query a local model (q optional)
  flynn models probe <id>    measure a local model's agentic reliability and record its profile
  flynn models use <id>      provision a local model and set it as the default
  flynn models status        list the local model servers that are running
  flynn models stop <id>     stop a running local model server
  flynn db reset             move an unusable database aside (backed up) so the next run recreates it; also 'db path' and 'db backup'
  flynn notices [--refresh] [--all]  show the signed security advisories and release notices that apply to this build
  flynn regrade              re-grade learned skills against the working directory
  flynn memory ls            list what this install remembers, with ids, sources and whether each is pushed at wake (--pending: only items awaiting review)
  flynn memory promote <id>  review one of the agent's own memories into every later run's wake digest (revoke <id> takes it back out)
  flynn memory forget <id>   delete a memory, including one pinned with /remember
  flynn memory consolidate   distil each subject's accumulated episodes into one lesson and retire them (--max-calls n caps the model spend)
  flynn memory usage         show what memory was pushed at readers, what they used, and how alike the instances' pushed sets are
  flynn skill ls             list the skills a run can be offered: the pack shipped in the binary and the ones this install learned
  flynn skill show <skill>   print one skill: what it is for, how runs have taken it up, and its body
  flynn skill ab <skill>     measure whether a skill helps: its exercises run with it and without it, paired; --against/--candidate compare two versions, --max-cost usd stops at a spend, --prices file prices the tokens
  flynn serve [--telegram-token T --telegram-allow ID] [--signal-tcp ADDR --signal-allow NUMBER] [--api-addr ADDR]  run as a service: answer chat messages from the allowed senders (Telegram, Signal) and/or expose the read-only monitor API
  flynn mcp serve [--read-only]  expose the toolset to an MCP client over stdio, every call governed and recorded
  flynn --version            print the version
  flynn version [list]      print the running build, or list the releases that exist
  flynn upgrade             replace this binary with a newer, signature-verified release
  flynn sandbox check [--json] [--ready-file PATH]  start a confined child and report whether this host allows the Linux sandbox, or why not and the fix (exits 1 on any refusal)
Flags: --model, --data-dir, --no-learn, --verify "<cmd>", --goal-spec <file>, --require-proof, --require-approval <action>, --irreversible <action>, --allow <action>, --fanout, --max-cost, --max-tokens, --max-memory, --max-processes, --allow-network, --allow-credential-reads, --require-credential-hiding, --bare, --skills <slugs>, --no-skill <slugs>, --tools <names>, --no-tools, --no-bundled-skills, --project-files, -v/--verbose, --plain, --profile <dir> (run with --help for details).

A goal spec file states the terms of a run: what must stay true while it works. Each
term is audited after every step, before the goal is asked whether it is done, and a
breach stops the run naming the term. Pass it with --goal-spec ("-" reads stdin):

  {
    "objective": "upgrade the http client and keep the suite green",
    "stopCondition": "the client is upgraded and the suite passes",
    "invariants": [
      {"id": "public-api",
       "statement": "the exported API of ./client does not change",
       "check": "./dev/apidiff ./client"}
    ],
    "allowances": [{"action": "shell", "target": "prod"}]
  }

Every field is optional as long as the file states either an objective or one term;
a file of nothing but terms takes its objective from the command line as usual. A
term whose "check" is omitted is ruled on by the auditor model reading the run's
record, which is weaker than running a command: write the check where you can. A term
claiming something is not there needs one, since the record cannot show an absence.
A check runs as semi-trusted work and needs a host that can contain it (the same
requirement --require-proof carries); where it cannot be run, the run stops saying so
rather than carrying on unaudited.`)
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
		model, plan, _, err = resolveModelOrOnboard(ctx, modelSpec, modelSpecExplicit, dataDir)
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

	model, plan, _, err := resolveModelOrOnboard(ctx, modelSpec, modelSpecExplicit, dataDir)
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
	}.resuming(ctx, dataDir, shell)
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
