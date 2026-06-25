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
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/internal/version"
	"github.com/ionalpha/flynn/learn"
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

	// No subcommand: print usage. The interactive session is not wired in yet.
	fmt.Fprintln(os.Stderr, `flynn: an autonomous software agent. Usage:
  flynn goal "<objective>"   drive a goal to completion in the current directory
  flynn inspect <run-id>     replay a past run's recorded events (alias: replay)
  flynn auth set <provider>  store an API key in the encrypted vault
  flynn regrade              re-grade learned skills against the working directory
  flynn --version            print the version
Flags: --model, --data-dir, --no-learn, -v/--verbose (run with --help for details).`)
	os.Exit(2)
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

	// Resolve the model's credential through the vault first (the OS keychain, then
	// the passphrase-sealed file), falling back to the environment. So a key stored
	// once with `flynn auth set` is used automatically and nothing need be exported.
	source := secret.Chain(vault.New(dataDir, vault.WithPassphrase(terminalPassphrase)), secret.EnvSource{})
	model, err := provider.ResolveWith(ctx, modelSpec, source)
	if err != nil {
		return err
	}
	// The key is now held inside the model as a secret.Text, so drop it from the
	// process environment. The sandbox already withholds the parent environment
	// from commands; unsetting here additionally keeps the raw key out of
	// os.Environ(), a crash dump, or any future child that reads the parent env.
	for _, k := range provider.CredentialEnvVars() {
		_ = os.Unsetenv(k)
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
