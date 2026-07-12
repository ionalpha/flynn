package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/externagent"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
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

// replSession holds the state of one interactive session across its turns: the
// assembly inputs (model, store, working directory), the identity and cursor of the
// durable run the turns share, and what the session has accumulated for the learning
// pass at the end.
type replSession struct {
	out io.Writer
	// model and plan drive a native session; ext drives an external agent CLI instead.
	// Exactly one of model and ext is set, chosen by the --model spec, and every place
	// that assembles a turn branches on ext rather than asking the model what it is.
	model        llm.Model
	plan         harness.Plan
	ext          *externAgent
	distiller    learn.Distiller
	learnEnabled bool
	// provDeclared records that the run's external provenance has been written to its
	// stream. A record carries one declaration (the first is what a verifier reads), so
	// it is appended once, at the end of the session, when the tallies are complete.
	provDeclared bool
	verbose      bool
	cwd          string
	store        *sqlite.Store
	reg          *resource.Registry
	keys         editor.Keymap // composer bindings; nil selects the default map
	theme        *theme.Theme  // session theme; nil selects the default theme

	// signer is the instance identity the session seals its run under; nil when no key
	// could be loaded, in which case /seal reports the run cannot be sealed rather than
	// failing. dataDir roots the store the run is verified from.
	signer  chain.RootSigner
	dataDir string

	// modelSpec is the "provider:model" string of the model the session currently
	// drives, shown by /model and updated when /model switches it.
	modelSpec string

	// notice, when set, surfaces an out-of-band session note (currently the recall
	// summary) to the user. The full-screen shell appends it to the transcript; the
	// line interface prints it. Nil discards it, so a non-interactive run is quiet.
	notice func(string)

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

	// carriedContext is a compacted summary a prior /compact produced, folded into the
	// next fresh run's standing instructions so the thread continues with less context.
	// /clear drops it; /compact sets it.
	carriedContext string

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
	if s.ext != nil && len(images) > 0 {
		// The turn reaches the CLI as text on its stdin, so an attachment has nowhere to go.
		// Refusing is the honest answer: dropping it silently would leave the user reasoning
		// about an image the agent never saw.
		return "", fmt.Errorf("a %s session takes text turns only: it has no way to carry an image attachment to the external agent", s.ext.driver.Name())
	}

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
		// A prior /compact carries a summary of the compacted conversation into the fresh
		// run's standing instructions, so the new run continues the thread with far less
		// context than replaying every turn.
		if s.carriedContext != "" {
			s.system += "\n\n" + s.carriedContext
		}
		if block, recalled, items := recallContext(turnCtx, s.store.Skills(), s.store.Memory(), userText); block != "" {
			s.system += "\n\n" + block
			s.recalled = recalled
			// Surface what past learning was pulled into context (naming each item), so
			// the recall the agent stands on is visible rather than a silent addition.
			if s.notice != nil && len(items) > 0 {
				s.notice("recalled from earlier runs:")
				for _, it := range items {
					s.notice("  " + it)
				}
			}
		}
	} else if err := s.reopen(turnCtx, userText, images); err != nil {
		return "", err
	}

	var (
		run *missionRun
		err error
	)
	if s.ext != nil {
		// The external CLI drives the loop: the same sandbox, session, bridged toolset,
		// grant, and governance recording as a native turn, but the turn is an episode of
		// the CLI's own conversation rather than a step of ours.
		run, err = assembleExternalMission(s.ext, s.cwd, s.system, s.store.Resources(s.reg), s.store.Jobs(), s.store.Log(), s.runID, sandbox.ResourceLimits{})
	} else {
		run, err = assembleMission(s.model, s.plan, s.cwd, s.system, s.store.Resources(s.reg), s.store.Jobs(), s.store.Log(), s.runID, sandbox.ResourceLimits{})
	}
	if err != nil {
		return "", err
	}
	// One assembly per turn, so one sandbox per turn: without this an interactive session
	// accumulates a container profile for every message the user sends.
	defer func() { _ = run.Close() }()
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
			// The model the loop drives. Empty for a native session (the session's own model
			// applies); for an external agent it is the CLI's model string, so `flynn --model
			// claude:<model>` pins the model the CLI itself runs.
			Model: externalModel(s.ext),
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
	if s.ext != nil {
		// An external turn continues the conversation the CLI holds: the transcript lives
		// inside the harness, so the goal carries the handle to it, not a copy of it.
		status, err = externagent.ContinueEpisode(status, userText)
	} else {
		status, err = mission.ContinueConversation(status, userText, images...)
	}
	if err != nil {
		return err
	}
	enc, err := status.Encode()
	if err != nil {
		return err
	}
	r.Status = enc
	// A /model switch inside an external session changes the model the CLI drives on the
	// next episode, which lives on the goal's spec. Write it with the reopened status, so
	// one write reopens the goal and retargets it.
	if s.ext != nil {
		spec, derr := goal.DecodeSpec(r)
		if derr != nil {
			return derr
		}
		if spec.Model != s.ext.model {
			spec.Model = s.ext.model
			raw, merr := json.Marshal(spec)
			if merr != nil {
				return merr
			}
			r.Spec = raw
		}
	}
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
	s.declareProvenance(ctx)
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
// the run into a verifiable record, verifying that record, exporting the record to a
// portable file, forking the run onto a new branch, and replaying its recorded history.
// It reports whether it claimed the line and
// any error to surface, so each interface renders the outcome its own way. A line that is
// not a command is left for the model.
func (s *replSession) replCommand(ctx context.Context, line string) (handled bool, err error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return false, nil
	}
	switch strings.ToLower(fields[0]) {
	case "/help", "?":
		renderHelp(s.out)
		return true, nil
	case "/clear":
		s.clear()
		_, _ = fmt.Fprintln(s.out, "  context cleared; starting a fresh conversation")
		return true, nil
	case "/compact":
		n, err := s.compact(ctx)
		if err != nil {
			return true, err
		}
		_, _ = fmt.Fprintf(s.out, "  compacted %d messages into a summary; continuing with less context\n", n)
		return true, nil
	case "/memory":
		renderMemory(ctx, s.out, s.store.Memory())
		return true, nil
	case "/remember":
		rememberFact(ctx, s.out, s.store.Memory(), strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0])))
		return true, nil
	case "/skills":
		renderSkills(ctx, s.out, s.store.Skills())
		return true, nil
	case "/tokens":
		u, turns := session.Usage{}, 0
		if s.runID != "" {
			if events, herr := session.History(ctx, s.store.Log(), s.runID); herr == nil {
				p := session.Project(events)
				u, turns = p.Usage, p.Turns
			}
		}
		renderTokens(s.out, u, turns)
		return true, nil
	case "/models":
		return true, s.showCatalog(s.out)
	case "/model":
		return true, s.switchModel(ctx, fields[1:], s.out)
	case "/seal":
		if err := s.seal(ctx); err != nil {
			return true, err
		}
		_, _ = fmt.Fprintln(s.out, "  run sealed; /verify to check it")
		return true, nil
	case "/verify":
		return true, s.verify(ctx, s.out)
	case "/export":
		path, err := s.export(ctx, "")
		if err != nil {
			return true, err
		}
		_, _ = fmt.Fprintf(s.out, "  record exported to %s; verify anywhere with: flynn spine verify --file %s\n", path, path)
		return true, nil
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

