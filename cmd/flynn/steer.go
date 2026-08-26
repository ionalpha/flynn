package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
)

// `flynn steer` is the operator's side of a redirect: a correction to a run that is
// already going, without touching what it was asked to achieve.
//
// It writes to the same durable record the run is reconciled from, so it works on a run
// this process is not driving: a goal being worked by `flynn serve` in another terminal is
// redirected from here, and the redirect takes effect on the next turn. That is the whole
// reason it edits the goal rather than talking to the run. There is nothing to attach to,
// no session to be inside, and a run nobody is watching is exactly the run worth being able
// to correct.
//
// Amending the objective is the other operation and this is deliberately not it. Rewriting
// the objective changes what done means, and the grader then rules on the amended text, so
// the correction disappears into the definition of success. A redirect leaves the
// definition of success alone and is checked separately: the run cannot report success
// until it has said what it did about each one (goal/steer.go).

// steerRetries is how many times a redirect is re-applied after losing a write race. The
// run's own reconciler writes to the same record every few seconds, so a conflict here is
// ordinary rather than exceptional, and the retry re-reads and re-applies rather than
// overwriting what the reconciler just recorded.
const steerRetries = 3

// dispatchSteer implements `flynn steer <run> "<instruction>"`.
func dispatchSteer(args []string, dataDir string) error {
	return steerRun(os.Stdout, args, dataDir)
}

// steerRun is dispatchSteer with the destination named, so what the command prints is what
// a test reads.
func steerRun(out io.Writer, args []string, dataDir string) error {
	if len(args) < 2 {
		return errors.New(`usage: flynn steer <run-id> "<instruction>"`)
	}
	instruction := strings.TrimSpace(strings.Join(args[1:], " "))
	if instruction == "" {
		return errors.New("a redirect needs something to say: flynn steer <run-id> \"<instruction>\"")
	}

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
	steer, err := applySteer(ctx, store, id, instruction)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "steered %s (%s)\n", args[0], steer.ID)
	_, _ = fmt.Fprintf(out, "  %s\n", steer.Instruction)
	_, _ = fmt.Fprintln(out, "The run keeps its objective and its stop condition, and cannot report success")
	_, _ = fmt.Fprintln(out, "until it has said what it did about this.")
	return nil
}

// applySteer appends one redirect to a goal's spec, re-reading and re-applying if the
// run's own reconciler wrote to the record in between. It returns the redirect as it was
// recorded.
func applySteer(ctx context.Context, store resource.Store, id, instruction string) (goal.Steer, error) {
	var last error
	for range steerRetries {
		r, err := store.GetByID(ctx, id)
		if err != nil {
			return goal.Steer{}, err
		}
		spec, err := goal.DecodeSpec(r)
		if err != nil {
			return goal.Steer{}, err
		}
		status, err := goal.DecodeStatus(r)
		if err != nil {
			return goal.Steer{}, err
		}
		// A run that has settled is past being redirected. Appending here would hold its
		// finished account against an instruction that did not exist when it was written,
		// which stops the run for something nobody could have done anything about. Saying
		// so is more use than doing that.
		if status.Phase == goal.PhaseConverged || status.Phase == goal.PhaseStalled {
			return goal.Steer{}, fmt.Errorf("that run has already finished (%s): a redirect only applies to a run that is still going", strings.ToLower(string(status.Phase)))
		}
		steer := goal.Steer{ID: "steer-" + strconv.Itoa(len(spec.Steers)+1), Instruction: instruction}
		spec.Steers = append(spec.Steers, steer)
		if err := goal.ValidateSteers(spec.Steers); err != nil {
			return goal.Steer{}, err
		}
		raw, err := json.Marshal(spec)
		if err != nil {
			return goal.Steer{}, err
		}
		r.Spec = raw
		if _, err := store.Put(ctx, r); err == nil {
			return steer, nil
		} else if !errors.Is(err, resource.ErrConflict) {
			return goal.Steer{}, err
		} else {
			last = err
		}
	}
	return goal.Steer{}, fmt.Errorf("the run is being written to faster than the redirect can be applied: %w", last)
}
