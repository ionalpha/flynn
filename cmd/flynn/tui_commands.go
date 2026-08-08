package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/session"
)

// doSeal seals the session's run into a verifiable record and moves the status badge to
// sealed. The seal appends the record to the run's own stream; because the shell does
// not subscribe to the stream at idle, it folds the sealed state into the projection
// directly, the same state a live record.sealed event would produce, so the badge
// tracks it. A failure is reported inline and leaves the badge unchanged.
func (h *sessionHost) doSeal(ctx context.Context) {
	h.echoPrompt("/seal")
	if err := h.s.seal(ctx); err != nil {
		h.ui.Append(h.th.Render(theme.Rejected, "  "+err.Error()))
		return
	}
	h.foldRecord(session.Event{Kind: session.KindRecordSealed})
	h.ui.Append(h.th.Render(theme.Success, "  run sealed; /verify to check it"))
}

// doVerify verifies the session's sealed record, prints its per-tier report to the
// scrollback, and moves the badge to verified when every tier passes. A run not yet
// sealed, or a tier that fails, is reported and leaves the badge unchanged.
func (h *sessionHost) doVerify(ctx context.Context) {
	h.echoPrompt("/verify")
	var buf bytes.Buffer
	err := h.s.verify(ctx, &buf)
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		h.ui.Append(h.th.Render(theme.ToolOutput, "  "+line))
	}
	if err != nil {
		// A failed tier is already named in the report above; only a plain error (an
		// unsealed run) needs a line of its own.
		if !errors.Is(err, errChecksFailed) {
			h.ui.Append(h.th.Render(theme.Rejected, "  "+err.Error()))
		}
		return
	}
	h.foldRecord(session.Event{Kind: session.KindRecordVerified})
	h.ui.Append(h.th.Render(theme.Success, "  record verified"))
}

// doModels prints the model catalog to the scrollback, the same view as `flynn models`,
// so a user can browse the blessed models without leaving the session.
func (h *sessionHost) doModels(_ context.Context) {
	h.echoPrompt("/models")
	var buf bytes.Buffer
	err := h.s.showCatalog(&buf)
	h.appendReport(buf.String())
	if err != nil {
		h.ui.Append(h.th.Render(theme.Rejected, "  "+err.Error()))
	}
}

// doModel reports the current model, or switches to the requested one and saves it as the
// default, mirroring /model in line mode. Its feedback and any error land in the
// scrollback rather than the discarded turn output.
func (h *sessionHost) doModel(ctx context.Context, args []string) {
	echo := "/model"
	if len(args) > 0 {
		echo += " " + strings.Join(args, " ")
	}
	h.echoPrompt(echo)
	var buf bytes.Buffer
	err := h.s.switchModel(ctx, args, &buf)
	h.appendReport(buf.String())
	if err != nil {
		h.ui.Append(h.th.Render(theme.Rejected, "  "+err.Error()))
	}
}

// doHelp prints the session's commands and shortcuts to the scrollback, so a user
// can see everything available without leaving the session or reading the footer.
func (h *sessionHost) doHelp(_ context.Context) {
	h.echoPrompt("/help")
	var buf bytes.Buffer
	renderHelp(&buf)
	h.appendReport(buf.String())
}

// doClear detaches the session from its run and starts a fresh conversation, resetting
// the badge and the transcript's dedup state. The prior run stays durable and resumable.
func (h *sessionHost) doClear(_ context.Context) {
	h.echoPrompt("/clear")
	h.s.clear()
	h.resetContext()
	h.ui.Append(h.th.Render(theme.Status, "  context cleared; starting a fresh conversation"))
}

// doCompact summarizes the conversation and continues from the summary, so the session
// stops resending its whole history. It calls the model, so it runs as a turn.
func (h *sessionHost) doCompact(ctx context.Context) {
	h.echoPrompt("/compact")
	h.live.set("compacting...")
	h.pokeLive()
	n, err := h.s.compact(ctx)
	h.live.set("")
	h.pokeLive()
	if err != nil {
		h.ui.Append(h.th.Render(theme.Rejected, "  "+err.Error()))
		return
	}
	h.resetContext()
	h.ui.Append(h.th.Render(theme.Success, fmt.Sprintf("  compacted %d messages into a summary; continuing with less context", n)))
}

// resetContext resets the run projection and the transcript's converged-dedup state
// after the session detaches from its run (clear or compact), so the badge and the
// dedup start fresh with the next run.
func (h *sessionHost) resetContext() {
	h.mu.Lock()
	h.proj = session.NewProjection()
	p := h.proj
	h.mu.Unlock()
	h.tv = newTranscriptView(h.th)
	h.panel.set(p)
	h.refreshStatus()
}

