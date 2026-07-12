package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/watch"
)

// watchPoll is how often watch mode samples the working tree for new markers.
// Polling (rather than an OS file-notification dependency) matches how the shell
// samples terminal size, and keeps the watcher deterministic under a Manual clock in
// tests. Half a second is well below a human edit-save cadence and cheap over a tree
// the size of a source checkout.
const watchPoll = 500 * time.Millisecond

// runWatch runs the watch loop: it tails the working tree for ai! and ai? markers
// left in trailing comments and drives each one as a governed run, recording the
// marker's file and line onto the run so the request's provenance lands in the sealed
// record like any other input. The marker is cleared from its file once picked up, so
// the same request never fires twice. Ctrl-C stops watching and cancels an in-flight
// run. Learning is captured per run unless disabled, exactly as a one-shot goal.
func runWatch(modelSpec, dataDir string, learnEnabled, verbose bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := errExternalAgentUnsupported("watch", modelSpec); err != nil {
		return err
	}
	model, plan, _, err := resolveModelOrOnboard(ctx, modelSpec, modelSpecExplicit, dataDir)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	// Seal each run under the instance identity when one is available, matching a
	// one-shot goal: without a key the runs proceed unsigned rather than failing.
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

	out := &syncWriter{w: os.Stdout}
	_, _ = fmt.Fprintf(out, "watching %s for ai!/ai? markers; Ctrl-C to stop\n", cwd)

	handle := func(m watch.Marker) error {
		_, _ = fmt.Fprintf(out, "\n  marker %s\n  %s\n", m.Provenance(), m.Text)
		objective := watchObjective(m)
		if _, err := runLearningMission(ctx, out, model, plan, distiller, cwd, objective, "", store, signer, verbose, nil); err != nil {
			// Surface the failure and leave the marker in place (unretried until edited),
			// so a transient run error does not silently swallow the request.
			_, _ = fmt.Fprintf(out, "  marker run failed: %v\n", err)
			return err
		}
		return nil
	}

	w := watch.Start(clock.System{}, cwd, watchPoll, handle)
	defer w.Stop()
	<-ctx.Done()
	_, _ = fmt.Fprintln(out, "\nwatch stopped.")
	return nil
}

// watchObjective composes the run objective for a marker. The marker's provenance
// (file:line and kind) is written into the objective itself, so it is carried on the
// run's opening event and sealed into the record: a later replay or verify shows
// exactly where the request came from. An ai! marker asks the agent to change the
// code in place; an ai? marker asks it to answer without editing.
func watchObjective(m watch.Marker) string {
	var b []byte
	w := func(s string) { b = append(b, s...) }
	switch m.Kind {
	case watch.Ask:
		w("A watch marker asks a question about the code in this working tree. Answer it; do not edit files unless the question asks you to.\n\n")
	default:
		w("A watch marker was left in this working tree. Do what it asks, editing the code in place.\n\n")
	}
	w("Source: " + m.Provenance() + "\n")
	if m.Code != "" {
		w("On that line:\n    " + m.Code + "\n")
	}
	w("\nRequest: " + m.Text)
	return string(b)
}
