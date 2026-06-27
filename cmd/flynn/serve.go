package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/ionalpha/flynn/channel"
	"github.com/ionalpha/flynn/channel/telegram"
	"github.com/ionalpha/flynn/learn"
)

// runServe runs the agent as a long-lived service that answers messages from chat
// channels. Each inbound message is driven as a goal in the working directory and
// the agent's final answer is sent back on the same conversation. Telegram is the
// available channel today; the gateway accepts more as adapters are added.
//
// It blocks until interrupted (Ctrl-C), then drains any in-flight reply before
// returning.
func runServe(args []string, modelSpec, dataDir string, learnEnabled, verbose bool) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	tgToken := fs.String("telegram-token", "", "Telegram bot token (or set TELEGRAM_BOT_TOKEN)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	token := *tgToken
	if token == "" {
		token = os.Getenv("TELEGRAM_BOT_TOKEN")
	}
	if token == "" {
		return errors.New("serve: no channel configured; pass --telegram-token or set TELEGRAM_BOT_TOKEN")
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

	var distiller learn.Distiller
	if learnEnabled {
		distiller = governedDistiller(model)
	}

	tg, err := telegram.New(token)
	if err != nil {
		return err
	}

	runner := channel.RunnerFunc(func(ctx context.Context, _ /* convo */, text string) (string, error) {
		// Each message is one goal driven to its end state; the agent's final answer
		// is the reply. Output is discarded here because the channel, not a terminal,
		// is the surface. The run records under its own id on the shared store, so it
		// is inspectable later with `flynn runs` / `flynn inspect`.
		return runLearningMission(ctx, io.Discard, model, distiller, workdir, text, store, verbose)
	})

	gw := channel.NewGateway(
		runner, []channel.Channel{tg},
		// One goal at a time: the runs share a single database, so serializing keeps
		// the store uncontended. Per-conversation parallelism can lift this later.
		channel.WithConcurrency(1),
		channel.WithErrorHandler(func(e error) { fmt.Fprintln(os.Stderr, "serve:", e) }),
	)

	fmt.Fprintln(os.Stderr, "flynn serve: answering Telegram messages; press Ctrl-C to stop")
	if err := gw.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