// doTokens prints this run's token breakdown to the scrollback, reading the same
// projection the badge shows so the two always agree.
func (h *sessionHost) doTokens(_ context.Context) {
	h.echoPrompt("/tokens")
	h.mu.Lock()
	u, turns := h.proj.Usage, h.proj.Turns
	h.mu.Unlock()
	var buf bytes.Buffer
	renderTokens(&buf, u, turns)
	h.appendReport(buf.String())
}

// doMemory prints the agent's durable memory to the scrollback, so a user can see
// what it remembers across runs, not just that recall happened.
func (h *sessionHost) doMemory(ctx context.Context) {
	h.echoPrompt("/memory")
	var buf bytes.Buffer
	renderMemory(ctx, &buf, h.s.memory().store)
	h.appendReport(buf.String())
}

// doRemember pins the fact into durable memory and reports the outcome to the
// scrollback, so the user sees what was kept rather than trusting a silent write.
func (h *sessionHost) doRemember(ctx context.Context, fact string) {
	h.echoPrompt(strings.TrimSpace("/remember " + fact))
	var buf bytes.Buffer
	rememberFact(ctx, &buf, h.s.memory().store, fact)
	h.appendReport(buf.String())
}

// doSkills prints the agent's learned skills to the scrollback, with each one's
// outcome record, so a user can see and judge what it has learned.
func (h *sessionHost) doSkills(ctx context.Context) {
	h.echoPrompt("/skills")
	var buf bytes.Buffer
	renderSkills(ctx, &buf, h.s.store.Skills())
	h.appendReport(buf.String())
}

// appendReport writes each non-empty line of a captured command's output to the
// scrollback as tool output, the same treatment /verify gives its per-tier report.
func (h *sessionHost) appendReport(s string) {
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if line == "" {
			continue
		}
		h.ui.Append(h.th.Render(theme.ToolOutput, "  "+line))
	}
}

// doExport writes the session's sealed record to a portable file and reports the path
// inline, so a run can be handed to a third party or re-verified with `flynn spine verify
// --file` without the durable store. A run not yet sealed carries no record and is
// reported, leaving nothing written; the file defaults to <run-id>.flynnrecord in the
// working directory.
func (h *sessionHost) doExport(ctx context.Context) {
	h.echoPrompt("/export")
	path, err := h.s.export(ctx, "")
	if err != nil {
		h.ui.Append(h.th.Render(theme.Rejected, "  "+err.Error()))
		return
	}
	h.ui.Append(h.th.Render(theme.Success, "  record exported to "+path))
	h.ui.Append(h.th.Render(theme.Muted, "  verify anywhere with: flynn spine verify --file "+path))
}

// doFork branches the run into a new independent run seeded with the conversation so far
// and switches the session onto it, resetting the record badge to the fork's own fresh
// recording state. The original run keeps its history and seal. The next prompt continues
// on the fork; a failure is reported inline and leaves the session on the original run.
func (h *sessionHost) doFork(ctx context.Context) {
	h.echoPrompt("/fork")
	forkID, err := h.s.fork(ctx)
	if err != nil {
		h.ui.Append(h.th.Render(theme.Rejected, "  "+err.Error()))
		return
	}
	// The fork's stream is empty, so its badge starts at the fresh recording state; the
	// next turn's events repopulate the governance projection from the branch point.
	h.mu.Lock()
	h.proj = session.NewProjection()
	p := h.proj
	h.mu.Unlock()
	h.panel.set(p)
	h.refreshStatus()
	h.ui.Append(h.th.Render(theme.Success, "  forked to run "+forkID+"; the original is untouched"))
}

// doReplay re-renders the run's recorded events into the scrollback through the themed
// transcript renderer, between clear delimiters. It reads the run's history from the
// durable store and folds it through a fresh transcript view, so the replay is the run
// as it was recorded (the same markdown and governance rendering a live turn produces),
// independent of what is currently on screen. It is a pure read; it changes no run state.
func (h *sessionHost) doReplay(ctx context.Context) {
	h.echoPrompt("/replay")
	events, err := session.History(ctx, h.s.store.Log(), h.s.runID)
	if err != nil {
		h.ui.Append(h.th.Render(theme.Rejected, "  replay failed: "+err.Error()))
		return
	}
	if len(events) == 0 {
		h.ui.Append(h.th.Render(theme.Status, "  nothing recorded to replay yet"))
		return
	}
	h.ui.Append(h.th.Render(theme.Status, fmt.Sprintf("  replay of run %s (%d events)", h.s.runID, len(events))))
	tv := newTranscriptView(h.th)
	width := h.ui.Width()
	for _, ev := range events {
		if lines := tv.lines(ev, width); len(lines) > 0 {
			h.ui.Append(lines...)
		}
	}
	h.ui.Append(h.th.Render(theme.Status, "  end of replay"))
}
