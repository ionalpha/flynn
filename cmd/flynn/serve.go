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

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/internal/bindguard"
	"github.com/ionalpha/flynn/internal/exposure"
	"github.com/ionalpha/flynn/internal/instance"
	"github.com/ionalpha/flynn/internal/ops"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/internal/source/signalcli"
	"github.com/ionalpha/flynn/internal/source/telegram"
	"github.com/ionalpha/flynn/internal/version"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/spine"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runServeContext(ctx, args, modelSpec, dataDir)
}

// runServeContext is runServe with the shutdown signal supplied rather than installed, so the
// server's whole lifecycle (bring-up, bind, shutdown) is drivable without sending the
// process a signal. Cancelling ctx stops it exactly as Ctrl-C does.
func runServeContext(ctx context.Context, args []string, modelSpec, dataDir string) error {
	cfg, err := parseServeConfig(args)
	if err != nil {
		return err
	}
	if err := errExternalAgentUnsupported("serve", modelSpec); err != nil {
		return err
	}

	// Load the instance signer so the served spine is durably chained and its resource
	// snapshots are verified under the same key. Best effort: without an identity the
	// server runs unsigned (no chain, unverified snapshots) rather than failing to start.
	signer, serr := runSigner(ctx, dataDir)
	if serr != nil {
		signer = nil
	}
	store, err := openDataStore(ctx, dataDir, snapshotOptions(signer)...)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// Durably record the served spine into a per-stream RFC 6962 Merkle log with periodic
	// signed checkpoints, so a long-lived server's proof material stays bounded and every
	// recorded stream can be verified after a restart. Recording is best effort and never
	// fails an append; the tip of each stream is sealed on shutdown.
	servedLog := store.Log()
	if signer != nil {
		rec := chain.NewDurableRecorder(store.Log(),
			func(s string) chain.FlushNodeStore { return store.MerkleNodes(s) },
			store, signer, nil, checkpointEvery()).
			OnError(func(e error) { fmt.Fprintln(os.Stderr, "serve: chain:", e) })
		servedLog = rec
		defer func() {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if cerr := rec.CheckpointAll(cctx); cerr != nil {
				fmt.Fprintln(os.Stderr, "serve: checkpoint on shutdown:", cerr)
			}
		}()
	}

	reg, err := missionRegistry()
	if err != nil {
		return err
	}
	rstore := store.Resources(reg)

	// Keep this process's Instance record live for the read surface. The heartbeat
	// registers the process on start and rewrites its state and active runs on an
	// interval, so flynn ps/status, the API, and the dashboard show a real,
	// heartbeat-tracked process rather than a record that only updates when observed
	// and always reads Idle. A stopped process records a terminal Done; a crashed one
	// stops beating and the effective-state rule reports it Unknown. It runs for both
	// the monitor-only and channel modes below, so it is started here once.
	hostname, _ := os.Hostname()
	hb := instance.NewHeartbeat(rstore, resource.Scope{}, store.InstanceID(),
		instance.Spec{Host: hostname, Version: version.String()},
		instanceReporter(rstore, store.InstanceID()), clock.System{},
		instance.WithErrorHandler(func(err error) { fmt.Fprintln(os.Stderr, "serve: heartbeat:", err) }))
	go func() { _ = hb.Run(ctx) }()

	// Supervise deployed workloads. A Service registered by `flynn deploy` is held in
	// its desired state by a level-triggered loop: a running service is re-observed
	// through its provider and a service marked stopped is torn down and retired. This
	// runs in every mode, channels or monitor-only, because a deployed app must be
	// supervised regardless of whether this process also answers messages.
	_, opsLoader, err := wireExtensions(rstore, dataDir)
	if err != nil {
		return err
	}
	supervisor := service.NewSupervisor(service.NewStore(rstore), ops.NewDriver(rstore, opsLoader))
	svcMgr := reconcile.NewManager(rstore)
	svcMgr.Register(service.Kind, supervisor)
	go func() { svcMgr.Start(ctx) }()

	// Assemble the configured channels as inbox sources and sinks.
	var sources []inbox.Source
	var sinks []inbox.Sink
	if cfg.telegramToken != "" {
		bot, err := telegram.New(cfg.telegramToken)
		if err != nil {
			return err
		}
		sources = append(sources, bot)
		sinks = append(sinks, bot)
	}
	if cfg.signalTCP != "" {
		sig, err := signalcli.New(cfg.signalTCP)
		if err != nil {
			return err
		}
		sources = append(sources, sig)
		sinks = append(sinks, sig)
	}

	if len(sources) == 0 && cfg.apiAddr == "" {
		return errors.New("serve: nothing to do; configure a channel (--telegram-token / --signal-tcp) and/or the API (--api-addr)")
	}

	if cfg.apiAddr != "" {
		if err := startControlPlaneAPI(ctx, rstore, servedLog, cfg); err != nil {
			return err
		}
	}

	// With no channels this is a monitor-only daemon: just hold the API open.
	if len(sources) == 0 {
		fmt.Fprintln(os.Stderr, "flynn serve: monitor-only; press Ctrl-C to stop")
		<-ctx.Done()
		return nil
	}

	return serveChannels(ctx, modelSpec, dataDir, rstore, store.Jobs(), servedLog, sources, sinks)
}

