// Command flynn is the standalone Flynn agent binary.
//
// Build:  go build -o flynn ./cmd/flynn
// Run a goal:  flynn goal "audit the repo for TODOs and write a summary to NOTES.md"
// The model is chosen with --model provider:model (default anthropic:claude-opus-4-8);
// the provider's API key is read from its environment variable. State (skills and
// memory the agent learns) persists under --data-dir, so each run starts ahead of
// the last; --no-learn skips that capture step.
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
	"strings"
	"time"

	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/diag"
	"github.com/ionalpha/flynn/harness"
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
// They share one dispatch path in main, keyed by the first argument.
var dataDirCommands = map[string]func(args []string, dataDir string) error{
	"auth":         runAuth,
	"integrations": runIntegrations,
	"extensions":   runExtensions,
	"deps":         runDeps,
	"playbook":     runPlaybook,
	"deploy":       runDeploy,
	"services":     runServices,
	"models":       dispatchModels,
	"notices":      runNotices,
	"get":          dispatchGet,
	"mcp":          runMCP,
	"describe":     dispatchDescribe,
	"diff":         dispatchDiff,
	"ps":           dispatchPs,
	"status":       dispatchStatus,
	"spine":        dispatchSpine,
	"steer":        dispatchSteer,
	"db":           runDB,
	"version":      runVersion,
	"upgrade":      runUpgrade,
}

// staleSandboxProfileAge is how old a sandbox's operating-system profile must be before
// the startup sweep collects it. A profile belongs to a live sandbox until that sandbox
// closes, so the cutoff is set far beyond any plausible confined command rather than close
// to it: collecting one out from under a running command would break the command, and
// waiting a day to reclaim a directory costs nothing.
const staleSandboxProfileAge = 24 * time.Hour

// sweepStaleSandboxProfiles collects the sandbox profiles that earlier runs left behind.
// A sandbox unregisters its own profile when it closes, but a run that crashed or was
// killed never got the chance, and on Windows each survivor keeps a registered container
// identity whose access entries stay live. The sweep runs in the background because it is
// housekeeping, not part of the command the user asked for: a short command may well exit
// before it finishes, and the next run picks up where this one stopped. Failures are not
// reported for the same reason, and no sandbox depends on the sweep having run.
func sweepStaleSandboxProfiles() {
	cutoff := clock.System{}.Now().Add(-staleSandboxProfileAge)
	go func() { _, _ = sandbox.CleanStaleProfiles(cutoff) }()
}

// sweepSupersededBinaries collects the binary an earlier `flynn upgrade` displaced.
// Windows cannot delete a running executable, so an upgrade moves the outgoing one
// aside and leaves it for the next start to remove; that next start is this one. It is
// housekeeping, it cannot fail in a way that matters, and nothing depends on it having
// run: the displaced file is inert.
func sweepSupersededBinaries() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	go func() { _ = selfupdate.SweepSuperseded(exe) }()
}

