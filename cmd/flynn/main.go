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
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/internal/version"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/secret"
)

// dataDirCommands are the subcommands whose only state is the data directory.
// They share one dispatch path in main, keyed by the first argument.
var dataDirCommands = map[string]func(args []string, dataDir string) error{
	"auth":         runAuth,
	"integrations": runIntegrations,
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
}

func main() {
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
	)
	flag.Parse()
	vrb := *verbose || *verboseLong

	if *showVersion {
		_, _ = fmt.Fprintln(os.Stdout, version.String())
		return
	}

	if args := flag.Args(); len(args) >= 1 && args[0] == "goal" {
		objective := strings.TrimSpace(strings.Join(args[1:], " "))
		if objective == "" {
			fmt.Fprintln(os.Stderr, `usage: flynn goal "<objective>"`)
			os.Exit(2)
		}
		if err := runGoal(*model, objective, *verify, *dataDir, !*noLearn, vrb, *fanout, *maxCost, *maxTokens, *maxMemory, *maxProcs); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if args := flag.Args(); len(args) >= 1 && (args[0] == "inspect" || args[0] == "replay") {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: flynn inspect <run-id>")
			os.Exit(2)
		}
		if err := inspectRun(*dataDir, args[1], vrb); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if args := flag.Args(); len(args) >= 1 && (args[0] == "runs" || args[0] == "sessions") {
		if err := listRuns(*dataDir); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if args := flag.Args(); len(args) >= 1 && args[0] == "resume" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: flynn resume <run-id>")
			os.Exit(2)
		}
		if err := resumeRun(*model, args[1], *dataDir, vrb); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if args := flag.Args(); len(args) >= 1 && args[0] == "regrade" {
		if err := regradeSkills(*dataDir); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// Subcommands that take only the data directory share one dispatch path, so
	// adding one is a table entry rather than another branch in main.
	if args := flag.Args(); len(args) >= 1 {
		if fn, ok := dataDirCommands[args[0]]; ok {
			if err := fn(args[1:], *dataDir); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
	}

	if args := flag.Args(); len(args) >= 1 && args[0] == "serve" {
		if err := runServe(args[1:], *model, *dataDir); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if args := flag.Args(); len(args) >= 1 && args[0] == "watch" {
		if err := runWatch(*model, *dataDir, !*noLearn, vrb); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if args := flag.Args(); len(args) >= 1 && args[0] == "help" {
		printUsage(os.Stdout)
		return
	}

	// No subcommand: start an interactive session when attached to a terminal, where
	// each line is a turn of one continuing conversation. With stdin redirected (a
	// pipe, a file, a CI step) there is no one to prompt, so print usage instead.
	if len(flag.Args()) == 0 && stdinIsTerminal() {
		if err := runInteractive(*model, *dataDir, !*noLearn, vrb, *plain); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	printUsage(os.Stderr)
	os.Exit(2)
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
  flynn regrade              re-grade learned skills against the working directory
  flynn serve [--telegram-token T] [--signal-tcp ADDR] [--api-addr ADDR]  run as a service: answer chat messages (Telegram, Signal) and/or expose the read-only monitor API
  flynn mcp serve [--read-only]  expose the toolset to an MCP client over stdio, every call governed and recorded
  flynn --version            print the version
Flags: --model, --data-dir, --no-learn, --verify "<cmd>", --fanout, --max-cost, --max-tokens, --max-memory, --max-processes, -v/--verbose, --plain (run with --help for details).`)
}

// defaultDataDir is where durable state lives unless overridden: a per-user
// directory so learning compounds across projects. It falls back to a local
// directory when the user config dir is unavailable.
func defaultDataDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "flynn")
	}
	return ".flynn"
}

// runGoal resolves the model, opens the durable store, and drives one objective to
// completion in the current directory, recalling past learning into the prompt and
// (unless disabled) distilling the result back out. Progress and the final result
// are printed; Ctrl-C cancels the run.
func runGoal(modelSpec, objective, verify, dataDir string, learnEnabled, verbose, fanout bool, maxCost float64, maxTokens int64, maxMemoryMiB, maxProcesses int) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	model, plan, err := resolveModelOrOnboard(ctx, modelSpec, dataDir)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
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

	var distiller learn.Distiller
	if learnEnabled {
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

	// The objective and the final answer are rendered from the run's own events
	// (session.started and session.converged), so the live transcript and a later
	// `flynn inspect` of the same run read identically.
	if _, err := runLearningMission(ctx, os.Stdout, model, plan, distiller, cwd, objective, verify, store, signer, verbose, fc,
		withBudget(budgetpkg.Limits{Tokens: maxTokens, Cost: maxCost}),
		withResourceLimits(sandbox.ResourceLimits{MemoryMiB: maxMemoryMiB, MaxProcesses: maxProcesses})); err != nil {
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

	model, plan, err := resolveModelOrOnboard(ctx, modelSpec, dataDir)
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
