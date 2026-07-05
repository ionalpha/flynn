package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

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

// TestGovPanelFanoutTree proves an open panel renders the run's fan-out tree: each child
// under its parent with its state, turn count, and folded result, so a delegating run's
// shape reads straight from the projection.
func TestGovPanelFanoutTree(t *testing.T) {
	p := &govPanel{th: theme.Default()}
	p.toggle()
	p.set(session.Project([]session.Event{
		{Kind: session.KindSessionStarted, Text: "research the topic"},
		{Kind: session.KindChildSpawned, Goal: "root", Child: "a", Text: "gather sources"},
		{Kind: session.KindChildSpawned, Goal: "root", Child: "b", Text: "draft outline"},
		{Kind: session.KindChildSpawned, Goal: "a", Child: "a1", Text: "read paper"},
		{Kind: session.KindTurnCompleted, Goal: "a"},
		{Kind: session.KindChildCompleted, Child: "a1", Result: "found\n three refs"},
		{Kind: session.KindChildCompleted, Child: "b", Result: "outline ready", IsError: true},
	}))
	got := panelText(p, 100)
	for _, want := range []string{
		"fan-out",
		"... gather sources (1t)", // a: running, one turn advanced
		"✗ draft outline (0t) -> outline ready",
		"✓ read paper (0t) -> found three refs", // a1 nested under a, newlines collapsed
	} {
		if !strings.Contains(got, want) {
			t.Errorf("panel missing %q in:\n%s", want, got)
		}
	}
	// a1 must render indented deeper than its parent a (strip the theme's escape codes
	// first so the leading spaces are the row's own indentation).
	lines := strings.Split(got, "\n")
	depthOf := func(sub string) int {
		for _, ln := range lines {
			if plain := ansi.Strip(ln); strings.Contains(plain, sub) {
				return len(plain) - len(strings.TrimLeft(plain, " "))
			}
		}
		return -1
	}
	if da, da1 := depthOf("gather sources"), depthOf("read paper"); da1 <= da {
		t.Errorf("nested child not indented deeper: parent indent %d, child indent %d", da, da1)
	}
}

// TestGovPanelFanoutPerChildGovernance proves each child's tree row carries its own
// governance posture: a child that hit a capability denial shows its trust and a blocked
// count on its row, while a sibling that ran clean shows its trust with no block.
func TestGovPanelFanoutPerChildGovernance(t *testing.T) {
	p := &govPanel{th: theme.Default()}
	p.toggle()
	p.set(session.Project([]session.Event{
		{Kind: session.KindSessionStarted, Text: "split the work"},
		{Kind: session.KindChildSpawned, Goal: "root", Child: "a", Text: "risky write"},
		{Kind: session.KindChildSpawned, Goal: "root", Child: "b", Text: "safe read"},
		{Kind: session.KindActionAdmitted, Goal: "a", Call: 1, Action: "write_file", Trust: "model"},
		{Kind: session.KindActionRejected, Goal: "a", Call: 1, Action: "write_file", Trust: "model", Fault: "capability_denied"},
		{Kind: session.KindActionAdmitted, Goal: "b", Call: 2, Action: "read_file", Trust: "agent"},
		{Kind: session.KindActionCompleted, Goal: "b", Call: 2, Action: "read_file", Trust: "agent"},
	}))
	got := panelText(p, 100)
	for _, want := range []string{
		"... risky write (0t, model, 1 blocked)", // child a: its own trust and the block
		"... safe read (0t, agent)",              // child b: its own trust, no block
	} {
		if !strings.Contains(got, want) {
			t.Errorf("panel missing %q in:\n%s", want, got)
		}
	}
	// The block must not bleed onto the clean sibling's row.
	for _, ln := range strings.Split(got, "\n") {
		if strings.Contains(ln, "safe read") && strings.Contains(ln, "blocked") {
			t.Errorf("clean sibling row shows a block: %q", ln)
		}
	}
}

// TestGovPanelFanoutPerChildSeal proves a folded child's tree row shows its seal state
// once the run is sealed, and its verified state once the run is verified, so the tree
// carries each child's integrity posture alongside its governance.
func TestGovPanelFanoutPerChildSeal(t *testing.T) {
	evs := []session.Event{
		{Kind: session.KindSessionStarted, Text: "split the work"},
		{Kind: session.KindChildSpawned, Goal: "root", Child: "a", Text: "gather sources"},
		{Kind: session.KindChildCompleted, Child: "a", Result: "done"},
	}

	// After the run seals, the folded child's row reads sealed.
	sealed := &govPanel{th: theme.Default()}
	sealed.toggle()
	sealed.set(session.Project(append(append([]session.Event(nil), evs...), session.Event{Kind: session.KindRecordSealed})))
	if got := panelText(sealed, 100); !strings.Contains(got, "gather sources (0t, sealed)") {
		t.Errorf("sealed child row missing its seal state in:\n%s", got)
	}

	// After the run verifies, the row reads verified.
	verified := &govPanel{th: theme.Default()}
	verified.toggle()
	verified.set(session.Project(append(append([]session.Event(nil), evs...),
		session.Event{Kind: session.KindRecordSealed}, session.Event{Kind: session.KindRecordVerified})))
	if got := panelText(verified, 100); !strings.Contains(got, "gather sources (0t, verified)") {
		t.Errorf("verified child row missing its verified state in:\n%s", got)
	}
}