// profileConfig assembles the diagnostics config from the flags and the environment,
// returning a usage message (and no config) for a combination that cannot be honoured.
// The flags win over the environment, except that neither can disable a watchdog the
// other turned on.
func profileConfig(dir string, contention, leakWatch, leakRepeat bool) (diag.Config, string) {
	cfg := diag.FromEnv(diag.Config{
		Dir:        dir,
		Contention: contention,
		Args:       os.Args,
		Clock:      clock.System{},
		// The sandbox is the only thing in this binary that spawns a process, and it
		// records every one it starts and reaps in the process registry. Reading that
		// registry is an atomic load, so a bundle left on for days costs nothing per
		// sample; diag has no other way to learn the count without walking the machine.
		Children: procs.Live,
	})
	if leakWatch && cfg.Leak == nil {
		cfg.Leak = &diag.LeakConfig{}
	}
	if cfg.Leak == nil {
		return cfg, ""
	}

	// The watchdog samples the bundle's timeline and dumps into the bundle, so it has
	// nowhere to look and nowhere to write without one. Refuse rather than run a long
	// soak that was quietly watching nothing.
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

// main exists only to turn run's exit code into a process exit. Every path out of the
// command returns through run, so a --profile bundle's deferred Stop always executes:
// os.Exit runs no defers, and a bundle that is never stopped is never written.
func main() { os.Exit(run(flag.CommandLine, os.Args[1:], os.Stdout, os.Stderr)) }

// run dispatches the command line and returns the process exit code: 0 on success, 1 on
// a command error, 2 on a usage error, and exitChangesRequested for a review that asked
// for changes.
//
// The flag set, the arguments, and the two output streams are passed in rather than
// reached for, so the whole dispatch is exercisable in-process. main hands it the real
// ones: flag.CommandLine (which exits on a bad flag, as it always has), the arguments
// after the program name, and the process's own streams.
func run(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) int {
	var (
		model       = fs.String("model", "anthropic:claude-opus-4-8", "model as provider:model")
		dataDir     = fs.String("data-dir", defaultDataDir(), "directory for the durable state database")
		noLearn     = fs.Bool("no-learn", false, "do not capture skills/memory from this run")
		noBundled   = fs.Bool("no-bundled-skills", false, "run without the skills shipped in the binary: the bundled set is removed from the store rather than merely left unseeded, so a run measures the learning loop against a clean baseline. Your own and learned skills are untouched.")
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
		maxMemory   = fs.Int("max-memory", 0, "cap the memory (MiB) a command the agent runs may commit; 0 (default) is unlimited. Bounds a memory bomb; enforced where the platform supports it (a Windows job object today).")
		maxProcs    = fs.Int("max-processes", 0, "cap how many processes a command the agent runs may spawn; 0 (default) uses the platform's generous fork-bomb backstop.")
		showVersion = fs.Bool("version", false, "print version and exit")
		profileDir  = fs.String("profile", "", "capture a runtime profile bundle (cpu, heap, goroutines, a sampled timeline, and a hashed manifest) into this directory for the life of the command")
		profileCont = fs.Bool("profile-contention", false, "add block and mutex profiles to the --profile bundle; both slow every blocking operation, so they are off by default")
		leakWatch   = fs.Bool("leak-watch", false, "watch the --profile bundle's timeline for sustained growth in goroutines, live heap, open descriptors, or child processes, and dump a labelled goroutine profile, a heap profile, and the offending window into the bundle when one of them grows; requires --profile")
		leakRepeat  = fs.Bool("leak-watch-repeat", false, "let --leak-watch dump a counter more than once; by default a counter dumps once per process, because the second dump of a leak that is still leaking says what the first already said")
	)
	fs.Var(reqApproval, "require-approval", "require a person to authorize this action before the run takes it (repeat for more than one, e.g. --require-approval shell). The action pauses at the dispatch waist and the interactive session asks; a run with nobody to ask refuses it rather than taking it. Every decision, allowed or refused, is recorded on the run's own stream. With no --require-approval nothing pauses, and the run's grant and sandbox are its controls as before.")
	fs.Var(outside, "irreversible", "treat this action as reaching outside the workspace and not undoable (repeat for more than one, e.g. --irreversible shell). The run takes it only where --allow declared it, and a run that reaches an undeclared one stops with the ask instead of being handed a refusal it would look for a way around. With no --irreversible nothing is marked and the run's grant and sandbox are its controls as before.")
	fs.Var(allowed, "allow", "declare in advance that this run may take an action marked --irreversible (repeat for more than one, e.g. --allow shell). It is the standing form of a decision nobody is present to make: an irreversible action outside the workspace is authorized by a person who wrote it down, never inferred from the objective.")
	if err := fs.Parse(args); err != nil {
		// flag.CommandLine exits the process on a bad flag, so this is reached only by a
		// flag set that was asked to hand parse errors back: a bad flag is a usage error.
		return 2
	}
	vrb := *verbose || *verboseLong

	// Whether the binary's own skills apply is settled here, before anything opens a
	// store, because opening one is what reconciles them.
	bundledSkillsDisabled = *noBundled

	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version.String())
		return 0
	}

	// A profile bundle spans the whole command, so it opens before any work and closes
	// on the single exit path below. With no --profile and no FLYNN_PROFILE this costs
	// nothing: Start returns a nil bundle and Stop on it is a no-op.
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
		// A bundle that failed to seal is a warning, not a failed command: the work the
		// user asked for already happened, and its exit code is theirs, not the profiler's.
		if err := bundle.Stop(); err != nil {
			_, _ = fmt.Fprintln(stderr, "warning: profile bundle:", err)
		}
	}()

	sweepStaleSandboxProfiles()
	sweepSupersededBinaries()

	// Say anything we owe the user (a security advisory about the version they are
	// running, above all) before the command runs, and look for a newer feed in the
	// background. Both are best-effort and neither can fail the command.
	startupNotices(context.Background(), *dataDir)

	// The model to drive: an explicit --model wins; otherwise a previously chosen default
	// (from onboarding, /model, or `flynn models use`) applies; otherwise the built-in
	// default the flag carries. So a user need not repeat --model once one is chosen.
	modelSpecExplicit = flagPassed(fs, "model")
	modelSpec := effectiveModelSpec(*model, modelSpecExplicit, *dataDir)

	// The subcommand, or "" when none was given. No subcommand is named "", so every
	// branch below can compare against it directly.
	rest := fs.Args()
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
		maxMemoryMiB: *maxMemory,
		maxProcesses: *maxProcs,
	})
}

