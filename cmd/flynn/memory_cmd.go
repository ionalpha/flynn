package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/memory/consolidate"
	"github.com/ionalpha/flynn/memory/distil"
	"github.com/ionalpha/flynn/state"
)

// consolidateMinInterval spaces the model calls a sweep makes. A sweep is offline
// work with nobody waiting on it, so the cheap courtesy of not arriving at a
// provider as a burst costs the run nothing anybody notices.
const consolidateMinInterval = 2 * time.Second

// errMemoryUsage is the usage line for `flynn memory`.
var errMemoryUsage = errors.New("usage: flynn memory consolidate [--max-calls n]")

// dispatchMemory routes `flynn memory <sub>`.
func dispatchMemory(args []string, modelSpec, dataDir string, out io.Writer) error {
	if len(args) == 0 || args[0] != "consolidate" {
		return errMemoryUsage
	}
	return consolidateMemory(args[1:], modelSpec, dataDir, out)
}

// consolidateMemory runs the consolidation pass over this install's memory: every
// subject whose episodes have accumulated into a series is distilled into one
// lesson, and the episodes it was drawn from are retired.
//
// It is a command rather than something a run does on its way past, because it is
// the one piece of memory work that is nobody's turn. Distilling five failures
// into a lesson spends model calls on material the current run is not about, and
// hanging that off a session would charge whichever conversation happened to be
// open. The pass is built to be killed halfway and repeated, so an operator (or a
// scheduler) runs it when a spend on learning is what they meant to buy.
func consolidateMemory(args []string, modelSpec, dataDir string, out io.Writer) error {
	maxCalls, err := parseConsolidateArgs(args)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	model, _, err := resolveModel(ctx, modelSpec, dataDir)
	if err != nil {
		return err
	}

	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// The raw store, deliberately: the pass owns its own write semantics. It writes
	// the lesson with the episodes it supersedes and retires them itself, so putting
	// it behind the curated write path would have two things deciding what one write
	// retires.
	return runConsolidation(ctx, store.Memory(), consolidateDistiller(model, maxCalls), out)
}

// runConsolidation sweeps memories through d and reports what happened. It is the
// whole of the command that does not need a resolved model or a database file
// behind it.
func runConsolidation(ctx context.Context, memories state.MemoryStore, d consolidate.Distiller, out io.Writer) error {
	pass, err := consolidate.New(memories, d)
	if err != nil {
		return err
	}
	rep, err := pass.Run(ctx, state.RecallQuery{})
	if err != nil {
		return err
	}
	reportConsolidation(out, rep)
	return nil
}

// parseConsolidateArgs reads the sweep's flags, returning the model-call cap. Zero
// is no cap, which is the right default for a command an operator ran on purpose:
// a spend limit they did not ask for would leave half the store consolidated and
// say it was finished.
func parseConsolidateArgs(args []string) (maxCalls int, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--max-calls":
			if i+1 >= len(args) {
				return 0, errors.New("--max-calls needs a number")
			}
			n, cerr := strconv.Atoi(args[i+1])
			if cerr != nil || n < 0 {
				return 0, fmt.Errorf("--max-calls takes a whole number, got %q", args[i+1])
			}
			maxCalls, i = n, i+1
		default:
			return 0, fmt.Errorf("unknown flag %q", args[i])
		}
	}
	return maxCalls, nil
}

// consolidateDistiller builds the distiller a sweep runs on: model-backed, spaced,
// optionally capped, and governed like every other model call the binary makes. A
// zero cap is no cap.
func consolidateDistiller(model llm.Model, maxCalls int) consolidate.Distiller {
	opts := []distil.Option{distil.WithMinInterval(consolidateMinInterval)}
	if maxCalls > 0 {
		opts = append(opts, distil.WithMaxCalls(maxCalls))
	}
	return distil.NewGoverned(distil.New(model, opts...), dispatch.WithAdmitter(capability.Admitter{}))
}

// reportConsolidation writes what the sweep did.
//
// The two outcomes that left a series alone are reported alongside the two that
// acted, because they are the ones an operator has to be able to tell apart from
// nothing happening: a subject short of a series and a subject a model declined
// both look like silence, and only one of them is worth looking into.
func reportConsolidation(out io.Writer, rep consolidate.Report) {
	// The two that acted are counted by the report itself and named line by line
	// below; these two are the ones only this function counts.
	var tooFew, declined int
	for _, r := range rep.Results {
		if r.Outcome == consolidate.OutcomeTooFew {
			tooFew++
		}
		if r.Outcome == consolidate.OutcomeDeclined {
			declined++
		}
	}
	_, _ = fmt.Fprintf(out, "consolidate: %d distilled, %d resumed, %d not yet a series, %d declined\n",
		rep.Distilled(), rep.Resumed(), tooFew, declined)
	for _, r := range rep.Results {
		if r.Outcome == consolidate.OutcomeDistilled || r.Outcome == consolidate.OutcomeResumed {
			_, _ = fmt.Fprintf(out, "  %s: %s (%d episode(s) retired)\n", r.Subject, r.Outcome, len(r.Retired))
		}
	}
	// A failed subject is named rather than counted. The sweep carries on past one
	// so two hundred healthy subjects are not lost to a single timed-out call, which
	// only holds up if the one that failed is still visible afterwards.
	for _, f := range rep.Failures {
		_, _ = fmt.Fprintf(out, "  %s: not consolidated: %v\n", f.Subject, f.Err)
	}
}
