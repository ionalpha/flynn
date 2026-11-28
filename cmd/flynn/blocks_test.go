package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/projectctx"
	"github.com/ionalpha/flynn/internal/tui/screen"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/session"
)

var sgr = regexp.MustCompile("\x1b\\[[0-9;:]*m")

// unstyled drops color and attribute sequences and trailing padding, leaving what a
// reader sees.
func unstyled(lines []string) string {
	rows := strings.Split(sgr.ReplaceAllString(strings.Join(lines, "\n"), ""), "\n")
	for i := range rows {
		rows[i] = strings.TrimRight(rows[i], " ")
	}
	return strings.Join(rows, "\n") + "\n"
}

// representativeTurn is one exchange: a prompt, prose, two tool calls with one
// failing, a notice, and the status row.
func representativeTurn(th *theme.Theme, width int) []string {
	v := newTranscriptView(th)
	out := append([]string{""}, layBlock(th, blockUser, "find out why the build fails and fix it, then run the tests again to make sure nothing else broke", width)...)
	out = append(out, "")
	events := []session.Event{
		{Kind: session.KindAssistant, Text: "I will read the module file first, then run the build to see the failure for myself."},
		{Kind: session.KindToolCall, Tool: "read", Input: json.RawMessage(`{"path":"go.mod"}`)},
		{Kind: session.KindToolResult, Tool: "read", Result: "module example.com/app\n\ngo 1.24\n"},
		{Kind: session.KindToolCall, Tool: "bash", Input: json.RawMessage(`{"command":"go build ./..."}`)},
		{Kind: session.KindToolResult, Tool: "bash", Result: "main.go:12:2: undefined: handler; did you mean Handler, which is declared in server.go of this package", IsError: true},
		{Kind: session.KindSelection, Reason: "bare", Text: "skills offered: none\ntools granted: read, bash"},
	}
	p := session.NewProjection()
	for _, ev := range events {
		p = session.Reduce(p, ev)
		out = append(out, v.lines(ev, width)...)
	}
	p = session.Reduce(p, session.Event{Kind: session.KindTurnCompleted, Usage: &session.Usage{InputTokens: 1200, OutputTokens: 300}})
	return append(out, "", statusBadge(th, p, false, 0, width))
}

func TestTurnGolden(t *testing.T) {
	for _, width := range []int{80, 120} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			got := unstyled(representativeTurn(theme.Default(), width))
			for _, row := range strings.Split(got, "\n") {
				if screen.Width(row) > width {
					t.Errorf("row is wider than %d: %q", width, row)
				}
			}
			path := filepath.Join("testdata", "turn_"+strconv.Itoa(width)+".golden")
			if os.Getenv("FLYNN_UPDATE_GOLDEN") != "" {
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
				t.Fatalf("turn at %d columns changed (FLYNN_UPDATE_GOLDEN=1 rewrites):\n%s", width, got)
			}
		})
	}
}

func TestStatusRowDropsHintsToFit(t *testing.T) {
	all := fitHints(statusHints(false, 0), 0)
	for room := screen.Width(all); room >= 1; room-- {
		if got := fitHints(statusHints(false, 0), room); screen.Width(got) > room {
			t.Fatalf("hints overflow %d cells: %q", room, got)
		}
	}
	if got := fitHints(statusHints(false, 0), 30); got != "enter sends · /help" {
		t.Fatalf("hints at 30 cells = %q", got)
	}
	for _, width := range []int{12, 24, 40, 80} {
		row := sgr.ReplaceAllString(statusBadge(theme.Default(), session.NewProjection(), true, 2, width), "")
		if screen.Width(row) > width {
			t.Fatalf("status row overflows %d columns: %q", width, row)
		}
	}
}

// declaredKinds reads the event kinds from the session package's source, so a new
// kind fails the table below until someone says where it is shown.
func declaredKinds(t *testing.T) []session.Kind {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "session", "event.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []session.Kind
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Kind" {
			return true
		}
		for _, v := range vs.Values {
			if lit, ok := v.(*ast.BasicLit); ok {
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				kinds = append(kinds, session.Kind(s))
			}
		}
		return true
	})
	return kinds
}