// invocation carries the resolved command-line state routeCommand needs to run a subcommand: the
// output streams, the chosen model spec and data dir, verbosity, and the run-shaping flags a
// `goal` consumes. run parses these once and hands them over so routeCommand stays a pure router.
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
	// goalSpec is the path to the goal spec file, or "" when none was passed. It is the
	// path rather than the loaded spec because loading it is a fallible step the router
	// reports as a usage error.
	goalSpec     string
	reqApproval  []string
	outside      []string
	allowed      []string
	maxCost      float64
	maxTokens    int64
	maxMemoryMiB int
	maxProcesses int
}

// exit maps a subcommand's error to a process exit code: 1 with the message on stderr, or 0 on
// success. It is the uniform tail shared by every command whose only outcome is ok-or-error.
func (inv invocation) exit(err error) int {
	if err != nil {
		_, _ = fmt.Fprintln(inv.stderr, "error:", err)
		return 1
	}
	return 0
}

// routeCommand routes a parsed command line to its subcommand and returns the process exit code.
// It is the command table split out of run so parsing/profiling setup and command routing each
// stay small; the exit-code contract lives here: 0 on success, 1 on a command error, 2 on a usage
// error, and exitChangesRequested for a review that asked for changes.
func routeCommand(cmd string, rest []string, inv invocation) int {
	// A goal spec is the terms of one run, and only `flynn goal` submits one. Passing it
	// to anything else is refused rather than ignored: silently dropping it would leave an
	// operator believing their run is being held to terms that were never in force, which
	// is worse than not having stated them.
	if inv.goalSpec != "" && cmd != "goal" {
		what := "flynn " + cmd
		if cmd == "" {
			what = "an interactive session"
		}
		_, _ = fmt.Fprintf(inv.stderr, "usage: --goal-spec states the terms of one run, so it applies to `flynn goal` and not to %s\n", what)
		return 2
	}
	switch cmd {
	case "goal":
		// The run's terms are read and validated here, before a store is opened or a
		// credential is resolved: a spec file that will not load is a usage error, and the
		// operator finds out while they are still looking at the file rather than after
		// the run has started without the terms they wrote.
		var spec goalSpecFile
		if inv.goalSpec != "" {
			loaded, err := loadGoalSpecFile(inv.goalSpec)
			if err != nil {
				_, _ = fmt.Fprintln(inv.stderr, "error:", err)
				return 2
			}
			spec = loaded
		}
		// Printed without the error prefix: an objective stated nowhere, or stated twice
		// and differently, is the command line being wrong rather than the run failing.
		objective, err := mergeGoalSpec(spec, strings.Join(rest[1:], " "))
		if err != nil {
			_, _ = fmt.Fprintln(inv.stderr, err)
			return 2
		}
		return inv.exit(runGoal(inv.modelSpec, objective, inv.verify, inv.dataDir, spec, inv.learn, inv.verbose, inv.fanout, inv.requireProof, inv.reqApproval, inv.outside, inv.allowed, inv.maxCost, inv.maxTokens, inv.maxMemoryMiB, inv.maxProcesses))

	case "inspect", "replay":
		if len(rest) < 2 {
			_, _ = fmt.Fprintln(inv.stderr, "usage: flynn inspect <run-id>")
			return 2
		}
		return inv.exit(inspectRun(inv.stdout, inv.dataDir, rest[1], inv.verbose))

	case "runs", "sessions":
		return inv.exit(listRuns(inv.stdout, inv.dataDir))

	case "resume":
		if len(rest) < 2 {
			_, _ = fmt.Fprintln(inv.stderr, "usage: flynn resume <run-id>")
			return 2
		}
		return inv.exit(resumeRun(inv.modelSpec, rest[1], inv.dataDir, inv.verbose))

	case "regrade":
		return inv.exit(regradeSkills(inv.stdout, inv.dataDir))

	case "memory":
		// Not a dataDirCommands entry: consolidation distils a series through a model,
		// so this one needs the run's model spec as well as the data directory.
		if err := dispatchMemory(rest[1:], inv.modelSpec, inv.dataDir, inv.stdout); err != nil {
			if errors.Is(err, errMemoryUsage) {
				_, _ = fmt.Fprintln(inv.stderr, err)
				return 2
			}
			_, _ = fmt.Fprintln(inv.stderr, "error:", err)
			return 1
		}
		return 0

	case "skill":
		if len(rest) < 2 || rest[1] != "ab" {
			_, _ = fmt.Fprintln(inv.stderr, `usage: flynn skill ab <skill> [--repeats n] [--exercises dir]`)
			return 2
		}
		return inv.exit(runSkillAB(rest[2:], inv.modelSpec, inv.dataDir, inv.stdout))

	case "serve":
		return inv.exit(runServe(rest[1:], inv.modelSpec, inv.dataDir))

	case "watch":
		return inv.exit(runWatch(inv.modelSpec, inv.dataDir, inv.learn, inv.verbose))

	case "review":
		// review is the one command whose non-error outcome is not simply success: a run that
		// requested changes exits with its own code rather than 0 or 1.
		switch err := runReview(rest[1:], inv.modelSpec, inv.dataDir, inv.verbose); {
		case errors.Is(err, errChangesRequested):
			return exitChangesRequested
		case err != nil:
			_, _ = fmt.Fprintln(inv.stderr, "error:", err)
			return 1
		}
		return 0

	case "help":
		printUsage(inv.stdout)
		return 0
	}

	// Subcommands that take only the data directory share one dispatch path, so adding one is a
	// table entry rather than another case here.
	if fn, ok := dataDirCommands[cmd]; ok {
		if err := fn(rest[1:], inv.dataDir); err != nil {
			// An error with no message is a command reporting an outcome through its exit code
			// rather than a failure: `flynn version check` exits non-zero when an upgrade is
			// waiting, and nothing has gone wrong that warrants a message.
			if err.Error() != "" {
				_, _ = fmt.Fprintln(inv.stderr, "error:", err)
			}
			return 1
		}
		return 0
	}

	// No subcommand: start an interactive session when attached to a terminal, where each line
	// is a turn of one continuing conversation. With stdin redirected (a pipe, a file, a CI
	// step) there is no one to prompt, so print usage instead.
	if len(rest) == 0 && stdinIsTerminal() {
		return inv.exit(runInteractive(inv.modelSpec, inv.dataDir, inv.learn, inv.verbose, inv.plain, inv.reqApproval))
	}

	printUsage(inv.stderr)
	return 2
}

