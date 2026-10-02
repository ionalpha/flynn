package main

// The session's front door: assembling a replSession from the flags, resuming a prior
// run when the user picks one, and choosing which of the two interfaces runs it.

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
)

// runInteractive runs the no-subcommand interactive session: a read-eval-print loop
// where each line the user types is a turn of one durable conversation with the
// agent. The first line opens a run; every later line continues the same run, so the
// model is handed the whole history and the conversation stays addressable by a
// single id for replay and audit. Ctrl-C cancels the in-flight turn without ending
// the session; Ctrl-D or "exit" leaves, after which a learning pass distills the
// session (unless learning is disabled). It assumes stdin is a terminal; the caller
// falls back to usage when it is not. By default it runs the full-screen interface;
// plain (or a non-terminal stdout) selects the line-based interface instead.
func runInteractive(modelSpec, dataDir string, learnEnabled, verbose, plain bool, reqApproval []string) error {
	ctx := context.Background()

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	// The session drives either a model conversation or an external agent CLI. An
	// external backend brings its own harness, so it resolves to a driver rather than an
	// llm.Model: the turns still land on one durable run, sealed the same way, but each
	// turn is an episode of the CLI's own conversation instead of a step of ours.
	var (
		model    llm.Model
		plan     harness.Plan
		extAgent *externAgent
	)
	if name, cliModel, ok := externalAgentSpec(modelSpec); ok {
		extAgent, err = resolveExternalAgent(ctx, name, cliModel, cwd)
		if err != nil {
			return err
		}
		// The harness's home outlives its episodes: it is where the CLI keeps the conversation
		// each turn continues. It dies with the session.
		defer extAgent.close()
	} else {
		var resolvedSpec string
		model, plan, resolvedSpec, err = resolveModelOrOnboard(ctx, modelSpec, modelSpecExplicit, dataDir)
		if err != nil {
			return err
		}
		// Report and drive the model that actually resolved, not the default spec that
		// started resolution: a fallback to an already-configured provider changes it.
		modelSpec = resolvedSpec
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
	// Load the instance signer so the session can seal its run into a verifiable record
	// on demand. Best effort, exactly as the one-shot runner treats it: a session
	// without a key still runs every turn; only /seal reports the key is missing.
	signer, serr := runSigner(ctx, dataDir)
	if serr != nil {
		signer = nil
	}

	// Learning distills a session through a model. An external agent exposes no model of
	// its own (its inner loop is unobserved), so a session it drives does not distill:
	// the run is still sealed and verifiable, only the learn-back step is skipped. This
	// mirrors what a one-shot external run does.
	var distiller learn.Distiller
	if learnEnabled && extAgent == nil {
		distiller = governedDistiller(model)
	}

	keys, err := loadKeymap(dataDir)
	if err != nil {
		return err
	}
	th, err := loadTheme(dataDir)
	if err != nil {
		return err
	}

	s := &replSession{
		keys:         keys,
		theme:        th,
		out:          &syncWriter{w: os.Stdout},
		model:        model,
		plan:         plan,
		ext:          extAgent,
		distiller:    distiller,
		learnEnabled: learnEnabled,
		verbose:      verbose,
		cwd:          cwd,
		store:        store,
		reg:          reg,
		signer:       signer,
		dataDir:      dataDir,
		modelSpec:    modelSpec,
		// The policy is the session's for its whole life; the prompter that resolves a
		// pause is installed by whichever interface runs, because only one of them can ask.
		gates: gateSetup{approve: reqApproval},
	}

	// Every turn runs under a prime scope, so a memory the wake digest pushed is
	// attributed as primed when it is used rather than as the session having gone
	// and found it. It is the session's, not the turn's: the digest is built once at
	// the opening line and what it primed stays primed for the conversation.
	ctx = wakeContext(ctx)

	// Front door: when prior runs exist, let the user resume one or start fresh. A
	// resumed run is seeded so the session continues the same durable conversation.
	var seed string
	if stdinIsTerminal() {
		id, history, lastSeq, perr := pickSession(ctx, store, reg, verbose)
		if perr != nil {
			return perr
		}
		if id != "" {
			s.started = true
			s.runID = id
			s.system = defaultSystemPrompt
			s.lastSeq = lastSeq
			seed = history
		}
	}

	if plain || !stdoutIsTerminal() {
		if seed != "" {
			_, _ = fmt.Fprint(s.out, seed)
		}
		return s.runLineMode(ctx, cwd)
	}
	return runInteractiveTUI(ctx, s)
}

// runLineMode is the line-based session: a terminal reader giving line editing,
// history, and bracketed paste, entering raw mode only while reading a line. Ctrl-C
// cancels the in-flight turn only (the signal is delivered while a turn runs, when
// the terminal is back in its normal mode); the session survives, so a runaway turn
// is interruptible without losing the conversation.
func (s *replSession) runLineMode(ctx context.Context, cwd string) error {
	in := newTermReader(stdio{os.Stdin, os.Stdout}, int(os.Stdin.Fd()), "flynn> ")
	defer in.Close()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	s.notice = func(text string) { _, _ = fmt.Fprintf(s.out, "  %s\n", text) }
	_, _ = fmt.Fprintf(s.out, "flynn interactive session in %s (model: %s)\n", cwd, s.modelSpec)
	_, _ = fmt.Fprintln(s.out, `type a message and press enter; /models to list models, /model <id> to switch; Ctrl-C cancels a turn, Ctrl-D or "exit" leaves.`)
	return s.loop(ctx, in, sigCh)
}

// stdinIsTerminal reports whether standard input is an interactive terminal, so the
// no-subcommand invocation starts a REPL only when there is a human to prompt and
// falls back to usage when stdin is a pipe or file (a script, a CI step).
func stdinIsTerminal() bool {
	return isCharDevice(os.Stdin)
}

// stdoutIsTerminal reports whether standard output is an interactive terminal, so
// the full-screen interface is used only when it can render and the line-based one is
// chosen when output is redirected.
func stdoutIsTerminal() bool {
	return isCharDevice(os.Stdout)
}

func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
