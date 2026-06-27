package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/source/signalcli"
	"github.com/ionalpha/flynn/source/telegram"
)

// runServe runs the agent as a long-lived service that answers messages from chat
// channels. Inbound messages are recorded as entries and triaged: each is driven as
// a goal in the working directory and the agent's final answer is sent back on the
// same conversation. Telegram and Signal are the available channels today; the
// triage boundary accepts more sources as adapters are added. Goals run with the full sandboxed
// toolset under the run's budget; the learning loop is not yet wired into the served
// path, so a message is answered but not distilled into skills.
//
// It blocks until interrupted (Ctrl-C), then stops the control loops.
func runServe(args []string, modelSpec, dataDir string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	tgToken := fs.String("telegram-token", "", "Telegram bot token (or set TELEGRAM_BOT_TOKEN)")
	signalTCP := fs.String("signal-tcp", "", "signal-cli JSON-RPC daemon address, e.g. 127.0.0.1:7583")
	if err := fs.Parse(args); err != nil {
		return err
	}

	token := *tgToken
	if token == "" {
		token = os.Getenv("TELEGRAM_BOT_TOKEN")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	model, err := resolveModelOrOnboard(ctx, modelSpec, dataDir)
	if err != nil {
		return err
	}
	workdir, err := os.Getwd()
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
	rstore := store.Resources(reg)

	// The goal runtime executes the work a triaged entry implies.
	mr, err := assembleMission(model, workdir, "", rstore, store.Jobs(), store.Log(), "")
	if err != nil {
		return err
	}
	rt := mr.rt

	// Assemble the configured channels as inbox sources and sinks.
	var sources []inbox.Source
	var sinks []inbox.Sink
	if token != "" {
		bot, err := telegram.New(token)
		if err != nil {
			return err
		}
		sources = append(sources, bot)
		sinks = append(sinks, bot)
	}
	if *signalTCP != "" {
		sig, err := signalcli.New(*signalTCP)
		if err != nil {
			return err
		}
		sources = append(sources, sig)
		sinks = append(sinks, sig)
	}
	if len(sources) == 0 {
		return errors.New("serve: no channel configured; pass --telegram-token (or TELEGRAM_BOT_TOKEN) and/or --signal-tcp")
	}

	// Triage turns each recorded entry into a goal and replies with its answer on the
	// channel it arrived from.
	worker := &goalWorker{rt: rt, store: rstore}
	triage := inbox.NewTriage(rstore, worker, inbox.NewSinks(sinks...), clock.System{})
	mgr := reconcile.NewManager(rstore)
	mgr.Register(inbox.Kind, triage)

	// Ingest records inbound messages from every source and enqueues them for triage.
	ingest := inbox.NewIngest(rstore, mgr, clock.System{}, sources,
		inbox.WithIngestErrorHandler(func(e error) { fmt.Fprintln(os.Stderr, "serve:", e) }))

	// Run the goal runtime, the triage manager, and ingest together. Ingest blocks
	// until ctx is cancelled; the others stop with it.
	go func() { _ = rt.Start(ctx) }()
	go func() { mgr.Start(ctx) }()

	fmt.Fprintln(os.Stderr, "flynn serve: answering messages; press Ctrl-C to stop")
	if err := ingest.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// goalWorker adapts the goal runtime to the inbox.Worker port: it submits an entry's
// content as a goal and reports the goal's outcome by reading its status.
type goalWorker struct {
	rt    *runtime.Runtime
	store resource.Store
}

// Start submits the objective as a goal and returns the goal's name as the handle.
func (w *goalWorker) Start(ctx context.Context, _, objective string) (string, error) {
	g, err := w.rt.SubmitGoal(ctx, "", goal.Spec{
		Objective:     objective,
		StopCondition: "the objective is fully accomplished",
	})
	if err != nil {
		return "", err
	}
	return g.Name, nil
}

// Poll reports whether the goal has reached a terminal phase and its final message,
// treating a stalled goal as failed.
func (w *goalWorker) Poll(ctx context.Context, handle string) (done bool, answer string, failed bool, err error) {
	r, err := w.store.Get(ctx, goal.Kind, resource.Scope{}, handle)
	if err != nil {
		return false, "", false, err
	}
	st, err := goal.DecodeStatus(r)
	if err != nil {
		return false, "", false, err
	}
	switch st.Phase {
	case goal.PhaseConverged:
		return true, st.Message, false, nil
	case goal.PhaseStalled:
		return true, st.Message, true, nil
	default:
		return false, "", false, nil
	}
}
