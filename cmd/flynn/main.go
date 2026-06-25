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

	"github.com/ionalpha/flynn/internal/version"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/vault"
)

func main() {
	var (
		model       = flag.String("model", "anthropic:claude-opus-4-8", "model as provider:model")
		dataDir     = flag.String("data-dir", defaultDataDir(), "directory for the durable state database")
		noLearn     = flag.Bool("no-learn", false, "do not capture skills/memory from this run")
		verbose     = flag.Bool("v", false, "verbose: show tool arguments, outputs, and per-turn detail")
		verboseLong = flag.Bool("verbose", false, "alias for -v")
		plain       = flag.Bool("plain", false, "interactive session: use the line-based interface, not the full-screen one")
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
		if err := runGoal(*model, objective, *dataDir, !*noLearn, vrb); err != nil {
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

	if args := flag.Args(); len(args) >= 1 && args[0] == "auth" {
		if err := runAuth(args[1:], *dataDir); err != nil {
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
  flynn resume <run-id>      continue a parked or interrupted run by id
  flynn inspect <run-id>     replay a past run's recorded events (alias: replay)
  flynn auth set <provider>  store an API key in the encrypted vault
  flynn regrade              re-grade learned skills against the working directory
  flynn --version            print the version
Flags: --model, --data-dir, --no-learn, -v/--verbose, --plain (run with --help for details).`)
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
func runGoal(modelSpec, objective, dataDir string, learnEnabled, verbose bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	model, err := resolveModel(ctx, modelSpec, dataDir)
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

	var distiller learn.Distiller
	if learnEnabled {
		distiller = governedDistiller(model)
	}

	// The objective and the final answer are rendered from the run's own events
	// (session.started and session.converged), so the live transcript and a later
	// `flynn inspect` of the same run read identically.
	if _, err := runLearningMission(ctx, os.Stdout, model, distiller, cwd, objective, store, verbose); err != nil {
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
func resolveModel(ctx context.Context, modelSpec, dataDir string) (llm.Model, error) {
	source := secret.Chain(vault.New(dataDir, vault.WithPassphrase(terminalPassphrase)), secret.EnvSource{})
	model, err := provider.ResolveWith(ctx, modelSpec, source)
	if err != nil {
		return nil, err
	}
	for _, k := range provider.CredentialEnvVars() {
		_ = os.Unsetenv(k)
	}
	return model, nil
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

	model, err := resolveModel(ctx, modelSpec, dataDir)
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
	_, _, _, err = drive(ctx, os.Stdout, model, cwd, "", defaultSystemPrompt, store.Resources(reg), store.Jobs(), store.Log(), verbose, runID)
	return err
}