// TestGovPanelFanoutBounded proves the tree lists at most fanoutRows children and notes
// how many more it omitted, so a wide fan-out keeps the panel a fixed height.
func TestGovPanelFanoutBounded(t *testing.T) {
	p := &govPanel{th: theme.Default()}
	p.toggle()
	proj := session.NewProjection()
	total := fanoutRows + 4
	for i := range total {
		proj = session.Reduce(proj, session.Event{
			Kind: session.KindChildSpawned, Goal: "root",
			Child: string(rune('a' + i)), Text: "child work",
		})
	}
	p.set(proj)
	got := panelText(p, 100)
	if n := strings.Count(got, "... child work"); n != fanoutRows {
		t.Errorf("tree showed %d children, want %d", n, fanoutRows)
	}
	if !strings.Contains(got, "(4 more)") {
		t.Errorf("tree did not note the omitted children:\n%s", got)
	}
}

// TestGovPanelNoFanoutNoSection proves a run that never delegated draws no fan-out
// section, so the panel stays lean for an ordinary single conversation.
func TestGovPanelNoFanoutNoSection(t *testing.T) {
	p := &govPanel{th: theme.Default()}
	p.toggle()
	p.set(session.Project([]session.Event{{Kind: session.KindSessionStarted, Text: "just chat"}}))
	if got := panelText(p, 80); strings.Contains(got, "fan-out") {
		t.Errorf("panel showed a fan-out section for a run with no children:\n%s", got)
	}
}

// TestGovPanelEgress proves the panel names the run's egress posture: a counts line and
// a row per recent destination, with a blocked destination showing its reason, so the
// run's outbound network decisions read alongside its governed actions.
func TestGovPanelEgress(t *testing.T) {
	p := &govPanel{th: theme.Default()}
	p.toggle()
	p.set(session.Project([]session.Event{
		{Kind: session.KindEgressDecision, Host: "api.example.com", Allowed: true, Reason: "public"},
		{Kind: session.KindEgressDecision, Host: "api.example.com", Allowed: true, Reason: "public"},
		{Kind: session.KindEgressDecision, Host: "10.0.0.1", Allowed: false, Reason: "private or reserved address"},
	}))
	got := panelText(p, 80)
	for _, want := range []string{
		"egress: 2 allowed, 1 blocked",
		"✓ api.example.com",
		"✗ 10.0.0.1 (private or reserved address)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("egress section missing %q in:\n%s", want, got)
		}
	}
}

// TestGovPanelNoEgressNoSection proves a run that made no egress decision draws no
// egress row, so a local, loopback-model run's panel stays lean.
func TestGovPanelNoEgressNoSection(t *testing.T) {
	p := &govPanel{th: theme.Default()}
	p.toggle()
	p.set(session.Project([]session.Event{{Kind: session.KindSessionStarted, Text: "local run"}}))
	if got := panelText(p, 80); strings.Contains(got, "egress:") {
		t.Errorf("panel showed an egress section for a run with no egress:\n%s", got)
	}
}

// TestGovPanelEgressBounded proves the panel lists at most egressRows destinations and
// notes how many older ones it dropped, while the counts line still tallies every dial.
func TestGovPanelEgressBounded(t *testing.T) {
	p := &govPanel{th: theme.Default()}
	p.toggle()
	proj := session.NewProjection()
	total := egressRows + 3
	for i := range total {
		proj = session.Reduce(proj, session.Event{
			Kind: session.KindEgressDecision, Host: "h" + string(rune('a'+i)), Allowed: true, Reason: "public",
		})
	}
	p.set(proj)
	got := panelText(p, 100)
	if n := strings.Count(got, "✓ h"); n != egressRows {
		t.Errorf("egress showed %d destinations, want %d", n, egressRows)
	}
	if !strings.Contains(got, "(3 earlier)") {
		t.Errorf("egress did not note the omitted destinations:\n%s", got)
	}
	if !strings.Contains(got, "egress: "+strings.TrimSpace("7 allowed")) {
		t.Errorf("egress counts did not tally every dial:\n%s", got)
	}
}

// TestGovPanelEgressBoundedToWidth proves every egress row is truncated to the panel
// width, so a long hostname never overflows the live region.
func TestGovPanelEgressBoundedToWidth(t *testing.T) {
	p := &govPanel{th: theme.Default()}
	p.toggle()
	long := strings.Repeat("verylonghost.example.", 8) + "com"
	p.set(session.Project([]session.Event{
		{Kind: session.KindEgressDecision, Host: long, Allowed: false, Reason: "private or reserved address"},
	}))
	const width = 40
	for _, row := range p.Render(width) {
		if w := ansi.StringWidth(row); w > width {
			t.Fatalf("egress row width %d exceeds %d: %q", w, width, row)
		}
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