// TestEveryEventKindStaysVisible pins where each kind of session event reaches the
// user: a line in the chat transcript, a change to the status badge or governance
// panel, and a line from the flat renderer of `flynn goal` and `flynn inspect`.
// An empty row is a kind these renderers do not draw (approval requests have their
// own overlay).
func TestEveryEventKindStaysVisible(t *testing.T) {
	type shown struct {
		ev                    session.Event
		chat, status, flat, v bool // v: flat only when verbose
	}
	use := &session.Usage{InputTokens: 10, OutputTokens: 5}
	table := map[session.Kind]shown{
		session.KindSessionStarted:    {ev: session.Event{Text: "mark"}, status: true, flat: true},
		session.KindTurnStarted:       {ev: session.Event{Turn: 1}, flat: true},
		session.KindAssistant:         {ev: session.Event{Text: "mark"}, chat: true, v: true},
		session.KindToolCall:          {ev: session.Event{Tool: "mark"}, chat: true, flat: true},
		session.KindToolResult:        {ev: session.Event{Tool: "mark", Result: "mark"}, chat: true, v: true},
		session.KindTurnCompleted:     {ev: session.Event{Usage: use}, status: true, flat: true},
		session.KindChildSpawned:      {ev: session.Event{Child: "c1", Text: "mark"}, status: true},
		session.KindChildCompleted:    {ev: session.Event{Child: "c0", Result: "mark"}, status: true},
		session.KindConverged:         {ev: session.Event{Text: "mark"}, chat: true, status: true, flat: true},
		session.KindStalled:           {ev: session.Event{Err: "mark"}, chat: true, status: true, flat: true},
		session.KindActionAdmitted:    {ev: session.Event{Action: "mark", Trust: "trusted"}, status: true},
		session.KindActionCompleted:   {ev: session.Event{Action: "mark", Fault: "transient"}, chat: true, status: true},
		session.KindActionRejected:    {ev: session.Event{Action: "mark", Fault: "budget_exhausted"}, chat: true, status: true},
		session.KindRecordSealed:      {status: true},
		session.KindRecordVerified:    {status: true},
		session.KindEgressDecision:    {ev: session.Event{Host: "mark"}, status: true},
		session.KindApprovalRequested: {},
		session.KindApprovalDecision:  {},
		session.KindPromptEnded:       {},
		session.KindClientAttached:    {},
		session.KindBudgetExtended:    {},
		session.KindProjectContext:    {ev: session.Event{Reason: "flag", Text: "AGENTS.md abc", Allowed: true}, chat: true, flat: true},
		session.KindSelection:         {ev: session.Event{Reason: "bare", Text: "mark"}, chat: true, flat: true},
	}

	kinds := declaredKinds(t)
	if len(kinds) != len(table) {
		t.Fatalf("session declares %d kinds, the table covers %d", len(kinds), len(table))
	}
	th := theme.Default()
	status := func(p session.Projection) string {
		panel := &govPanel{th: th}
		panel.toggle()
		panel.set(p)
		return statusBadge(th, p, false, 0, 0) + strings.Join(panel.Render(120), "\n")
	}
	base := session.Reduce(session.NewProjection(), session.Event{Kind: session.KindChildSpawned, Child: "c0", Text: "child"})
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			want, ok := table[k]
			if !ok {
				t.Fatalf("no row for %s", k)
			}
			ev := want.ev
			ev.Kind = k
			if got := len(newTranscriptView(th).lines(ev, 80)) > 0; got != want.chat {
				t.Errorf("chat transcript shows it = %v, want %v", got, want.chat)
			}
			if got := status(session.Reduce(base, ev)) != status(base); got != want.status {
				t.Errorf("badge or panel changes = %v, want %v", got, want.status)
			}
			var plain, verbose strings.Builder
			renderEvent(&plain, ev, false)
			renderEvent(&verbose, ev, true)
			if got := plain.Len() > 0; got != want.flat {
				t.Errorf("flat renderer shows it = %v, want %v", got, want.flat)
			}
			if got := verbose.Len() > 0; got != (want.flat || want.v) {
				t.Errorf("verbose flat renderer shows it = %v, want %v", got, want.flat || want.v)
			}
		})
	}
}

// TestHostileTextRendersInert sends terminal control codes through every string a
// model or tool controls and checks that none reaches the output. The theme's own
// styling is removed first, so any escape left over came from the event.
func TestHostileTextRendersInert(t *testing.T) {
	const evil = "a\x1b]0;owned\x07b\x1b[2Jc\u009b31md\re"
	input, err := json.Marshal(map[string]string{"command": evil, "path": evil, "content": evil, "old": evil, "new": evil + "x"})
	if err != nil {
		t.Fatal(err)
	}
	events := []session.Event{
		{Kind: session.KindSessionStarted, Text: evil},
		{Kind: session.KindAssistant, Text: evil},
		{Kind: session.KindAssistant, Text: "```\n" + evil + "\n```"},
		{Kind: session.KindToolCall, Tool: evil, Input: input},
		{Kind: session.KindToolCall, Tool: "write", Input: input},
		{Kind: session.KindToolCall, Tool: "edit", Input: input},
		{Kind: session.KindToolResult, Tool: evil, Result: evil},
		{Kind: session.KindToolResult, Tool: evil, Result: evil, IsError: true},
		{Kind: session.KindActionRejected, Action: evil, Fault: evil},
		{Kind: session.KindActionCompleted, Action: evil, Fault: evil},
		{Kind: session.KindEgressDecision, Host: evil, Reason: evil},
		{Kind: session.KindChildSpawned, Child: "c", Text: evil},
		{Kind: session.KindConverged, Text: evil},
		{Kind: session.KindStalled, Err: evil},
		{Kind: session.KindProjectContext, Reason: "flag", Text: evil},
		{Kind: session.KindSelection, Reason: evil, Text: evil},
	}
	th := theme.Default()
	check := func(where, s string) {
		t.Helper()
		s = sgr.ReplaceAllString(s, "")
		if i := strings.IndexAny(s, "\x1b\x07\r\u009b"); i >= 0 {
			t.Errorf("%s passes a control code through: %q", where, s)
		}
	}
	p := session.NewProjection()
	for _, ev := range events {
		p = session.Reduce(p, ev)
		check("chat "+string(ev.Kind), strings.Join(newTranscriptView(th).lines(ev, 80), "\n"))
		if line, ok := activityFor(ev); ok {
			check("activity", line)
		}
		for _, verbose := range []bool{false, true} {
			var b strings.Builder
			renderEvent(&b, ev, verbose)
			check("flat "+string(ev.Kind), b.String())
		}
	}
	panel := &govPanel{th: th}
	panel.toggle()
	panel.set(p)
	check("panel", strings.Join(panel.Render(200), "\n"))
	check("banner", strings.Join(sessionBanner(th, evil, "x", projectctx.Decision{}), "\n"))
}