// effectiveModelSpec resolves which model to drive: the explicit --model value when the
// user passed one, else the recorded default under dataDir, else the flag's built-in
// default. So once a model is chosen (onboarding, /model, or `flynn models use`), a later
// launch reuses it without repeating --model.
func effectiveModelSpec(flagValue string, explicit bool, dataDir string) string {
	if explicit {
		return flagValue
	}
	if saved, ok := readActiveModel(dataDir); ok {
		return saved
	}
	return flagValue
}

// modelSpecExplicit records whether --model was named on the command line. Every command
// resolves the same spec, but only a spec the user named carries an instruction about
// which provider may see the work, so the resolver refuses a missing credential instead
// of falling back when this is set.
var modelSpecExplicit bool

// bundledSkillsDisabled records whether --no-bundled-skills was named. It is read
// where the durable store is opened rather than passed through every command,
// because reconciling the pack is part of opening the store and every command that
// opens one has to agree about it.
var bundledSkillsDisabled bool

// flagPassed reports whether the named flag was set on the command line, so a default can
// be told apart from a value the user passed explicitly.
func flagPassed(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// printUsage writes the command summary to w.
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
  flynn models bless <ref>   resolve a Hugging Face model into a verified catalog entry and print it for review
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
  flynn memory consolidate   distil each subject's accumulated episodes into one lesson and retire them (--max-calls n caps the model spend)
  flynn memory usage         show what memory was pushed at readers, what they used, and how alike the instances' pushed sets are
  flynn skill ab <skill>     measure whether a skill helps: its exercises run with it and without it, paired
  flynn serve [--telegram-token T] [--signal-tcp ADDR] [--api-addr ADDR]  run as a service: answer chat messages (Telegram, Signal) and/or expose the read-only monitor API
  flynn mcp serve [--read-only]  expose the toolset to an MCP client over stdio, every call governed and recorded
  flynn --version            print the version
  flynn version [list]      print the running build, or list the releases that exist
  flynn upgrade             replace this binary with a newer, signature-verified release
Flags: --model, --data-dir, --no-learn, --verify "<cmd>", --goal-spec <file>, --require-proof, --require-approval <action>, --irreversible <action>, --allow <action>, --fanout, --max-cost, --max-tokens, --max-memory, --max-processes, -v/--verbose, --plain, --profile <dir> (run with --help for details).

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

// defaultDataDir is where durable state lives unless overridden: a per-user
// directory so learning compounds across projects. It falls back to a local
// directory when the user config dir is unavailable. A development build uses a
// separate directory (dataDirName), so working on the schema on a branch never migrates,
// or is blocked by, a real installation's database.
func defaultDataDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, dataDirName())
	}
	return "." + dataDirName()
}

