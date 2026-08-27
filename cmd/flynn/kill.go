package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
)

// `flynn kill` is the operator's other side of a run in flight, and it is the short one.
// `flynn steer` says keep going and do it differently; this says stop.
//
// It writes to the durable record for the same reason steer does, and the reason is
// sharper here. A stop that only works from inside the session that started the run is a
// stop that is unavailable exactly when it is wanted: the run worth killing is usually the
// one going wrong in a terminal nobody is sitting at, driven by `flynn serve` or left
// running from a session that has since been closed. Writing the order to the goal means
// there is nothing to attach to and no process to find.
//
// The stop is not deferred to the end of the step. The reconciler driving the run reads the
// order and engages the run's halt at the dispatch waist, so the model call and every tool
// call the running step tries next are refused, and the goal settles as killed with the
// operator's reason on it. An action already executing finishes on its own; the run takes
// no new ones.

// killRetries is how many times the order is re-applied after losing a write race, matching
// the redirect's. The run's own reconciler writes to this record every few seconds, so a
// conflict is ordinary rather than exceptional and the retry re-reads instead of clobbering.
const killRetries = 3

// dispatchKill implements `flynn kill <run-id> ["<reason>"]`.
func dispatchKill(args []string, dataDir string) error {
	return killRun(os.Stdout, args, dataDir)
}

// killRun is dispatchKill with the destination named, so what the command prints is what a
// test reads.
func killRun(out io.Writer, args []string, dataDir string) error {
	if len(args) < 1 {
		return errors.New(`usage: flynn kill <run-id> ["<reason>"]`)
	}
	// The reason is optional, unlike a redirect's instruction. A redirect with nothing
	// written in it cannot be delivered or ruled on; a kill with nothing written in it is
	// complete, because the order is the content and the reason is the courtesy.
	reason := strings.TrimSpace(strings.Join(args[1:], " "))

	ctx := context.Background()
	durable, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = durable.Close() }()
	reg, err := missionRegistry()
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

// applyKill writes the operator's order onto a goal's spec, re-reading and re-applying if
// the run's own reconciler wrote to the record in between.
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
		// A run that has settled has nothing left to stop. Recording the order anyway would
		// put a kill on the record of a run that finished before it arrived, and whoever
		// reads that record later has no way to tell it from a run somebody actually
		// stopped.
		if status.Phase == goal.PhaseConverged || status.Phase == goal.PhaseStalled {
			return fmt.Errorf("that run has already finished (%s): there is nothing left to stop", strings.ToLower(string(status.Phase)))
		}
		// Already killed, by an earlier invocation or another operator. Saying so beats
		// overwriting the first reason with a second one: the account of why a run was
		// stopped belongs to whoever stopped it.
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
