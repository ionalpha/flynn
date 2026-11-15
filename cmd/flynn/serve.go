package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/instance"
	"github.com/ionalpha/flynn/internal/bindguard"
	"github.com/ionalpha/flynn/internal/exposure"
	"github.com/ionalpha/flynn/internal/ops"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/internal/source/signalcli"
	"github.com/ionalpha/flynn/internal/source/telegram"
	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/internal/version"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/spine"
)

// runServe runs the agent as a service. It answers chat channels (Telegram and Signal):
// each message from an allowed sender is recorded as an entry, triaged, driven as a
// goal in the working directory and answered on its conversation. It can also expose
// the read-only control-plane API. Goals get the full sandboxed toolset, the approval
// and allowance gates of a terminal run, and a per-goal budget; the served path does
// not learn.
//
// It blocks until interrupted (Ctrl-C), then stops the control loops.
func runServe(args []string, modelSpec, dataDir string, gov serveGovernance) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runServeContext(ctx, args, modelSpec, dataDir, gov)
}

// runServeContext is runServe with the shutdown signal supplied, so tests can drive the
// whole lifecycle. Cancelling ctx stops it exactly as Ctrl-C does.
func runServeContext(ctx context.Context, args []string, modelSpec, dataDir string, gov serveGovernance) error {
	cfg, err := parseServeConfig(args)
	if err != nil {
		return err
	}
	if err := errExternalAgentUnsupported("serve", modelSpec); err != nil {
		return err
	}

	// Best effort: without an identity the server runs unsigned, with no chain and
	// unverified snapshots.
	signer, serr := runSigner(ctx, dataDir)
	if serr != nil {
		signer = nil
	}
	store, err := openDataStore(ctx, dataDir, snapshotOptions(signer)...)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// Each served stream is recorded into an RFC 6962 Merkle log with periodic signed
	// checkpoints, so proof material stays bounded and streams verify after a restart.
	// Recording never fails an append; each stream's tip is sealed on shutdown.
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

	reg, err := governed.Registry()
	if err != nil {
		return err
	}
	rstore := store.Resources(reg)

	// The loops below write to the store, so they stop before it closes on every return
	// path. Otherwise a later refusal (an unsafe bind, nothing configured) leaves them
	// running against a closed store after serve returns: a database file Windows will not
	// remove, and goroutines an embedding host never gets back. Deferred after the close,
	// so it runs first.
	loopCtx, stopLoops := context.WithCancel(ctx)
	var loops sync.WaitGroup
	defer func() {
		stopLoops()
		loops.Wait()
	}()

	// The heartbeat registers this process and rewrites its state and active runs on an
	// interval, so flynn ps/status, the API and the dashboard show a tracked process. A
	// stopped process records Done; a crashed one stops beating and reads Unknown. Both
	// modes below need it.
	hostname, _ := os.Hostname()
	hb := instance.NewHeartbeat(rstore, resource.Scope{}, store.InstanceID(),
		instance.Spec{Host: hostname, Version: version.String()},
		instanceReporter(rstore, store.InstanceID()), clock.System{},
		instance.WithErrorHandler(func(err error) { fmt.Fprintln(os.Stderr, "serve: heartbeat:", err) }))
	loops.Go(func() { _ = hb.Run(loopCtx) })

	// `flynn deploy` services are held in their desired state by a level-triggered loop: a
	// running service is re-observed through its provider, and a stopped one is torn down
	// and retired. It runs in every mode, because a deployed app needs supervising whether
	// or not this process answers messages.
	opsLoader, err := governed.OpenIntegrations(rstore, vault.New(dataDir, vault.WithPassphrase(terminalPassphrase)))
	if err != nil {
		return err
	}
	supervisor := service.NewSupervisor(service.NewStore(rstore), ops.NewDriver(rstore, opsLoader))
	svcMgr := reconcile.NewManager(rstore)
	svcMgr.Register(service.Kind, supervisor)
	loops.Go(func() { svcMgr.Start(loopCtx) })

	var sources []inbox.Source
	var sinks []inbox.Sink
	allow := senderAllowList{}
	if cfg.telegramToken != "" {
		bot, err := telegram.New(cfg.telegramToken)
		if err != nil {
			return err
		}
		sources = append(sources, bot)
		sinks = append(sinks, bot)
		allow.add(bot.Name(), cfg.telegramAllow)
	}
	if cfg.signalTCP != "" {
		sig, err := signalcli.New(cfg.signalTCP)
		if err != nil {
			return err
		}
		sources = append(sources, sig)
		sinks = append(sinks, sig)
		allow.add(sig.Name(), cfg.signalAllow)
	}

	if len(sources) == 0 && cfg.apiAddr == "" {
		return errors.New("serve: nothing to do; configure a channel (--telegram-token / --signal-tcp) and/or the API (--api-addr)")
	}

	if cfg.apiAddr != "" {
		if err := startControlPlaneAPI(ctx, rstore, servedLog, cfg); err != nil {
			return err
		}
	}

	// No channels: a monitor-only daemon that holds the API open.
	if len(sources) == 0 {
		fmt.Fprintln(os.Stderr, "flynn serve: monitor-only; press Ctrl-C to stop")
		<-ctx.Done()
		return nil
	}

	return serveChannels(ctx, modelSpec, dataDir, rstore, store.Jobs(), servedLog, sources, sinks, allow, gov)
}