// serveConfig is the parsed `flynn serve` configuration: the channel credentials and the
// control-plane API's address, its auth (a static token or a delegation issuer key), and its
// exposure. A flag wins over its environment fallback.
type serveConfig struct {
	telegramToken string
	signalTCP     string
	apiAddr       string
	apiToken      string
	apiIssuer     string
	apiExpose     bool
}

// parseServeConfig parses the serve subcommand's flags and folds in the environment fallbacks
// (TELEGRAM_BOT_TOKEN, FLYNN_API_TOKEN, FLYNN_API_ISSUER), so the rest of bring-up reads one
// resolved config. A static token and an issuer key are two different trust models, so supplying
// both is refused here rather than letting one silently win.
func parseServeConfig(args []string) (serveConfig, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	tgToken := fs.String("telegram-token", "", "Telegram bot token (or set TELEGRAM_BOT_TOKEN)")
	signalTCP := fs.String("signal-tcp", "", "signal-cli JSON-RPC daemon address, e.g. 127.0.0.1:7583")
	apiAddr := fs.String("api-addr", "", "expose the read-only control-plane API here, loopback recommended, e.g. 127.0.0.1:7575")
	apiToken := fs.String("api-token", "", "bearer token for the control-plane API (or set FLYNN_API_TOKEN)")
	apiIssuer := fs.String("api-issuer", "", "accept delegated capability tokens issued under this operator public key (ed25519:...); enables scope-attenuated remote drive instead of a static read-only token")
	apiExpose := fs.Bool("api-expose", false, "allow --api-addr to bind a non-loopback interface (off by default; prefer a tunnel to a loopback bind, never a wildcard)")
	if err := fs.Parse(args); err != nil {
		return serveConfig{}, err
	}

	// A flag wins over its environment fallback; neither channel nor API auth is required here,
	// the caller decides what "nothing to do" means once every source is assembled.
	token := *tgToken
	if token == "" {
		token = os.Getenv("TELEGRAM_BOT_TOKEN")
	}
	apiTok := *apiToken
	if apiTok == "" {
		apiTok = os.Getenv("FLYNN_API_TOKEN")
	}
	apiIss := *apiIssuer
	if apiIss == "" {
		apiIss = os.Getenv("FLYNN_API_ISSUER")
	}
	if apiIss != "" && apiTok != "" {
		return serveConfig{}, errors.New("serve: --api-token and --api-issuer are mutually exclusive: a static bearer is capped at read scope, an issuer key accepts scope-attenuated delegated tokens; choose one trust model")
	}
	return serveConfig{
		telegramToken: token,
		signalTCP:     *signalTCP,
		apiAddr:       *apiAddr,
		apiToken:      apiTok,
		apiIssuer:     apiIss,
		apiExpose:     *apiExpose,
	}, nil
}

