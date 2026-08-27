package main

// One turn of the session, from the user's line to a terminal event: assembling the
// per-turn mission, driving it against the shared durable goal, reopening that goal for
// the next line, and the learning pass that closes the conversation.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/ionalpha/flynn/externagent"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/skill/skilltool"
)

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
		// The digest is the push half: what this install has learned, offered whether
		// or not the opening line mentions it. It goes in before recall because it is
		// the standing background the conversation runs against, where recall answers
		// the line the user actually typed.
		// A failed read is only reported in a verbose session: a chat interface that
		// opened with a store diagnostic would be answering a question nobody asked.
		var digestErrs io.Writer
		if s.verbose {
			digestErrs = s.out
		}
		s.system = withWake(turnCtx, s.memory(), s.system, digestErrs)
		if block, recalled, items := recallContext(turnCtx, s.store.Skills(), s.memory().store, userText); block != "" {
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
	// The turn is reassembled each message; the skill toolset is not, so what the
	// session has read accumulates across the whole conversation.
	if s.skillset == nil {
		s.skillset = skilltool.New(s.store.Skills(), skilltool.WithNotes(s.memory().skillNotes()))
	}
	if s.ext != nil {
		// The external CLI drives the loop: the same sandbox, session, bridged toolset,
		// grant, and governance recording as a native turn, but the turn is an episode of
		// the CLI's own conversation rather than a step of ours.
		run, err = assembleExternalMission(s.ext, s.cwd, s.system, s.store.Resources(s.reg), s.store.Jobs(), s.store.Log(), s.skillset, s.runID, sandbox.ResourceLimits{})
	} else {
		run, err = assembleMission(s.model, s.plan, s.cwd, s.system, s.store.Resources(s.reg), s.store.Jobs(), s.store.Log(), s.skillset, s.runID, sandbox.ResourceLimits{}, false, false, s.gates)
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
			StopCondition: defaultStopCondition,
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
	// What the session was shown and what it read are recorded apart: every recalled
	// skill was offered, and the session's outcome is credited only to the ones it
	// loaded through skill_read.
	_ = learn.Offer(ctx, s.store.Skills(), s.recalled)
	_ = learn.Reinforce(ctx, s.store.Skills(), s.skillset.Reads(), s.converged)
	if s.distiller != nil && s.converged {
		_, _ = fmt.Fprintln(s.out, "\nlearning from this session...")
		distillOutcome(ctx, s.out, s.distiller, s.store.Skills(), s.memory().store, s.cwd, learn.Outcome{
			Objective:  s.objective,
			Result:     s.lastResult,
			Transcript: s.transcript,
			Converged:  true,
			Source:     s.runID,
			SkillsRead: s.skillset.Reads(),
		})
	}
	_, _ = fmt.Fprintf(s.out, "\nsession %s ended.\n", s.runID)
	return nil
}
