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
	"get":          dispatchGet,
	"mcp":          runMCP,
	"describe":     dispatchDescribe,
	"diff":         dispatchDiff,
	"ps":           dispatchPs,
	"status":       dispatchStatus,
	"spine":        dispatchSpine,
	"db":           runDB,
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
func main() { os.Exit(run()) }

// run dispatches the command line and returns the process exit code: 0 on success, 1 on
// a command error, 2 on a usage error, and exitChangesRequested for a review that asked
// for changes.
func run() int {
	var (
		model       = flag.String("model", "anthropic:claude-opus-4-8", "model as provider:model")
		dataDir     = flag.String("data-dir", defaultDataDir(), "directory for the durable state database")
		noLearn     = flag.Bool("no-learn", false, "do not capture skills/memory from this run")
		verbose     = flag.Bool("v", false, "verbose: show tool arguments, outputs, and per-turn detail")
		verboseLong = flag.Bool("verbose", false, "alias for -v")
		plain       = flag.Bool("plain", false, "interactive session: use the line-based interface, not the full-screen one")
		verify      = flag.String("verify", "", "a command that independently checks the goal succeeded; run after the agent stops, its result grounds the run's success in the verifiable record")
		fanout      = flag.Bool("fanout", false, "let the goal delegate sub-tasks to concurrent child agents (each routed to the model its archetype pins), all folded into one verifiable record")
		maxCost     = flag.Float64("max-cost", 0, "cap the run's total model+tool spend in the provider's currency unit; 0 (default) is unlimited. A fan-out's children share the one ceiling, and an action is refused once it is reached.")
		maxTokens   = flag.Int64("max-tokens", 0, "cap the run's total metered tokens; 0 (default) is unlimited. Shares one ceiling across a fan-out.")
		maxMemory   = flag.Int("max-memory", 0, "cap the memory (MiB) a command the agent runs may commit; 0 (default) is unlimited. Bounds a memory bomb; enforced where the platform supports it (a Windows job object today).")
		maxProcs    = flag.Int("max-processes", 0, "cap how many processes a command the agent runs may spawn; 0 (default) uses the platform's generous fork-bomb backstop.")
		showVersion = flag.Bool("version", false, "print version and exit")
		profileDir  = flag.String("profile", "", "capture a runtime profile bundle (cpu, heap, goroutines, a sampled timeline, and a hashed manifest) into this directory for the life of the command")
		profileCont = flag.Bool("profile-contention", false, "add block and mutex profiles to the --profile bundle; both slow every blocking operation, so they are off by default")
		leakWatch   = flag.Bool("leak-watch", false, "watch the --profile bundle's timeline for sustained growth in goroutines, live heap, open descriptors, or child processes, and dump a labelled goroutine profile, a heap profile, and the offending window into the bundle when one of them grows; requires --profile")
		leakRepeat  = flag.Bool("leak-watch-repeat", false, "let --leak-watch dump a counter more than once; by default a counter dumps once per process, because the second dump of a leak that is still leaking says what the first already said")
	)
	flag.Parse()
	vrb := *verbose || *verboseLong

	if *showVersion {
		_, _ = fmt.Fprintln(os.Stdout, version.String())
		return 0
	}

	// A profile bundle spans the whole command, so it opens before any work and closes
	// on the single exit path below. With no --profile and no FLYNN_PROFILE this costs
	// nothing: Start returns a nil bundle and Stop on it is a no-op.
	profileCfg, usage := profileConfig(*profileDir, *profileCont, *leakWatch, *leakRepeat)
	if usage != "" {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}

	bundle, err := diag.Start(profileCfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer func() {
		// A bundle that failed to seal is a warning, not a failed command: the work the
		// user asked for already happened, and its exit code is theirs, not the profiler's.
		if err := bundle.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: profile bundle:", err)
		}
	}()

	sweepStaleSandboxProfiles()

	// The model to drive: an explicit --model wins; otherwise a previously chosen default
	// (from onboarding, /model, or `flynn models use`) applies; otherwise the built-in
	// default the flag carries. So a user need not repeat --model once one is chosen.
	modelSpec := effectiveModelSpec(*model, flagSet("model"), *dataDir)

	if args := flag.Args(); len(args) >= 1 && args[0] == "goal" {
		objective := strings.TrimSpace(strings.Join(args[1:], " "))
		if objective == "" {
			fmt.Fprintln(os.Stderr, `usage: flynn goal "<objective>"`)
			return 2
		}
		if err := runGoal(modelSpec, objective, *verify, *dataDir, !*noLearn, vrb, *fanout, *maxCost, *maxTokens, *maxMemory, *maxProcs); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}

	if args := flag.Args(); len(args) >= 1 && (args[0] == "inspect" || args[0] == "replay") {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: flynn inspect <run-id>")
			return 2
		}
		if err := inspectRun(*dataDir, args[1], vrb); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}

	if args := flag.Args(); len(args) >= 1 && (args[0] == "runs" || args[0] == "sessions") {
		if err := listRuns(*dataDir); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}

	if args := flag.Args(); len(args) >= 1 && args[0] == "resume" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: flynn resume <run-id>")
			return 2
		}
		if err := resumeRun(modelSpec, args[1], *dataDir, vrb); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}

	if args := flag.Args(); len(args) >= 1 && args[0] == "regrade" {
		if err := regradeSkills(*dataDir); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}

	// Subcommands that take only the data directory share one dispatch path, so
	// adding one is a table entry rather than another branch in run.
	if args := flag.Args(); len(args) >= 1 {
		if fn, ok := dataDirCommands[args[0]]; ok {
			if err := fn(args[1:], *dataDir); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				return 1
			}
			return 0
		}
	}

	if args := flag.Args(); len(args) >= 1 && args[0] == "serve" {
		if err := runServe(args[1:], modelSpec, *dataDir); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}

	if args := flag.Args(); len(args) >= 1 && args[0] == "watch" {
		if err := runWatch(modelSpec, *dataDir, !*noLearn, vrb); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}

	if args := flag.Args(); len(args) >= 1 && args[0] == "review" {
		err := runReview(args[1:], modelSpec, *dataDir, vrb)
		switch {
		case errors.Is(err, errChangesRequested):
			return exitChangesRequested
		case err != nil:
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}

	if args := flag.Args(); len(args) >= 1 && args[0] == "help" {
		printUsage(os.Stdout)
		return 0
	}

	// No subcommand: start an interactive session when attached to a terminal, where
	// each line is a turn of one continuing conversation. With stdin redirected (a
	// pipe, a file, a CI step) there is no one to prompt, so print usage instead.
	if len(flag.Args()) == 0 && stdinIsTerminal() {
		if err := runInteractive(modelSpec, *dataDir, !*noLearn, vrb, *plain); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}

	printUsage(os.Stderr)
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

// flagSet reports whether the named flag was set on the command line, so a default can be
// told apart from a value the user passed explicitly.
func flagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
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
  flynn runs                 list past runs (id, phase, objective)
  flynn get <kind>           list resources of a kind (instances, agents, runs, ...)
  flynn describe <kind> <id> show one resource's fields and recent change history
  flynn diff <kind> <a> <b>  show the fields that differ between two resources of a kind
  flynn ps                   list instances with their live, heartbeat-aware state
  flynn status [<run>]       show the live overview, or one run's phase and progress
  flynn resume <run-id>      continue a parked or interrupted run by id
  flynn watch                watch the working tree for ai!/ai? comment markers and run each as a governed turn
  flynn review <pr>          review a pull request and submit a formal verdict (APPROVE gated behind --approve --as); exits 3 on changes requested
  flynn inspect <run-id>     replay a past run's recorded events (alias: replay)
  flynn spine verify <run>   report a run's record tier by tier: integrity, governance, ground truth (or --file <path> for an exported record)
  flynn spine export <run>   write a sealed run's portable record to a file (--out <path>) for third-party verification
  flynn auth set <provider>  store an API key in the encrypted vault
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
  flynn regrade              re-grade learned skills against the working directory
  flynn serve [--telegram-token T] [--signal-tcp ADDR] [--api-addr ADDR]  run as a service: answer chat messages (Telegram, Signal) and/or expose the read-only monitor API
  flynn mcp serve [--read-only]  expose the toolset to an MCP client over stdio, every call governed and recorded
  flynn --version            print the version
Flags: --model, --data-dir, --no-learn, --verify "<cmd>", --fanout, --max-cost, --max-tokens, --max-memory, --max-processes, -v/--verbose, --plain, --profile <dir> (run with --help for details).`)
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
func runGoal(modelSpec, objective, verify, dataDir string, learnEnabled, verbose, fanout bool, maxCost float64, maxTokens int64, maxMemoryMiB, maxProcesses int) error {
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
	} else {
		model, plan, _, err = resolveModelOrOnboard(ctx, modelSpec, dataDir)
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
	}
	if extAgent != nil {
		opts = append(opts, withExternalAgent(extAgent))
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

	model, plan, _, err := resolveModelOrOnboard(ctx, modelSpec, dataDir)
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
