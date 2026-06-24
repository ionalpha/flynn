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

	agent "github.com/ionalpha/flynn"
	"github.com/ionalpha/flynn/internal/version"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/provider"
)

func main() {
	var (
		model       = flag.String("model", "anthropic:claude-opus-4-8", "model as provider:model")
		dataDir     = flag.String("data-dir", defaultDataDir(), "directory for the durable state database")
		noLearn     = flag.Bool("no-learn", false, "do not capture skills/memory from this run")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

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
		if err := runGoal(*model, objective, *dataDir, !*noLearn); err != nil {
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

	// No subcommand: the interactive session is not wired in yet.
	a := agent.New(agent.Config{Model: *model, Out: os.Stdout})
	if err := a.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
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
func runGoal(modelSpec, objective, dataDir string, learnEnabled bool) error {
	model, err := provider.Resolve(modelSpec)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	var distiller learn.Distiller
	if learnEnabled {
		distiller = learn.NewModelDistiller(model)
	}

	_, _ = fmt.Fprintf(os.Stdout, "goal: %s\n", objective)
	result, err := runLearningMission(ctx, os.Stdout, model, distiller, cwd, objective, store)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stdout, "\n"+result)
	return nil
}