// showCatalog prints the model catalog into the session, the same view as `flynn models`,
// so a user can see what to switch to without leaving the session. It writes to out so
// both front-ends can place it: line mode prints straight through, the full-screen
// session captures it into the scrollback.
func (s *replSession) showCatalog(out io.Writer) error {
	return runModels(nil, s.dataDir, out)
}

// switchModel changes the model the rest of the session drives. With no argument it
// reports the current model; otherwise it resolves the requested "provider:model" spec,
// swaps it in for the next turn, and records it as the default so a later launch reuses
// it. A spec that cannot be resolved (an unknown provider, a missing key) is reported
// without ending the session. Feedback goes to out so either front-end can place it.
func (s *replSession) switchModel(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprintf(out, "  current model: %s\n  switch with: /model <provider:model> (see /models)\n", s.modelSpec)
		return nil
	}
	spec := args[0]
	name, cliModel, isExt := externalAgentSpec(spec)

	switch {
	case isExt && s.ext != nil && s.ext.driver.Name() == name:
		// Same harness, different model: the CLI keeps driving, and the next episode runs
		// the model named here. The conversation the CLI holds is not disturbed, which is
		// the point of switching rather than restarting.
		s.ext.model = cliModel

	case s.started && (isExt || s.ext != nil):
		// The run's record declares which harness drove it, and a record states one
		// provenance for the whole run. Swapping the harness mid-run would seal a record
		// whose declaration is true of only part of it, so the swap is refused rather than
		// quietly producing that. A new session is one line away.
		return fmt.Errorf("/model %s: this run is already being driven by %s, and a run's record declares the one harness that drove it. "+
			"Leave and start a new session to drive %s", spec, s.harnessName(), spec)

	case isExt:
		// Nothing has run yet, so the session is free to become an external one.
		ea, err := resolveExternalAgent(ctx, name, cliModel, s.cwd)
		if err != nil {
			return fmt.Errorf("/model %s: %w", spec, err)
		}
		s.ext.close() // the harness this session is no longer going to drive
		s.ext = ea
		s.model, s.plan = nil, harness.Plan{}
		// An external harness exposes no model to distill through, so a session it drives
		// does not learn back. The one-shot path skips it for the same reason.
		s.distiller = nil

	default:
		model, plan, err := resolveModel(ctx, spec, s.dataDir)
		if err != nil {
			return fmt.Errorf("/model %s: %w", spec, err)
		}
		s.model = model
		s.plan = plan
		s.ext.close() // switching to a native model before any turn ran
		s.ext = nil
		// A distilling session learns through the model, so keep the distiller on the model
		// the session now drives.
		if s.learnEnabled {
			s.distiller = governedDistiller(model)
		}
	}

	s.modelSpec = spec
	if err := writeActiveModel(s.dataDir, spec); err != nil {
		// The switch still holds for this session; only persistence failed.
		_, _ = fmt.Fprintf(out, "  switched to %s (could not save it as the default: %v)\n", spec, err)
		return nil
	}
	_, _ = fmt.Fprintf(out, "  switched to %s; saved as the default for the next run\n", spec)
	return nil
}

