package main

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/input"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/session"
)

// panelText renders the panel and joins its rows for substring assertions, so a test
// reads the panel's content without caring about the theme's escape codes.
func panelText(p *govPanel, width int) string {
	return strings.Join(p.Render(width), "\n")
}

// TestGovPanelHiddenRendersNothing proves a closed panel draws no rows, so it can sit
// permanently in the live region without showing until the user opens it.
func TestGovPanelHiddenRendersNothing(t *testing.T) {
	p := &govPanel{th: theme.Default()}
	p.set(session.Project([]session.Event{{Kind: session.KindActionAdmitted, Action: "bash", Call: 1, Trust: "agent"}}))
	if rows := p.Render(80); len(rows) != 0 {
		t.Fatalf("closed panel rendered %d rows, want 0", len(rows))
	}
}

// TestGovPanelShowsPosture proves an open panel names the run's governance posture: the
// goal, record state, trust level, action tallies, budget, and each recent action with
// its outcome, the substance the one-line badge only summarizes.
func TestGovPanelShowsPosture(t *testing.T) {
	p := &govPanel{th: theme.Default()}
	p.toggle()
	p.set(session.Project([]session.Event{
		{Kind: session.KindSessionStarted, Text: "ship the feature"},
		{Kind: session.KindActionAdmitted, Action: "bash", Call: 1, Trust: "agent"},
		{Kind: session.KindActionCompleted, Call: 1},
		{Kind: session.KindActionRejected, Action: "write_file", Call: 2, Trust: "model", Fault: "capability_denied"},
		{Kind: session.KindTurnCompleted, Usage: &session.Usage{InputTokens: 100, OutputTokens: 20}},
	}))
	got := panelText(p, 80)
	for _, want := range []string{
		"governance", "goal: ship the feature", "record: recording", "trust: model",
		"1 done", "1 blocked", "budget:", "✓ bash (agent)", "✗ write_file (model) blocked: capability_denied",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("panel missing %q in:\n%s", want, got)
		}
	}
}

// TestGovPanelRunningAction proves an admitted action with no outcome yet shows as
// running, so the panel distinguishes an in-flight governed call from a finished one.
func TestGovPanelRunningAction(t *testing.T) {
	p := &govPanel{th: theme.Default()}
	p.toggle()
	p.set(session.Project([]session.Event{{Kind: session.KindActionAdmitted, Action: "fetch", Call: 1, Trust: "model"}}))
	got := panelText(p, 80)
	if !strings.Contains(got, "... fetch (model)") {
		t.Errorf("panel did not show fetch as running:\n%s", got)
	}
	if !strings.Contains(got, "1 running") {
		t.Errorf("actions summary missing running count:\n%s", got)
	}
}

// TestGovPanelLedgerBounded proves the panel lists at most ledgerRows actions and notes
// how many older ones it dropped, so its height is bounded and the omission is visible.
func TestGovPanelLedgerBounded(t *testing.T) {
	p := &govPanel{th: theme.Default()}
	p.toggle()
	proj := session.NewProjection()
	for i := range ledgerRows + 5 {
		proj = session.Reduce(proj, session.Event{Kind: session.KindActionCompleted, Action: "step", Call: int64(i + 1)})
	}
	p.set(proj)
	got := panelText(p, 100)
	if !strings.Contains(got, "(5 earlier)") {
		t.Errorf("panel did not note the dropped entries:\n%s", got)
	}
	if n := strings.Count(got, "✓ step"); n != ledgerRows {
		t.Errorf("panel showed %d ledger rows, want %d", n, ledgerRows)
	}
}

// TestGovPanelToggleKey proves Ctrl+O opens and closes the panel and Escape closes it,
// so the overlay's key contract holds through the host.
func TestGovPanelToggleKey(t *testing.T) {
	host, _ := newHostForTest(t, constModel{text: "ok"})
	ctrlO := input.Key{Mods: input.ModCtrl, Code: 'o'}

	if !host.key(ctrlO) {
		t.Fatal("Ctrl+O was not claimed by the host")
	}
	if !host.panel.isOpen() {
		t.Fatal("Ctrl+O did not open the panel")
	}
	host.key(ctrlO)
	if host.panel.isOpen() {
		t.Fatal("a second Ctrl+O did not close the panel")
	}

	host.key(ctrlO) // open again
	host.onEsc()
	if host.panel.isOpen() {
		t.Fatal("Escape did not close the open panel")
	}
}