// serveConfig is the parsed `flynn serve` configuration. A flag wins over its
// environment fallback.
//
// parseServeConfig refuses a channel with no allowed senders: the sender writes the
// goal's objective, so an open channel hands the agent's tools to anyone who finds the
// bot.
type serveConfig struct {
	telegramToken string
	telegramAllow []string
	signalTCP     string
	signalAllow   []string
	apiAddr       string
	apiToken      string
	apiIssuer     string
	apiExpose     bool
}

// parseServeConfig folds in TELEGRAM_BOT_TOKEN, FLYNN_API_TOKEN and FLYNN_API_ISSUER. A
// static token and an issuer key are different trust models, so both together are
// refused.
func parseServeConfig(args []string) (serveConfig, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	tgToken := fs.String("telegram-token", "", "Telegram bot token (or set TELEGRAM_BOT_TOKEN)")
	tgAllow := &stringList{}
	fs.Var(tgAllow, "telegram-allow", "Telegram user id allowed to start a run (repeat for more, or set FLYNN_TELEGRAM_ALLOW to a comma separated list); required with --telegram-token. Numeric ids only: a username can be changed or reassigned.")
	signalTCP := fs.String("signal-tcp", "", "signal-cli JSON-RPC daemon address, e.g. 127.0.0.1:7583")
	sigAllow := &stringList{}
	fs.Var(sigAllow, "signal-allow", "Signal number (+15551234567) allowed to start a run (repeat for more, or set FLYNN_SIGNAL_ALLOW to a comma separated list); required with --signal-tcp.")
	apiAddr := fs.String("api-addr", "", "expose the read-only control-plane API here, loopback recommended, e.g. 127.0.0.1:7575")
	apiToken := fs.String("api-token", "", "bearer token for the control-plane API (or set FLYNN_API_TOKEN)")
	apiIssuer := fs.String("api-issuer", "", "accept delegated capability tokens issued under this operator public key (ed25519:...); enables scope-attenuated remote drive instead of a static read-only token")
	apiExpose := fs.Bool("api-expose", false, "allow --api-addr to bind a non-loopback interface (off by default; prefer a tunnel to a loopback bind, never a wildcard)")
	if err := fs.Parse(args); err != nil {
		return serveConfig{}, err
	}

	// Neither a channel nor API auth is required here; the caller decides what "nothing to
	// do" means.
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
	cfg := serveConfig{
		telegramToken: token,
		signalTCP:     *signalTCP,
		apiAddr:       *apiAddr,
		apiToken:      apiTok,
		apiIssuer:     apiIss,
		apiExpose:     *apiExpose,
	}
	var err error
	if cfg.telegramAllow, err = channelAllow("telegram", token != "", tgAllow.values, "FLYNN_TELEGRAM_ALLOW", parseTelegramAllow); err != nil {
		return serveConfig{}, err
	}
	if cfg.signalAllow, err = channelAllow("signal", *signalTCP != "", sigAllow.values, "FLYNN_SIGNAL_ALLOW", parseSignalAllow); err != nil {
		return serveConfig{}, err
	}
	return cfg, nil
}

// channelAllow returns the flag's senders, or the environment's when the flag is
// absent. A configured channel with none is refused by name: no default admits
// everyone. An allow-list for an unconfigured channel is ignored.
func channelAllow(channel string, configured bool, flagValues []string, env string, parse func([]string) ([]string, error)) ([]string, error) {
	if !configured {
		return nil, nil
	}
	values := flagValues
	if len(values) == 0 {
		values = splitList(os.Getenv(env))
	}
	ids, err := parse(values)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("serve: the %s channel needs an allow-list: name who may start a run with --%s-allow (or %s); a message from anyone else would run with this agent's tools", channel, channel, env)
	}
	return ids, nil
}