func TestBlockHelpers(t *testing.T) {
	if got := resultSummary("\n\n"); got != "(no output)" {
		t.Errorf("empty result = %q", got)
	}
	if got := resultSummary("\n first \nsecond\n"); got != "first (+1 lines)" {
		t.Errorf("summary = %q", got)
	}
	if got := layRow(nil, blockToolResult, strings.Repeat("long ", 20), 20); screen.Width(got) > 20 || !strings.HasSuffix(got, "...") {
		t.Errorf("row = %q", got)
	}
	if got := toolHead(nil, "bash", strings.Repeat("x", 40), 20); screen.Width(got) > 20 || !strings.HasSuffix(got, "...") {
		t.Errorf("head = %q", got)
	}
	if got := toolHead(nil, "bash", "detail", 8); got != "→ bash" {
		t.Errorf("narrow head = %q", got)
	}
	if got := layBlock(nil, blockNotice, "ab", 1); len(got) != 2 {
		t.Errorf("one-cell wrap = %q", got)
	}

	home := filepath.Join(string(filepath.Separator)+"home", "u")
	for in, want := range map[string]string{
		home:                           "~",
		filepath.Join(home, "src"):     "~" + string(filepath.Separator) + "src",
		filepath.Dir(home):             filepath.Dir(home),
		filepath.Join(home, "..", "v"): filepath.Join(home, "..", "v"),
		"relative":                     "relative",
	} {
		if got := shortPathUnder(in, home); got != want {
			t.Errorf("shortPathUnder(%q) = %q, want %q", in, got, want)
		}
	}

	var b strings.Builder
	(&replSession{out: &b}).plainBlock(blockCancelled, "turn cancelled")
	if b.String() != "○ turn cancelled\n" {
		t.Errorf("plain block = %q", b.String())
	}

	if th := defaultTheme(func(string) string { return "1" }); th.Render(theme.Error, "x") != theme.Mono().Render(theme.Error, "x") {
		t.Error("NO_COLOR did not select the attribute-only theme")
	}
	if th := defaultTheme(func(string) string { return "" }); th.Render(theme.Error, "x") != theme.Default().Render(theme.Error, "x") {
		t.Error("without NO_COLOR the default theme applies")
	}
}

func TestProjectState(t *testing.T) {
	if got := projectState(projectctx.Decision{}); got != "no project files" {
		t.Errorf("none = %q", got)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := projectctx.Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := projectctx.Decision{Set: set}
	if got := projectState(d); !strings.HasPrefix(got, "project files skipped: ") || !strings.Contains(got, "AGENTS.md") {
		t.Errorf("skipped = %q", got)
	}
	d.Verdict = projectctx.Loaded
	if got := projectState(d); !strings.HasPrefix(got, "project files loaded: ") {
		t.Errorf("loaded = %q", got)
	}
}

// TestTabsKeepTheirColumns: tool and model text that aligns with tabs keeps its
// columns in a chat block and in the flat goal renderer, because a tab is expanded to
// the next stop before the block is wrapped and indented.
func TestTabsKeepTheirColumns(t *testing.T) {
	cases := []struct {
		name string
		got  func() string
		want []string
	}{
		{"indented shell output", func() string {
			return strings.Join(layBlock(nil, blockOutput, "total 8\n\tmain.go\n\t\tlines\t42", 80), "\n")
		}, []string{"  total 8", "          main.go", "                  lines   42"}},
		{"goal final answer", func() string {
			var b strings.Builder
			renderEvent(&b, session.Event{Kind: session.KindConverged, Text: "Done.\nfile\tstatus\nmain.go\tfixed"}, false)
			return b.String()
		}, []string{"file    status", "main.go fixed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.got()
			if strings.Contains(out, "\t") {
				t.Fatalf("a tab reached the terminal: %q", out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Fatalf("output %q missing %q", out, w)
				}
			}
		})
	}
}
