package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/storage/sqlite"
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
func runInteractive(modelSpec, dataDir string, learnEnabled, verbose, plain bool) error {
	ctx := context.Background()

	model, err := resolveModelOrOnboard(ctx, modelSpec, dataDir)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
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

	var distiller learn.Distiller
	if learnEnabled {
		distiller = governedDistiller(model)
	}

	s := &replSession{
		out:       &syncWriter{w: os.Stdout},
		model:     model,
		distiller: distiller,
		verbose:   verbose,
		cwd:       cwd,
		store:     store,
		reg:       reg,
	}

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
	return runInteractiveTUI(ctx, s, seed)
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

	_, _ = fmt.Fprintf(s.out, "flynn interactive session in %s\n", cwd)
	_, _ = fmt.Fprintln(s.out, `type a message and press enter; Ctrl-C cancels a turn, Ctrl-D or "exit" leaves.`)
	return s.loop(ctx, in, sigCh)
}

// replSession holds the state of one interactive session across its turns: the
// assembly inputs (model, store, working directory), the identity and cursor of the
// durable run the turns share, and what the session has accumulated for the learning
// pass at the end.
type replSession struct {
	out       io.Writer
	model     llm.Model
	distiller learn.Distiller
	verbose   bool
	cwd       string
	store     *sqlite.Store
	reg       *resource.Registry

	// Per-session run state, set on the first turn and continued by the rest.
	started   bool
	runID     string
	system    string
	objective string
	lastSeq   int64
	converged bool

	recalled   []string
	transcript []llm.Message
	lastResult string
}

// loop is the read-eval-print loop. It reads a message, then drives it as a turn,
// until input ends (Ctrl-D or Ctrl-C at the prompt) or the user types an exit
// command, at which point it runs the session's learning pass and returns. A turn
// error is reported but does not end the session, so a transient failure or a
// cancelled turn returns the user to the prompt.
func (s *replSession) loop(ctx context.Context, in lineReader, sigCh <-chan os.Signal) error {
	for {
		line, err := in.ReadLine()
		if errors.Is(err, io.EOF) {
			_, _ = fmt.Fprintln(s.out)
			return s.finish(ctx)
		}
		if err != nil {
			_, _ = fmt.Fprintf(s.out, "  input error: %v\n", err)
			return s.finish(ctx)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if isExit(line) {
			return s.finish(ctx)
		}
		if _, err := s.runTurn(ctx, line, sigCh); err != nil {
			if errors.Is(err, context.Canceled) {
				_, _ = fmt.Fprintln(s.out, "  (turn cancelled)")
			} else {
				_, _ = fmt.Fprintf(s.out, "  error: %v\n", err)
			}
		}
	}
}

// runTurn drives one user turn to a terminal event, rendering it live. The first
// turn folds recall into the system prompt and submits the line as the session's
// opening goal; every later turn reopens the same durable goal with the new line and
// re-drives it, so the model sees the whole conversation and the run keeps one id. A
// Ctrl-C on sigCh cancels just this turn (a fresh per-turn runtime is bound to a
// cancellable context), leaving the session intact for the next line.
func (s *replSession) runTurn(ctx context.Context, userText string, sigCh <-chan os.Signal) (string, error) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if sigCh != nil {
		watchStop := make(chan struct{})
		defer close(watchStop)
		go func() {
			select {
			case <-sigCh:
				_, _ = fmt.Fprintln(s.out, "  (interrupting turn...)")
				cancel()
			case <-watchStop:
			}
		}()
	}

	if !s.started {
		// Recall once, against the opening line: fold what past runs learned into the
		// standing instructions the whole session runs under, and remember which
		// skills were surfaced so the session's outcome can reinforce them.
		s.system = defaultSystemPrompt
		if block, recalled := recallContext(turnCtx, s.store.Skills(), s.store.Memory(), userText); block != "" {
			s.system += "\n\n" + block
			s.recalled = recalled
		}
	} else if err := s.reopen(turnCtx, userText); err != nil {
		return "", err
	}

	run, err := assembleMission(s.model, s.cwd, s.system, s.store.Resources(s.reg), s.store.Jobs(), s.store.Log(), s.runID)
	if err != nil {
		return "", err
	}
	done := make(chan struct{})
	go func() { _ = run.rt.Start(turnCtx); close(done) }()

	result, runErr := s.driveTurn(turnCtx, run, userText)

	cancel()
	<-done
	return result, runErr
}

