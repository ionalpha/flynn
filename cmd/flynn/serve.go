package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/ionalpha/flynn/bindguard"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/source/signalcli"
	"github.com/ionalpha/flynn/source/telegram"
)

// runServe runs the agent as a long-lived service. It answers messages from chat
// channels (each inbound message is recorded as an entry, triaged, driven as a goal
// in the working directory, and answered on the same conversation) and/or exposes
// the read-only control-plane API for remote monitoring. Telegram and Signal are
// the available channels today; the triage boundary accepts more sources as
// adapters are added. Goals run with the full sandboxed toolset under the run's
// budget; the learning loop is not yet wired into the served path.
//
// It blocks until interrupted (Ctrl-C), then stops the control loops.
func runServe(args []string, modelSpec, dataDir string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	tgToken := fs.String("telegram-token", "", "Telegram bot token (or set TELEGRAM_BOT_TOKEN)")
	signalTCP := fs.String("signal-tcp", "", "signal-cli JSON-RPC daemon address, e.g. 127.0.0.1:7583")
	apiAddr := fs.String("api-addr", "", "expose the read-only control-plane API here, loopback recommended, e.g. 127.0.0.1:7575")
	apiToken := fs.String("api-token", "", "bearer token for the control-plane API (or set FLYNN_API_TOKEN)")
	apiExpose := fs.Bool("api-expose", false, "allow --api-addr to bind a non-loopback interface (off by default; prefer a tunnel to a loopback bind, never a wildcard)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	token := *tgToken
	if token == "" {
		token = os.Getenv("TELEGRAM_BOT_TOKEN")
	}
	apiTok := *apiToken
	if apiTok == "" {
		apiTok = os.Getenv("FLYNN_API_TOKEN")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

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

	if len(sources) == 0 && *apiAddr == "" {
		return errors.New("serve: nothing to do; configure a channel (--telegram-token / --signal-tcp) and/or the API (--api-addr)")
	}

	// Read-only control-plane API (optional). Auth is on by default: a supplied token
	// authenticates the operator, and when none is supplied one is generated and printed
	// once rather than serving openly, so the API is secured-by-default with zero config
	// and there is never a reason to run it unauthenticated.
	if *apiAddr != "" {
		var auth controlplane.Authenticator
		if apiTok != "" {
			auth = controlplane.NewTokenAuthenticator(map[string]controlplane.Principal{
				apiTok: {ID: "operator", Scope: controlplane.ScopeRead},
			})
		} else {
			gen, tok, err := controlplane.GeneratedOperator("operator", controlplane.ScopeRead, ids.Token)
			if err != nil {
				return fmt.Errorf("serve: api: generate token: %w", err)
			}
			auth = gen
			fmt.Fprintln(os.Stderr, "flynn serve: no --api-token given; generated one for this run:")
			fmt.Fprintln(os.Stderr, "  FLYNN_API_TOKEN="+tok)
			fmt.Fprintln(os.Stderr, "  present it as: Authorization: Bearer "+tok)
		}
		api := controlplane.NewServer(rstore, store.Log(), auth)
		// Bind-safe by default: the listener is opened through the inbound gate, which
		// refuses a wildcard bind outright and a non-loopback bind unless --api-expose
		// was passed. The bind is checked before the socket opens, so an unsafe address
		// fails closed.
		exposure := bindguard.Loopback()
		if *apiExpose {
			exposure = bindguard.Exposed()
		}
		ln, err := bindguard.Listen("tcp", *apiAddr, exposure)
		if err != nil {
			return fmt.Errorf("serve: api: %w", err)
		}
		httpSrv := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(sc)
		}()
		go func() {
			if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintln(os.Stderr, "serve: api:", err)
			}
		}()
		fmt.Fprintln(os.Stderr, "flynn serve: control-plane API (read-only) on", ln.Addr())
	}

	// With no channels this is a monitor-only daemon: just hold the API open.
	if len(sources) == 0 {
		fmt.Fprintln(os.Stderr, "flynn serve: monitor-only; press Ctrl-C to stop")
		<-ctx.Done()
		return nil
	}

	// Channels need a model and the goal runtime that executes a triaged entry.
	model, plan, err := resolveModelOrOnboard(ctx, modelSpec, dataDir)
	if err != nil {
		return err
	}
	workdir, err := os.Getwd()
	if err != nil {
		return err
	}
	mr, err := assembleMission(model, plan, workdir, "", rstore, store.Jobs(), store.Log(), "")
	if err != nil {
		return err
	}
	rt := mr.rt

	// Triage turns each recorded entry into a goal and replies with its answer on the
	// channel it arrived from.
	worker := &goalWorker{rt: rt, store: rstore}
	triage := inbox.NewTriage(rstore, worker, inbox.NewSinks(sinks...), clock.System{})
	mgr := reconcile.NewManager(rstore)
	mgr.Register(inbox.Kind, triage)

	// Ingest records inbound messages from every source and enqueues them for triage.
	ingest := inbox.NewIngest(rstore, mgr, clock.System{}, sources,
		inbox.WithIngestErrorHandler(func(e error) { fmt.Fprintln(os.Stderr, "serve:", e) }))

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
