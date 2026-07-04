package main

import (
	"fmt"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/internal/tui/screen"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/session"
)

// govPanel is the shell's governance overlay: an on-demand multi-row view of the run's
// current governance posture, toggled with Ctrl+O and dismissed with Escape. The status
// badge compresses the same projection into one line; the panel expands it, naming the
// recent governed actions the badge only counts. It is a live-region component repainted
// in place, so it never joins the scrollback. Its own mutex guards the snapshot and the
// open flag, so the paint goroutine's Render and the turn goroutine's set never race and
// Render never reaches back into the shell.
type govPanel struct {
	th *theme.Theme

	mu   sync.Mutex
	open bool
	proj session.Projection
}

// ledgerRows bounds how many recent actions the panel lists, so a busy run's panel
// stays a fixed height instead of growing with the ledger. The newest actions are
// shown; older ones are summarized by the counts row above them.
const ledgerRows = 8

// set replaces the panel's snapshot of the run projection. It is called as events fold
// into the run status, so an open panel tracks the run live.
func (p *govPanel) set(proj session.Projection) {
	p.mu.Lock()
	p.proj = proj
	p.mu.Unlock()
}

// toggle flips the panel between shown and hidden and reports the new state.
func (p *govPanel) toggle() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.open = !p.open
	return p.open
}

// close hides the panel. It is idempotent, so Escape with the panel already closed is
// harmless.
func (p *govPanel) close() {
	p.mu.Lock()
	p.open = false
	p.mu.Unlock()
}

// isOpen reports whether the panel is currently shown.
func (p *govPanel) isOpen() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.open
}

// Render draws the governance panel from the current projection, or nothing when the
// panel is hidden. The rows are styled by role so a theme restyles the panel without
// touching this, and every row is truncated to width so the panel never overflows.
func (p *govPanel) Render(width int) []string {
	p.mu.Lock()
	open, proj := p.open, p.proj
	p.mu.Unlock()
	if !open {
		return nil
	}

	var rows []string
	add := func(role theme.Role, text string) {
		rows = append(rows, p.th.Render(role, screen.Truncate(text, width)))
	}

	add(theme.Heading, "governance  (ctrl+o to close)")
	if obj := strings.TrimSpace(proj.Objective); obj != "" {
		add(theme.Muted, "  goal: "+obj)
	}
	add(recordRole(proj.Record), "  record: "+string(proj.Record))
	if proj.Containment != "" {
		add(theme.Trust, "  trust: "+proj.Containment)
	}
	add(theme.Muted, "  actions: "+actionsSummary(proj))
	if tokens := badgeTokens(proj.Usage); tokens != "" {
		add(theme.Muted, "  budget: "+tokens)
	}
	if proj.Terminal {
		if proj.Err != "" {
			add(theme.Error, "  stalled: "+proj.Err)
		} else {
			add(theme.Success, "  converged")
		}
	}
	for _, line := range ledgerLines(proj.Actions) {
		add(line.role, line.text)
	}
	return rows
}

// actionsSummary renders the run's governed-action tallies as one line: how many are
// running, done, and blocked, dropping the segments that are zero so a run with no
// boundary hit does not show "0 blocked".
func actionsSummary(p session.Projection) string {
	inflight := p.Admitted - p.Completed - p.Rejected
	if inflight < 0 {
		inflight = 0
	}
	var segs []string
	if inflight > 0 {
		segs = append(segs, fmt.Sprintf("%d running", inflight))
	}
	segs = append(segs, fmt.Sprintf("%d done", p.Completed))
	if p.Rejected > 0 {
		segs = append(segs, fmt.Sprintf("%d blocked", p.Rejected))
	}
	return strings.Join(segs, " · ")
}

// panelRow pairs a ledger line's text with the theme role it renders under, so the
// action's state colours its row (blocked red, done muted, running busy).
type panelRow struct {
	role theme.Role
	text string
}

// ledgerLines renders the tail of the action ledger, newest last, one row per action,
// or nothing when the ledger is empty. It shows at most ledgerRows entries and notes
// how many older ones it dropped, so the panel height is bounded and the omission is
// visible rather than silent.
func ledgerLines(actions []session.ActionEntry) []panelRow {
	if len(actions) == 0 {
		return nil
	}
	shown := actions
	dropped := 0
	if len(shown) > ledgerRows {
		dropped = len(shown) - ledgerRows
		shown = shown[dropped:]
	}
	rows := make([]panelRow, 0, len(shown)+1)
	if dropped > 0 {
		rows = append(rows, panelRow{theme.Muted, fmt.Sprintf("  (%d earlier)", dropped)})
	}
	for _, a := range shown {
		rows = append(rows, ledgerRow(a))
	}
	return rows
}

// ledgerRow renders one action entry as a marked, role-styled row: a check for a clean
// completion, a warning for one that finished with a fault, a cross for a refusal, and
// an ellipsis for one still running. The trust level trails the name so the row shows
// the posture the action ran under.
func ledgerRow(a session.ActionEntry) panelRow {
	name := a.Action
	if name == "" {
		name = "action"
	}
	if a.Trust != "" {
		name += " (" + a.Trust + ")"
	}
	switch a.State {
	case session.ActionBlocked:
		return panelRow{theme.Rejected, "  ✗ " + name + " blocked: " + faultText(a.Fault)}
	case session.ActionDone:
		if a.Fault != "" {
			return panelRow{theme.Warning, "  !! " + name + " failed: " + a.Fault}
		}
		return panelRow{theme.Admitted, "  ✓ " + name}
	default:
		return panelRow{theme.StatusBusy, "  ... " + name}
	}
}