// driveTurn subscribes to the run's events after the last one already shown, submits
// the opening goal (first turn) or resumes the reopened goal (later turns), and
// renders the turn live. It advances the session cursor past the events it showed,
// accumulates the transcript, and records the result so the closing learning pass
// learns from the whole conversation.
func (s *replSession) driveTurn(turnCtx context.Context, run *missionRun, userText string) (string, error) {
	events, err := run.sess.Subscribe(turnCtx, s.lastSeq)
	if err != nil {
		return "", err
	}
	if s.started {
		g, err := run.rt.Resume(turnCtx, s.runID)
		if err != nil {
			return "", err
		}
		run.sess.Resume(turnCtx, run.rt, g.Key())
	} else {
		if _, err := run.sess.Submit(turnCtx, run.rt, goal.Spec{
			Objective:     userText,
			StopCondition: "the objective is fully accomplished",
		}); err != nil {
			return "", err
		}
		s.runID = run.sess.ID()
		s.objective = userText
		s.started = true
		_, _ = fmt.Fprintf(s.out, "  run %s\n", s.runID)
	}

	result, transcript, lastSeq, runErr := renderStream(s.out, events, s.verbose)
	if lastSeq > s.lastSeq {
		s.lastSeq = lastSeq
	}
	s.transcript = append(s.transcript, transcript...)
	if runErr == nil {
		s.lastResult = result
		s.converged = true
	}
	return result, runErr
}

// reopen appends the user's line to the shared goal's recorded conversation and
// resets it to run again, so the next drive continues the exchange rather than
// restarting it or stopping on the prior turn's convergence.
func (s *replSession) reopen(ctx context.Context, userText string) error {
	rs := s.store.Resources(s.reg)
	r, err := rs.Get(ctx, goal.Kind, resource.Scope{}, s.runID)
	if err != nil {
		return err
	}
	status, err := goal.DecodeStatus(r)
	if err != nil {
		return err
	}
	status, err = mission.ContinueConversation(status, userText)
	if err != nil {
		return err
	}
	enc, err := status.Encode()
	if err != nil {
		return err
	}
	r.Status = enc
	_, err = rs.Put(ctx, r)
	return err
}

// finish ends the session: it reinforces the skills recall surfaced and, unless
// learning is disabled, distills the whole conversation into durable knowledge so
// the next session starts ahead. A session that never ran a turn just says goodbye.
// Learning is best effort and runs on a live context even when the loop's was
// cancelled, so a Ctrl-C-to-exit still captures what the session learned.
func (s *replSession) finish(ctx context.Context) error {
	if !s.started {
		_, _ = fmt.Fprintln(s.out, "goodbye.")
		return nil
	}
	if len(s.recalled) > 0 {
		_ = learn.Reinforce(ctx, s.store.Skills(), s.recalled, s.converged)
	}
	if s.distiller != nil && s.converged {
		_, _ = fmt.Fprintln(s.out, "\nlearning from this session...")
		distillOutcome(ctx, s.out, s.distiller, s.store.Skills(), s.store.Memory(), s.cwd, learn.Outcome{
			Objective:  s.objective,
			Result:     s.lastResult,
			Transcript: s.transcript,
			Converged:  true,
			Source:     s.runID,
		})
	}
	_, _ = fmt.Fprintf(s.out, "\nsession %s ended.\n", s.runID)
	return nil
}

// isExit reports whether a line is a command to leave the session.
func isExit(line string) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "exit", "quit", ":q", "/exit", "/quit":
		return true
	}
	return false
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