// declareProvenance writes the run's provenance onto its stream when an external agent
// harness drove the session: the record then vouches for the enforced effects (every
// tool call crossed the dispatch waist) while naming the harness's inner reasoning as an
// unobserved gap, so an external run never claims the integrity of a native one. The
// absence of this declaration is what marks a record as natively driven, so a session
// the CLI drove must not be sealed without it.
//
// It is written once, and late: a verifier reads the first declaration a record carries,
// and the tallies it declares (the attested events, the harness's tool-choice rate) are
// only complete once the session's episodes have all run. A native session declares
// nothing, which is what says its own loop drove it.
func (s *replSession) declareProvenance(ctx context.Context) {
	if s.ext == nil || s.provDeclared || !s.started {
		return
	}
	s.provDeclared = true
	if err := appendProvenance(ctx, s.store.Log(), s.runID, observedProvenance(s.ext)); err != nil {
		_, _ = fmt.Fprintf(s.out, "  (provenance not recorded: %v)\n", err)
		return
	}
	// An event the harness reported that the record could not hold is a hole in the
	// harness's account of itself. The declaration names every event it reported, so a
	// verifier sees the gap from the record alone; saying it here tells the operator why.
	if lost, lerr := unrecordedAttested(s.ext); lost > 0 {
		_, _ = fmt.Fprintf(s.out, "  (%d attested event(s) not recorded: %v)\n", lost, lerr)
	}
}

// harnessName names what is driving the session, for a message that has to say so.
func (s *replSession) harnessName() string {
	if s.ext != nil {
		return s.ext.driver.Name()
	}
	return s.modelSpec
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
	// An external run's record must carry its provenance declaration before it is sealed,
	// or the sealed record reads as though Flynn's own loop drove it: the exact overclaim
	// the declaration exists to prevent.
	s.declareProvenance(ctx)
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

// export writes the session's sealed record to path and returns the path written. It
// needs a sealed run: a run not yet sealed carries no record and is reported, so a caller
// seals before exporting. The written file is the portable, independently verifiable
// artifact `flynn spine verify --file` (and any third party) checks.
func (s *replSession) export(ctx context.Context, path string) (string, error) {
	if !s.started {
		return "", errors.New("nothing to export yet; run a turn first")
	}
	if path == "" {
		path = s.runID + ".flynnrecord"
	}
	if err := exportRecord(ctx, s.store, s.runID, path); err != nil {
		return "", err
	}
	return path, nil
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