// dataDirName is the per-user directory name for durable state: "flynn" for a release
// build, "flynn-dev" for an unstamped development build, so the two never share a
// database.
func dataDirName() string {
	if version.IsDev() {
		return "flynn-dev"
	}
	return "flynn"
}

// runGoal resolves the model, opens the durable store, and drives one objective to
// completion in the current directory, recalling past learning into the prompt and
// (unless disabled) distilling the result back out. Progress and the final result
// are printed; Ctrl-C cancels the run.
func runGoal(modelSpec, objective, verify, dataDir string, spec goalSpecFile, learnEnabled, verbose, fanout, requireProof bool, reqApproval, outside, allowed []string, maxCost float64, maxTokens int64, maxMemoryMiB, maxProcesses int) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	// Resolve the run's backend: a native model conversation, or an external agent CLI
	// whose own harness drives the loop. A `--model codex:<model>` spec selects the
	// latter; anything else resolves a hosted or local model as before.
	var (
		model    llm.Model
		plan     harness.Plan
		extAgent *externAgent
	)
	if name, cliModel, ok := externalAgentSpec(modelSpec); ok {
		// Fan-out delegates to concurrent native child loops; an external harness owns its
		// own loop, so the combination is refused rather than silently ignored.
		if fanout {
			return fmt.Errorf("--fanout is not supported with the %s external agent backend: it drives its own loop", name)
		}
		extAgent, err = resolveExternalAgent(ctx, name, cliModel, cwd)
		if err != nil {
			return err
		}
		// The harness's home outlives its episodes (it holds the CLI's own conversation) and
		// dies with the run.
		defer extAgent.close()
	} else {
		model, plan, _, err = resolveModelOrOnboard(ctx, modelSpec, modelSpecExplicit, dataDir)
		if err != nil {
			return err
		}
	}

	// Load the instance signer so the run is sealed into a verifiable record. This is
	// best effort: if the identity cannot be loaded, the run proceeds unsigned rather
	// than failing. Loaded before the store so the store's snapshots are sealed under
	// the same key: with a signer, resource snapshots are verified (checkpoint-bound,
	// COSE-signed) and written automatically as the stream grows.
	signer, serr := runSigner(ctx, dataDir)
	if serr != nil {
		signer = nil
	}
	store, err := openDataStore(ctx, dataDir, snapshotOptions(signer)...)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// Learning distills a converged run through a model. An external agent exposes no
	// model of its own (its inner loop is unobserved), so a run it drives does not
	// distill: the run is still sealed and verifiable, only the learn-back step is
	// skipped.
	var distiller learn.Distiller
	if learnEnabled && extAgent == nil {
		distiller = governedDistiller(model)
	}

	// With fan-out enabled the run drives the full goals engine: the model may
	// delegate sub-goals to concurrent child agents, each routed to the model its
	// bound archetype pins, all folded into this run's one sealed record. A child that
	// names a model resolves it through the same credential chain as the root.
	var fc *fanoutConfig
	if fanout {
		fc = &fanoutConfig{resolveModel: childModelResolver(ctx, dataDir)}
	}

	opts := []driveOption{
		withBudget(budgetpkg.Limits{Tokens: maxTokens, Cost: maxCost}),
		withResourceLimits(sandbox.ResourceLimits{MemoryMiB: maxMemoryMiB, MaxProcesses: maxProcesses}),
		// A one-shot run has no operator at a prompt, so it carries the gate without a
		// prompter: a listed action is refused rather than taken. That is the fail-closed
		// half of the same mechanism the interactive session resolves by asking.
		withApproval(reqApproval, nil),
		// The actions that reach outside the workspace, and the ones this run was declared
		// to be allowed among them. Both belong to the person who started the run: the run
		// itself can neither mark an action safe nor declare itself authorized, which is
		// what makes a declaration something other than the run's own reading of the
		// objective.
		withAllowance(outside, mergeAllowances(spec, allowed)),
		// The terms the operator stated, and their stop condition where they wrote one.
		withGoalSpec(spec),
		// What recall ranks by. Nil unless this install names an embedding model, which
		// leaves the lexical order every run has had until now.
		withMemoryEmbedder(configuredEmbedder(ctx, dataDir, noticeLine(os.Stdout))),
	}
	if extAgent != nil {
		opts = append(opts, withExternalAgent(extAgent))
	} else {
		// A native goal plans before it builds: it expands its objective into a visible
		// ledger first. An external agent CLI drives its own loop and its own planning, so
		// the phase is only added to the native path.
		opts = append(opts, withPlanning())
		// Hold the run to that plan when asked. Planning alone runs each item's check
		// and records the verdict, so a run always says how much of its plan it actually
		// proved; this is what makes an unproven item stop the run rather than only show
		// up in the record.
		if requireProof {
			opts = append(opts, withLedgerProof())
		}
	}

	// The objective and the final answer are rendered from the run's own events
	// (session.started and session.converged), so the live transcript and a later
	// `flynn inspect` of the same run read identically.
	if _, err := runLearningMission(ctx, os.Stdout, model, plan, distiller, cwd, objective, verify, store, signer, verbose, fc, opts...); err != nil {
		return err
	}
	return nil
}

