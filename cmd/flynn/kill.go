package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ionalpha/flynn/governed"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
)

// `flynn kill` stops a run in flight; `flynn steer` redirects one.
//
// Like steer it writes the order to the goal's durable record. The run worth killing is
// usually one nobody is watching, driven by `flynn serve` or left by a closed session,
// so there is nothing to attach to and no process to find.
//
// The reconciler reads the order and engages the run's halt at the dispatch waist: the
// step's next model and tool calls are refused, and the goal settles as killed with the
// operator's reason. An action already executing finishes; no new one starts.

// killRetries is how many times the order is re-applied after losing a write race. The
// run's reconciler writes this record every few seconds, so a conflict is ordinary and
// the retry re-reads instead of clobbering.
const killRetries = 3

func dispatchKill(args []string, dataDir string) error {
	return killRun(os.Stdout, args, dataDir)
}

// killRun is dispatchKill with the destination named, so a test reads what it prints.
func killRun(out io.Writer, args []string, dataDir string) error {
	if len(args) < 1 {
		return errors.New(`usage: flynn kill <run-id> ["<reason>"]`)
	}
	// The reason is optional: the order is complete without one. A redirect needs its
	// instruction.
	reason := strings.TrimSpace(strings.Join(args[1:], " "))

	ctx := context.Background()
	durable, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = durable.Close() }()
	reg, err := governed.Registry()
	if err != nil {
		return err
	}
	store := durable.Resources(reg)

	id, err := resolveID(ctx, store, goal.Kind, args[0])
	if err != nil {
		return err
	}
	if err := applyKill(ctx, store, id, reason); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "killed %s\n", args[0])
	if reason != "" {
		_, _ = fmt.Fprintf(out, "  %s\n", reason)
	}
	_, _ = fmt.Fprintln(out, "The run stops at its next model or tool call and settles as killed.")
	_, _ = fmt.Fprintln(out, "This cannot be taken back: restarting the work is a new run.")
	return nil
}

// applyKill writes the order onto a goal's spec, re-reading and re-applying if the
// reconciler wrote in between.
func applyKill(ctx context.Context, store resource.Store, id, reason string) error {
	var last error
	for range killRetries {
		r, err := store.GetByID(ctx, id)
		if err != nil {
			return err
		}
		spec, err := goal.DecodeSpec(r)
		if err != nil {
			return err
		}
		status, err := goal.DecodeStatus(r)
		if err != nil {
			return err
		}
		// A settled run has nothing to stop. A recorded kill would make it look stopped by
		// someone when it finished first.
		if status.Phase == goal.PhaseConverged || status.Phase == goal.PhaseStalled {
			return fmt.Errorf("that run has already finished (%s): there is nothing left to stop", strings.ToLower(string(status.Phase)))
		}
		// Already killed. The first reason stands: the account of a stop belongs to whoever
		// stopped it.
		if spec.Kill != nil {
			return fmt.Errorf("that run has already been killed (%s)", goal.KillMessage(*spec.Kill))
		}
		spec.Kill = &goal.Kill{Reason: reason}
		raw, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		r.Spec = raw
		if _, err := store.Put(ctx, r); err == nil {
			return nil
		} else if !errors.Is(err, resource.ErrConflict) {
			return err
		} else {
			last = err
		}
	}
	return fmt.Errorf("the run is being written to faster than the kill can be applied: %w", last)
}