// startControlPlaneAPI opens the read-only control-plane API on cfg.apiAddr and serves it until
// ctx is cancelled. Auth is on by default: a supplied token authenticates the operator, and when
// none is supplied one is generated and printed once rather than serving openly, so the API is
// secured-by-default with zero config and there is never a reason to run it unauthenticated.
func startControlPlaneAPI(ctx context.Context, rstore resource.Store, servedLog spine.Log, cfg serveConfig) error {
	var auth controlplane.Authenticator
	if cfg.apiIssuer != "" {
		// Delegation trust: the box holds no secret, only the operator's public key. The operator
		// (which enrolled this instance) mints scope-attenuated capability tokens under its issuer
		// key and presents them per request; each is verified offline against this key, fail-closed
		// on any forged, widened, or expired chain. This is the path that permits operator-scoped
		// remote drive (pause/resume/halt/run): a static --api-token is deliberately capped at read
		// scope, a delegated token carries exactly the scope and actions the operator attenuated to.
		issuer, err := controlplane.ParsePrincipalID(cfg.apiIssuer)
		if err != nil {
			return fmt.Errorf("serve: api: --api-issuer: %w", err)
		}
		auth = controlplane.NewDelegationAuthenticator(issuer, clock.System{})
	} else if cfg.apiToken != "" {
		// A local operator token is not grant-attenuated: it carries full action authority
		// (AllowAll), bounded only by its scope, so the action gate reduces to the instance's own
		// local grant. The zero Grant would instead deny-all.
		auth = controlplane.NewTokenAuthenticator(map[string]controlplane.Principal{
			cfg.apiToken: {ID: "operator", Scope: controlplane.ScopeRead, Grant: capability.AllowAll()},
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
	api := controlplane.NewServer(rstore, servedLog, auth)
	// Bind-safe by default: the listener is opened through the inbound gate, which refuses a
	// wildcard bind outright and a non-loopback bind unless --api-expose was passed. The bind is
	// checked before the socket opens, so an unsafe address fails closed. It is opened through the
	// exposure registry so the listener is recorded and visible (nothing stays exposed silently);
	// the API is long-lived, so it carries no TTL.
	reach := bindguard.Loopback()
	if cfg.apiExpose {
		reach = bindguard.Exposed()
	}
	exposures := exposure.New(clock.System{}, nil)
	ln, err := exposures.Listen("tcp", cfg.apiAddr, reach, exposure.Meta{Purpose: "control-plane API", Exposed: cfg.apiExpose})
	if err != nil {
		return fmt.Errorf("serve: api: %w", err)
	}
	httpSrv := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
	// The drain deliberately does not inherit ctx: it starts once ctx is done, and a shutdown on
	// an already-cancelled context would close every live connection instantly instead of letting
	// the requests in flight finish.
	go func() { //nolint:gosec // G118: the shutdown deadline cannot descend from the context that triggered it
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
	return nil
}

// serveChannels drives the message-answering path: it resolves the model and goal runtime, then
// records every inbound message from sources, triages each into a goal run in the working
// directory, and replies with the answer on the channel it arrived from. It blocks until ctx is
// cancelled.
func serveChannels(ctx context.Context, modelSpec, dataDir string, rstore resource.Store, jq jobs.Queue, servedLog spine.Log, sources []inbox.Source, sinks []inbox.Sink) error {
	// Channels need a model and the goal runtime that executes a triaged entry.
	model, plan, _, err := resolveModelOrOnboard(ctx, modelSpec, modelSpecExplicit, dataDir)
	if err != nil {
		return err
	}
	workdir, err := os.Getwd()
	if err != nil {
		return err
	}
	mr, err := assembleMission(model, plan, workdir, "", rstore, jq, servedLog, "", sandbox.ResourceLimits{}, false, false)
	if err != nil {
		return err
	}
	defer func() { _ = mr.Close() }()
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

// instanceReporter derives this process's live run-state from the goals it owns. A
// goal still Pending or Running counts as active work, so any active goal makes the
// instance Working and its names are the active runs; with none the instance is
// Idle. Ownership is the goal's creator (OriginInstanceID), so on a multi-instance
// store each process reports only its own runs; a blank origin (single-instance or
// an older record) is treated as local so nothing is dropped on one box. A store
// read error reports Unknown rather than guessing Idle, which would hide live work.
// Blocked is reserved for a future waiting signal (a run halted on approval or
// input); goals expose no such phase today, so the reporter never invents it.
func instanceReporter(store resource.Store, instanceID string) instance.Reporter {
	return func(ctx context.Context) (instance.State, []string) {
		rs, err := store.ListAll(ctx, goal.Kind, nil)
		if err != nil {
			return instance.StateUnknown, nil
		}
		var active []string
		for _, r := range rs {
			if r.OriginInstanceID != "" && r.OriginInstanceID != instanceID {
				continue
			}
			st, err := goal.DecodeStatus(r)
			if err != nil {
				continue
			}
			switch st.Phase {
			case goal.PhasePending, goal.PhasePlanning, goal.PhaseRunning:
				active = append(active, r.Name)
			case goal.PhaseConverged, goal.PhaseStalled:
				// Terminal: the goal is finished, so it is not active work.
			}
		}
		if len(active) > 0 {
			return instance.StateWorking, active
		}
		return instance.StateIdle, nil
	}
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