// startControlPlaneAPI serves the read-only control-plane API on cfg.apiAddr until ctx
// is cancelled. With no token supplied it generates one and prints it once, so the API
// never runs unauthenticated.
func startControlPlaneAPI(ctx context.Context, rstore resource.Store, servedLog spine.Log, cfg serveConfig) error {
	var auth controlplane.Authenticator
	if cfg.apiIssuer != "" {
		// Delegation: the box holds only the operator's public key. The operator mints
		// scope-attenuated capability tokens under its issuer key, each verified offline and
		// refused on any forged, widened or expired chain. Only this path permits remote drive
		// (pause/resume/halt/run); a static --api-token is capped at read scope.
		issuer, err := controlplane.ParsePrincipalID(cfg.apiIssuer)
		if err != nil {
			return fmt.Errorf("serve: api: --api-issuer: %w", err)
		}
		auth = controlplane.NewDelegationAuthenticator(issuer, clock.System{})
	} else if cfg.apiToken != "" {
		// A local operator token carries AllowAll bounded by its scope, so the action gate
		// reduces to the instance's own grant. The zero Grant would deny all.
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
	// The inbound gate refuses a wildcard bind and a non-loopback one without
	// --api-expose, before the socket opens. The exposure registry records the listener so
	// nothing stays exposed unseen. The API is long-lived, so it has no TTL.
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
	// The drain starts once ctx is done, so it does not inherit ctx: a shutdown on a
	// cancelled context would drop live connections instead of letting requests finish.
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

// serveChannels resolves the model, records each allowed sender's message from
// sources, triages it into a goal run and replies on the arriving channel. It blocks
// until ctx is cancelled.
func serveChannels(ctx context.Context, modelSpec, dataDir string, rstore resource.Store, jq jobs.Queue, servedLog spine.Log, sources []inbox.Source, sinks []inbox.Sink, allow senderAllowList, gov serveGovernance) error {
	// Channels need a model and the goal runtime that executes a triaged entry.
	model, plan, _, err := resolveModelOrOnboard(ctx, modelSpec, modelSpecExplicit, dataDir)
	if err != nil {
		return err
	}
	workdir, err := os.Getwd()
	if err != nil {
		return err
	}
	return serveIntake{
		model: model, plan: plan, workdir: workdir, rstore: rstore, jq: jq, log: servedLog,
		sources: sources, sinks: sinks, allow: allow, gov: gov, notice: os.Stderr,
	}.run(ctx)
}

// instanceReporter derives this process's run state from the goals it owns. Any Pending
// or Running goal makes it Working with those goals as active runs; otherwise Idle.
// Ownership is OriginInstanceID, so on a shared store each process reports its own; a
// blank origin counts as local. A read error reports Unknown, since guessing Idle would
// hide live work. Blocked is never reported, because goals have no waiting phase.
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
				// Finished goals are not active work.
			}
		}
		if len(active) > 0 {
			return instance.StateWorking, active
		}
		return instance.StateIdle, nil
	}
}

// goalWorker adapts the goal runtime to inbox.Worker: an entry's content becomes a goal,
// and its status is the outcome.
type goalWorker struct {
	rt    *runtime.Runtime
	store resource.Store
	// Each goal gets its own pool. A goal with no pool is unlimited, and a shared pool lets
	// one message spend what the rest were owed.
	budgets *budgetpkg.Ledger
	limits  budgetpkg.Limits
	// allowances are the irreversible actions the operator declared with --allow.
	allowances []goal.Allowance
}

// Start submits the objective as a goal and returns its name. The pool is named after
// the entry, and opening an existing one returns it, so a retried start reuses the pool
// and its spend.
func (w *goalWorker) Start(ctx context.Context, entry, objective string) (string, error) {
	pool := servedPoolPrefix + entry
	if _, err := w.budgets.Open(ctx, pool, resource.Scope{}, w.limits); err != nil {
		return "", fmt.Errorf("serve: open goal budget: %w", err)
	}
	g, err := w.rt.SubmitGoal(ctx, "", goal.Spec{
		Objective:     objective,
		StopCondition: governed.DefaultStopCondition,
		BudgetPool:    pool,
		Allowances:    w.allowances,
	})
	if err != nil {
		return "", err
	}
	return g.Name, nil
}

// Poll reports a terminal phase and the final message; a stalled goal is failed.
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
