// Command flynn is the standalone Flynn agent binary.
//
// Build:  go build -o flynn ./cmd/flynn
// Run a goal:  flynn goal "audit the repo for TODOs and write a summary to NOTES.md"
// The model is chosen with --model provider:model (default anthropic:claude-opus-4-8);
// the provider's API key is read from its environment variable.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	agent "github.com/ionalpha/flynn"
	"github.com/ionalpha/flynn/internal/version"
	"github.com/ionalpha/flynn/provider"
)

func main() {
	var (
		model       = flag.String("model", "anthropic:claude-opus-4-8", "model as provider:model")
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
		if err := runGoal(*model, objective); err != nil {
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

// runGoal resolves the model and drives one objective to completion in the current
// directory, printing progress and the final result. Ctrl-C cancels the run.
func runGoal(modelSpec, objective string) error {
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

	_, _ = fmt.Fprintf(os.Stdout, "goal: %s\n", objective)
	result, err := runMission(ctx, os.Stdout, model, cwd, objective)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stdout, "\n"+result)
	return nil
}
