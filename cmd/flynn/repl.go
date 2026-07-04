package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/session"
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

	model, plan, err := resolveModelOrOnboard(ctx, modelSpec, dataDir)
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
	// Load the instance signer so the session can seal its run into a verifiable record
	// on demand. Best effort, exactly as the one-shot runner treats it: a session
	// without a key still runs every turn; only /seal reports the key is missing.
	signer, serr := runSigner(ctx, dataDir)
	if serr != nil {
		signer = nil
	}

	var distiller learn.Distiller
	if learnEnabled {
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
		keys:      keys,
		theme:     th,
		out:       &syncWriter{w: os.Stdout},
		model:     model,
		plan:      plan,
		distiller: distiller,
		verbose:   verbose,
		cwd:       cwd,
		store:     store,
		reg:       reg,
		signer:    signer,
		dataDir:   dataDir,
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
	plan      harness.Plan
	distiller learn.Distiller
	verbose   bool
	cwd       string
	store     *sqlite.Store
	reg       *resource.Registry
	keys      editor.Keymap // composer bindings; nil selects the default map
	theme     *theme.Theme  // session theme; nil selects the default theme

	// signer is the instance identity the session seals its run under; nil when no key
	// could be loaded, in which case /seal reports the run cannot be sealed rather than
	// failing. dataDir roots the store the run is verified from.
	signer  chain.RootSigner
	dataDir string

	// observer, when set, receives every session event as the turn renders. The
	// interactive shell installs it to render the typed stream itself (transcript,
	// governance, status badge); the line interface leaves it nil and reads the
	// flat text renderStream writes to out.
	observer func(session.Event)

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
		if handled, err := s.replCommand(ctx, line); handled {
			// verify writes its own per-tier report to out; only a plain error (an
			// unsealed run, a missing key) needs a line of its own here.
			if err != nil && !errors.Is(err, errChecksFailed) {
				_, _ = fmt.Fprintf(s.out, "  %v\n", err)
			}
			continue
		}
		if _, err := s.runTurn(ctx, line, nil, sigCh); err != nil {
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
func (s *replSession) runTurn(ctx context.Context, userText string, images []llm.Image, sigCh <-chan os.Signal) (string, error) {
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
	} else if err := s.reopen(turnCtx, userText, images); err != nil {
		return "", err
	}

	run, err := assembleMission(s.model, s.plan, s.cwd, s.system, s.store.Resources(s.reg), s.store.Jobs(), s.store.Log(), s.runID)
	if err != nil {
		return "", err
	}
	done := make(chan struct{})
	go func() { _ = run.rt.Start(turnCtx); close(done) }()

	result, runErr := s.driveTurn(turnCtx, run, userText, images)

	cancel()
	<-done
	return result, runErr
}

// driveTurn subscribes to the run's events after the last one already shown, submits
// the opening goal (first turn) or resumes the reopened goal (later turns), and
// renders the turn live. It advances the session cursor past the events it showed,
// accumulates the transcript, and records the result so the closing learning pass
// learns from the whole conversation.
func (s *replSession) driveTurn(turnCtx context.Context, run *missionRun, userText string, images []llm.Image) (string, error) {
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
		objective := openingObjective(userText, images)
		if _, err := run.sess.Submit(turnCtx, run.rt, goal.Spec{
			Objective:     objective,
			Attachments:   images,
			StopCondition: "the objective is fully accomplished",
		}); err != nil {
			return "", err
		}
		s.runID = run.sess.ID()
		s.objective = objective
		s.started = true
		_, _ = fmt.Fprintf(s.out, "  run %s\n", s.runID)
	}

	result, transcript, lastSeq, runErr := renderStream(s.out, events, s.verbose, s.observer)
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

// openingObjective is the objective a goal opens on. It is the user's line,
// unless the opening turn is images with no prose: a Goal must carry a
// non-empty objective (an objective is what the run is driven toward), so an
// image-only open gets a neutral instruction that matches what pasting an
// image alone means. Later turns append to the conversation directly and are
// not bound by this.
func openingObjective(userText string, images []llm.Image) string {
	if userText == "" && len(images) > 0 {
		return "Look at the attached image."
	}
	return userText
}

// reopen appends the user's line to the shared goal's recorded conversation and
// resets it to run again, so the next drive continues the exchange rather than
// restarting it or stopping on the prior turn's convergence.
func (s *replSession) reopen(ctx context.Context, userText string, images []llm.Image) error {
	rs := s.store.Resources(s.reg)
	r, err := rs.Get(ctx, goal.Kind, resource.Scope{}, s.runID)
	if err != nil {
		return err
	}
	status, err := goal.DecodeStatus(r)
	if err != nil {
		return err
	}
	status, err = mission.ContinueConversation(status, userText, images...)
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

// replCommand handles the session's slash commands that are not model turns: sealing
// the run into a verifiable record, verifying that record, forking the run onto a new
// branch, and replaying its recorded history. It reports whether it claimed the line and
// any error to surface, so each interface renders the outcome its own way. A line that is
// not a command is left for the model.
func (s *replSession) replCommand(ctx context.Context, line string) (handled bool, err error) {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "/seal":
		if err := s.seal(ctx); err != nil {
			return true, err
		}
		_, _ = fmt.Fprintln(s.out, "  run sealed; /verify to check it")
		return true, nil
	case "/verify":
		return true, s.verify(ctx, s.out)
	case "/fork":
		forkID, err := s.fork(ctx)
		if err != nil {
			return true, err
		}
		_, _ = fmt.Fprintf(s.out, "  forked to run %s; the original is untouched\n", forkID)
		return true, nil
	case "/replay":
		hist, _, err := renderHistory(ctx, s.store, s.runID, s.verbose)
		if err != nil {
			return true, err
		}
		if strings.TrimSpace(hist) == "" {
			_, _ = fmt.Fprintln(s.out, "  nothing recorded to replay yet")
			return true, nil
		}
		_, _ = fmt.Fprint(s.out, hist)
		return true, nil
	}
	return false, nil
}

// seal signs the session's run into a verifiable record stored on its stream. It needs a
// started run and the instance signer; without either it reports why rather than failing
// the session. Sealing reads the run's current durable events, so it captures the whole
// conversation, including history a resumed session continued.
func (s *replSession) seal(ctx context.Context) error {
	if !s.started {
		return errors.New("nothing to seal yet; run a turn first")
	}
	if s.signer == nil {
		return errors.New("cannot seal: no instance signing key is available")
	}
	return sealRunFromStore(ctx, s.store, s.runID, s.signer)
}

// verify checks the session's sealed record and writes its per-tier report to out. It
// returns errChecksFailed if a tier fails (the report names which), or a plain error
// when the run has not been sealed yet.
func (s *replSession) verify(ctx context.Context, out io.Writer) error {
	if !s.started {
		return errors.New("nothing to verify yet; run a turn first")
	}
	return verifyStoredRun(ctx, out, s.store, s.runID)
}

// fork branches the current run into a new independent run seeded with a verbatim copy
// of the conversation so far, switches the session onto it, and returns its id. The
// original run keeps its id, its recorded history, and its seal, so a branch never
// disturbs the run it came from. The fork opens a fresh event stream under the new id:
// its turns record onto their own hash chain from the branch point on, while the model
// still sees the whole prior conversation carried on the copied checkpoint. It needs a
// started run; without one there is nothing to branch from.
func (s *replSession) fork(ctx context.Context) (string, error) {
	if !s.started {
		return "", errors.New("nothing to fork yet; run a turn first")
	}
	rs := s.store.Resources(s.reg)
	parent, err := rs.Get(ctx, goal.Kind, resource.Scope{}, s.runID)
	if err != nil {
		return "", err
	}
	forkID := ids.New()
	forked := parent
	forked.Name = forkID
	// Clear the parent's identity and sync envelope so the store creates a new record
	// instead of overwriting the run this branched from. Spec and Status (the
	// conversation checkpoint) carry over verbatim, so the fork opens from the exact
	// state the parent is in.
	forked.ID = ""
	forked.Envelope = resource.Envelope{}
	forked.Annotations = withForkParent(parent.Annotations, s.runID)
	if _, err := rs.Put(ctx, forked); err != nil {
		return "", err
	}
	// Switch the session onto the fork and rewind the cursor to the start of its empty
	// stream, so the next turn continues the copied conversation while recording onto
	// the fork's own chain.
	s.runID = forkID
	s.lastSeq = 0
	return forkID, nil
}

// forkParentAnnotation records the id of the run a fork branched from, so the lineage of
// a branched run is auditable from its resource alone.
const forkParentAnnotation = "flynn/forked-from"

// withForkParent returns a copy of the parent run's annotations with the fork-parent id
// set, leaving the parent's own annotation map untouched.
func withForkParent(parent map[string]string, parentID string) map[string]string {
	out := make(map[string]string, len(parent)+1)
	for k, v := range parent {
		out[k] = v
	}
	out[forkParentAnnotation] = parentID
	return out
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