// resolveModel resolves the model's credential through the vault first (the OS
// keychain, then the passphrase-sealed file), falling back to the environment, so a
// key stored once with `flynn auth set` is used automatically and nothing need be
// exported. The key is then dropped from the process environment: it lives inside
// the model as a secret.Text, and the sandbox already withholds the parent
// environment from commands, so unsetting keeps the raw key out of os.Environ(), a
// crash dump, or any child that reads the parent env.
func resolveModel(ctx context.Context, modelSpec, dataDir string) (llm.Model, harness.Plan, error) {
	// A local catalog model resolves by provisioning and serving it on demand, then
	// talking to its loopback endpoint, so selecting it is zero-touch: nothing has to be
	// installed, downloaded, or started by hand. A hosted provider spec falls through to
	// the credential-backed resolver below.
	if isLocalModelID(modelSpec) {
		return resolveLocalModel(ctx, modelSpec, dataDir)
	}
	model, err := provider.ResolveWith(ctx, modelSpec, credentialSource(dataDir))
	if err != nil {
		return nil, harness.Plan{}, err
	}
	for _, k := range provider.CredentialEnvVars() {
		_ = os.Unsetenv(k)
	}
	// A hosted frontier model is driven leanly: the zero plan applies no scaffolding,
	// preserving its full schemas and single-pass convergence.
	return model, harness.Plan{}, nil
}

// credentialSource is the credential lookup order: the vault first (the OS
// keychain, then the passphrase-sealed file), then the environment. One place
// builds it so model resolution and provider auto-detection read the same chain.
func credentialSource(dataDir string) secret.Source {
	return secret.Chain(vault.New(dataDir, vault.WithPassphrase(terminalPassphrase)), secret.EnvSource{})
}

// resumeRun continues an existing run by its id: it re-drives the run's goal from
// where it was left, streaming the rest of the conversation onto the same durable
// stream. The prior conversation replays first, then the run is driven to its
// terminal phase. Ctrl-C detaches without losing the run, which can be resumed
// again. Learning capture is skipped: a resume continues a run, it does not start a
// fresh one to distill.
func resumeRun(modelSpec, runID, dataDir string, verbose bool) error {
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
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	reg, err := missionRegistry()
	if err != nil {
		return err
	}
	_, _, _, err = drive(ctx, os.Stdout, model, plan, cwd, "", defaultSystemPrompt, store.Resources(reg), store.Jobs(), store.Log(), verbose, runID, nil)
	return err
}
