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
// recent governed actions the badge only counts and, when a run delegates, the live
// fan-out tree of its children. It is a live-region component repainted
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
	for _, line := range fanoutLines(proj.Fanout) {
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

// fanoutRows bounds how many fan-out children the panel lists, so a wide or deep
// delegation stays a fixed height. Children are shown in spawn order, parents before
// the children they spawned; any past the cap are noted rather than dropped silently.
const fanoutRows = 12

// fanoutLines renders the run's fan-out tree from the projection's flat child list, or
// nothing when the run has not delegated. Each child is one indented row marked by state
// (running, done, failed) with its turn count and, once folded, a short result. The list
// is flat with a Parent id per child, so this builds the hierarchy: a child whose parent
// is not itself a listed child hangs off the run root at depth zero, and a deeper
// delegation nests under the child that spawned it.
func fanoutLines(children []session.FanoutChild) []panelRow {
	if len(children) == 0 {
		return nil
	}
	// Group children under their parent in spawn order, and treat a child whose parent
	// is not another listed child as a root hanging off the run's own goal.
	isChild := make(map[string]bool, len(children))
	for _, c := range children {
		isChild[c.ID] = true
	}
	byParent := map[string][]session.FanoutChild{}
	var roots []session.FanoutChild
	for _, c := range children {
		if isChild[c.Parent] {
			byParent[c.Parent] = append(byParent[c.Parent], c)
		} else {
			roots = append(roots, c)
		}
	}

	rows := []panelRow{{theme.Heading, "  fan-out"}}
	shown := 0
	var walk func(c session.FanoutChild, depth int)
	walk = func(c session.FanoutChild, depth int) {
		// Cap total rows and guard the depth so a malformed parent cycle cannot loop the
		// walk; a bounded panel is worth more than a perfect render of pathological data.
		if shown >= fanoutRows || depth > 8 {
			return
		}
		rows = append(rows, fanoutRow(c, depth))
		shown++
		for _, kid := range byParent[c.ID] {
			walk(kid, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	if shown < len(children) {
		rows = append(rows, panelRow{theme.Muted, fmt.Sprintf("  (%d more)", len(children)-shown)})
	}
	return rows
}

// fanoutRow renders one fan-out child as an indented, state-styled row: a spinner mark
// while it runs, a check when it folds back clean, a cross when it fails. The objective
// names what the child was delegated, and a parenthetical meta group carries its turn
// count, its own trust level, and any governance block it hit, so a child's per-child
// posture reads on its own row. A folded result trails a finished child so the outcome
// reads without opening the record.
func fanoutRow(c session.FanoutChild, depth int) panelRow {
	indent := "  " + strings.Repeat("  ", depth+1)
	obj := strings.TrimSpace(c.Objective)
	if obj == "" {
		obj = "child"
	}
	tail := fmt.Sprintf(" %s (%s)", obj, childMeta(c))
	switch c.State {
	case session.FanoutDone:
		text := indent + "✓" + tail
		if r := summarizeResult(c.Result); r != "" {
			text += " -> " + r
		}
		return panelRow{theme.Admitted, text}
	case session.FanoutFailed:
		text := indent + "✗" + tail
		if r := summarizeResult(c.Result); r != "" {
			text += " -> " + r
		}
		return panelRow{theme.Rejected, text}
	default:
		return panelRow{theme.StatusBusy, indent + "..." + tail}
	}
}

// childMeta renders a fan-out child's parenthetical meta group: its turn count always,
// then its own trust level and a blocked-action count when it has them, so a child that
// hit a governance boundary shows the block on its own row (e.g. "2t, agent, 1 blocked")
// and a sibling that did not stays clean. It is joined into the row by fanoutRow.
func childMeta(c session.FanoutChild) string {
	segs := []string{fmt.Sprintf("%dt", c.Turns)}
	if c.Trust != "" {
		segs = append(segs, c.Trust)
	}
	if c.Blocked > 0 {
		segs = append(segs, fmt.Sprintf("%d blocked", c.Blocked))
	}
	return strings.Join(segs, ", ")
}

// summarizeResult flattens a child's folded result to one tidy line for its tree row:
// runs of whitespace and newlines collapse to single spaces and a long result is clipped,
// since the panel row is a single line and the full text lives in the sealed record.
func summarizeResult(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const maxLen = 40
	if len(s) > maxLen {
		s = strings.TrimSpace(s[:maxLen-3]) + "..."
	}
	return s
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
