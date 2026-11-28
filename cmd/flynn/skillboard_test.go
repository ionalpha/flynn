package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/skill/skillab"
	"github.com/ionalpha/flynn/usage"
)

const claudeStream = `warning: something printed before the stream
{"type":"system","subtype":"init","model":"claude-sonnet-5-5"}
{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"looking"},{"type":"tool_use","name":"Read"}]}}
{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","name":"Grep"}]}}
{"type":"user","message":{"content":[{"type":"tool_result"}]}}
{"type":"assistant","message":{"id":"m2","content":[{"type":"tool_use","name":"Edit"}]}}
{"type":"assistant","message":{"id":"m3","content":[{"type":"tool_use","name":"Write"}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":4,"result":"fixed it","total_cost_usd":0.0123,
 "usage":{"input_tokens":1,"output_tokens":2},
 "modelUsage":{"claude-sonnet-5-5":{"inputTokens":100,"outputTokens":20,"cacheReadInputTokens":300,"cacheCreationInputTokens":40},
               "claude-haiku-5-5":{"inputTokens":10,"outputTokens":1,"cacheReadInputTokens":0,"cacheCreationInputTokens":0}}}
`

// The result event is one JSON line in the real stream; the canned one is joined so
// the test can lay it out readably.
func claudeStreamBytes() []byte {
	lines := strings.Split(claudeStream, "\n")
	var b strings.Builder
	for i, l := range lines {
		b.WriteString(l)
		if i+1 < len(lines) && !strings.HasPrefix(lines[i+1], " ") {
			b.WriteString("\n")
		}
	}
	return []byte(b.String())
}

func TestParseClaudeCodeStream(t *testing.T) {
	run := parseClaudeCodeStream(claudeStreamBytes(), "claude-sonnet-5-5")
	if !run.finished || run.isError || run.subtype != "success" || run.result != "fixed it" {
		t.Fatalf("result = %+v", run)
	}
	// m1 arrived as two events and is one turn; m2 is the first that edited a file.
	if run.turns != 4 || run.firstUseful != 2 {
		t.Errorf("turns %d, first useful %d; want 4 and 2", run.turns, run.firstUseful)
	}
	if !run.hasCost || run.reportedUSD != 0.0123 {
		t.Errorf("reported cost = %v, %t", run.reportedUSD, run.hasCost)
	}
	sonnet := run.perModel["claude-sonnet-5-5"]
	// Anthropic's input excludes cache reads; usage.Tokens' Input includes them.
	if sonnet.Input.N != 400 || sonnet.Output.N != 20 || sonnet.CacheRead.N != 300 || sonnet.CacheWrite.N != 40 {
		t.Errorf("sonnet tokens = %+v", sonnet)
	}
	if len(run.perModel) != 2 {
		t.Errorf("per model = %v, want both models the run used", run.perModel)
	}
}

func TestParseClaudeCodeStreamWithoutAResult(t *testing.T) {
	run := parseClaudeCodeStream([]byte(`{"type":"assistant","message":{"id":"a","content":[]}}`+"\n"), "m")
	if run.finished || run.turns != 1 || run.firstUseful != 0 {
		t.Fatalf("an interrupted stream = %+v, want unfinished with one turn and no edit", run)
	}
}

func TestParseCodexStream(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"thread.started","thread_id":"t"}`,
		`{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}`,
		`{"type":"item.completed","item":{"type":"command_execution"}}`,
		`{"type":"item.completed","item":{"type":"file_change"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":500,"cached_input_tokens":200,"output_tokens":50}}`,
	}, "\n")
	run := parseCodexStream([]byte(stream))
	if !run.finished || run.failure != "" || run.lastMessage != "done" {
		t.Fatalf("run = %+v", run)
	}
	if run.steps != 3 || run.firstUseful != 2 {
		t.Errorf("steps %d, first useful %d; want 3 and 2 (reasoning is not a step)", run.steps, run.firstUseful)
	}
	if run.tokens.Input.N != 500 || run.tokens.CacheRead.N != 200 || run.tokens.Output.N != 50 {
		t.Errorf("tokens = %+v", run.tokens)
	}

	failed := parseCodexStream([]byte(`{"type":"turn.failed","error":{"message":"401 unauthorized"}}`))
	if failed.finished || failed.failure != "401 unauthorized" {
		t.Errorf("a failed turn = %+v", failed)
	}
}

func boardPrices(t *testing.T) *usage.Table {
	t.Helper()
	table, err := usage.LoadTable(strings.NewReader(`{"prices":[
		{"provider":"anthropic","model":"claude-sonnet-5-5","effective":"2026-01-01","currency":"USD","input":"2","output":"10","cacheRead":"0.1","cacheWrite":"2.5","source":"test"},
		{"provider":"openai","model":"gpt-5.5","effective":"2026-01-01","currency":"USD","input":"5","output":"30","cacheRead":"0.5","cacheWrite":"0","source":"test"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return table
}

// One model the table cannot price makes the whole attempt unpriced: a sum of the
// priced models alone would read as the attempt's cost.
func TestPriceClaudeCodeIsUnpricedWhenAnyModelIs(t *testing.T) {
	table := boardPrices(t)
	run := parseClaudeCodeStream(claudeStreamBytes(), "claude-sonnet-5-5")
	if c := priceClaudeCode(run, table, clock.System{}.Now()); c.Known() || c.Unpriced == 0 {
		t.Fatalf("haiku has no row, yet the attempt was priced: %+v", c)
	}
	delete(run.perModel, "claude-haiku-5-5")
	c := priceClaudeCode(run, table, clock.System{}.Now())
	if !c.Known() || c.Money <= 0 || c.Turns != 4 {
		t.Fatalf("a fully priced attempt = %+v", c)
	}
	if c := priceClaudeCode(claudeCodeRun{turns: 2}, table, clock.System{}.Now()); c.Known() {
		t.Errorf("an attempt with no reported usage was priced as free: %+v", c)
	}
}

func TestCLIEnvDropsTheDevelopersAccount(t *testing.T) {
	base := []string{"PATH=/bin", "ANTHROPIC_API_KEY=mine", "anthropic_auth_token=mine", "CODEX_HOME=/home/me/.codex", "OPENAI_API_KEY=mine", "CLAUDE_CONFIG_DIR=/home/me/.claude", "HOME=/home/me"}
	got := strings.Join(cliEnv(base, map[string]string{"ANTHROPIC_API_KEY": "board", "CLAUDE_CONFIG_DIR": "/tmp/fresh"}), " ")
	if strings.Contains(got, "mine") || strings.Contains(got, "/home/me/.") {
		t.Fatalf("the developer's account reached the arm: %s", got)
	}
	for _, want := range []string{"PATH=/bin", "HOME=/home/me", "ANTHROPIC_API_KEY=board", "CLAUDE_CONFIG_DIR=/tmp/fresh"} {
		if !strings.Contains(got, want) {
			t.Errorf("env lacks %s: %s", want, got)
		}
	}
}

// fakeCreds is a credential store holding only what a test put in it, so no test reads
// the developer's own.
type fakeCreds map[string]string

func (f fakeCreds) Lookup(_ context.Context, ref string) (secret.Text, error) {
	v, ok := f[ref]
	if !ok {
		return secret.Text{}, errors.New("no such key")
	}
	return secret.New(v), nil
}

func boardTestEnv(onPath map[string]bool, creds fakeCreds) boardEnv {
	return boardEnv{
		lookPath: func(name string) (string, error) {
			if onPath[name] {
				return "/fake/" + name, nil
			}
			return "", errors.New("not found")
		},
		creds: creds,
		native: func(context.Context) (llm.Model, harness.Plan, string, error) {
			return nil, harness.Plan{}, "", errors.New("no model is configured")
		},
		driven: func(context.Context, string, string, string) (*externAgent, error) {
			return nil, errors.New("governed egress is not available on this platform")
		},
	}
}

// An arm that cannot run is skipped with a reason a person can act on, and never
// fails the board.
func TestPrepareArmsSkipsWhatCannotRunAndSaysWhy(t *testing.T) {
	f, err := parseBoardArgs(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	anthropicRef := mustKeyRef(t, "anthropic")
	for _, tc := range []struct {
		name   string
		path   map[string]bool
		creds  fakeCreds
		ready  []string
		reason map[string]string
	}{
		{
			name: "nothing installed",
			reason: map[string]string{
				armFlynn: "no model is configured", armFlynnClaude: "governed egress", armFlynnCodex: "governed egress",
				armClaudeCode: "not installed", armCodex: "not installed",
			},
		},
		{
			name:   "installed without keys",
			path:   map[string]bool{"claude": true, "codex": true},
			reason: map[string]string{armClaudeCode: "flynn auth set anthropic", armCodex: "flynn auth set openai"},
		},
		{
			name:  "claude installed with a key",
			path:  map[string]bool{"claude": true},
			creds: fakeCreds{anthropicRef: "sk-test"},
			ready: []string{armClaudeCode},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arms := prepareArms(context.Background(), boardTestEnv(tc.path, tc.creds), f, nil, governed.ShellPolicy{})
			if len(arms) != len(boardArmNames) {
				t.Fatalf("%d arms, want all %d listed", len(arms), len(boardArmNames))
			}
			for _, a := range arms {
				ready := !a.status.Skipped
				if ready != containsName(tc.ready, a.status.Name) {
					t.Errorf("%s ready = %t (%s)", a.status.Name, ready, a.status.Reason)
				}
				if want, ok := tc.reason[a.status.Name]; ok && !strings.Contains(a.status.Reason, want) {
					t.Errorf("%s reason %q, want it to say %q", a.status.Name, a.status.Reason, want)
				}
			}
		})
	}
}

func containsName(xs []string, x string) bool {
	for _, y := range xs {
		if x == y {
			return true
		}
	}
	return false
}

func mustKeyRef(t *testing.T, name string) string {
	t.Helper()
	ref, ok := provider.KeyRef(name)
	if !ok {
		t.Fatalf("no key ref for %s", name)
	}
	return ref
}

// writeFixtures gives each named fixture of skill under root a one-file directory.
func writeFixtures(t *testing.T, root, skill string, names ...string) {
	t.Helper()
	for _, n := range names {
		dir := filepath.Join(root, skill, "fixtures", n)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "README"), []byte(n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWithoutHomeHidesTheAccountName(t *testing.T) {
	got := withoutHome(`sign in at C:\Users\someone\AppData\flynn\claude`, `C:\Users\someone`)
	if got != `sign in at ~\AppData\flynn\claude` {
		t.Errorf("got %q", got)
	}
	if got := withoutHome("x", ""); got != "x" {
		t.Errorf("an unknown home changed the reason: %q", got)
	}
}

func TestBoardEntriesSelectsAndRefusesUnknownNames(t *testing.T) {
	root := writeExerciseSet(t, "alpha", "[one] do one | exit 0\n[two] do two | exit 0\n")
	writeFixtures(t, root, "alpha", "one", "two")
	writeFixtures(t, root, "beta", "three")
	if err := os.WriteFile(filepath.Join(root, "beta", skillab.ExercisesFile), []byte("[three] do three | exit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	all, err := boardEntries(root, "", "")
	if err != nil || len(all) != 3 {
		t.Fatalf("every skill: %d entries, %v; want 3", len(all), err)
	}
	only, err := boardEntries(root, "", "alpha/two,beta/three")
	if err != nil || len(only) != 2 {
		t.Fatalf("--only: %d entries, %v", len(only), err)
	}
	if _, f := only[0].Key(); f != "two" {
		t.Errorf("first selected fixture = %s", f)
	}
	if _, err := boardEntries(root, "", "alpha/missing"); err == nil {
		t.Error("an --only name that matches nothing was accepted")
	}
	if _, err := boardEntries(root, "", "alpha"); err == nil {
		t.Error("an --only name without a fixture was accepted")
	}
}

// The verifier alone decides solved: the fake arm claims success every time, and only
// the exercise whose verifier passes is counted. The board is written even though the
// spend limit stopped it.
func TestPlayBoardGradesByTheVerifierAndWritesTheBoard(t *testing.T) {
	root := writeExerciseSet(t, "alpha", "[pass] do it | exit 0\n[fail] do it | exit 1\n")
	writeFixtures(t, root, "alpha", "pass", "fail")
	entries, err := boardEntries(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f := boardFlags{repeats: 2, timeout: time.Minute, out: filepath.Join(dir, "b.json"), markdown: filepath.Join(dir, "b.md")}
	arm := boardArm{
		status: skillab.ArmStatus{Name: "fake"},
		run: func(context.Context, string, string) (boardAttempt, error) {
			return boardAttempt{report: "all fixed", cost: skillab.Cost{Turns: 3, Money: 1_000_000_000, Currency: "USD"}, firstUseful: 2}, nil
		},
	}
	skipped := boardArm{status: skillab.ArmStatus{Name: "absent", Skipped: true, Reason: "not installed"}}
	var out bytes.Buffer
	board := skillab.Board{Generated: clock.System{}.Now()}
	limit := skillab.Limit{Money: 3_000_000_000, Currency: "USD"}
	if err := playBoard(context.Background(), &out, &board, entries, []boardArm{arm, skipped}, f, limit); err != nil {
		t.Fatalf("play: %v\n%s", err, out.String())
	}
	data, err := os.ReadFile(f.out)
	if err != nil {
		t.Fatal(err)
	}
	got, err := skillab.ReadBoard(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got.Planned != 4 || len(got.Runs) != 3 || got.Stopped == "" {
		t.Fatalf("planned %d, ran %d, stopped %q; want 4, 3 and a reason", got.Planned, len(got.Runs), got.Stopped)
	}
	for _, r := range got.Runs {
		if r.Solved != (r.Fixture == "pass") {
			t.Errorf("%s repeat %d solved = %t; only the verifier decides", r.Fixture, r.Repeat, r.Solved)
		}
		if r.Turns != 3 || r.FirstUseful != 2 || !r.Priced {
			t.Errorf("row = %+v", r)
		}
	}
	md, err := os.ReadFile(f.markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "| absent | - | skipped | not installed |") || !strings.Contains(out.String(), "skipping absent") {
		t.Errorf("the skipped arm is not reported:\n%s", md)
	}
}

func TestParseBoardArgsRefusesUnknownArms(t *testing.T) {
	var out bytes.Buffer
	if _, err := parseBoardArgs([]string{"--arms", "flynn,gemini"}, &out); err == nil {
		t.Error("an unknown arm was accepted")
	}
	if _, err := parseBoardArgs([]string{"stray"}, &out); err == nil {
		t.Error("a stray argument was accepted")
	}
}

func TestSkillBoardIsRoutedThroughTheSkillCommand(t *testing.T) {
	got := runCLIIn(t, t.TempDir(), "skill", "board", "stray")
	if got.code != 2 || !strings.Contains(got.stderr, "usage: flynn skill board") {
		t.Fatalf("exit %d, stderr %q, want 2 and the board usage", got.code, got.stderr)
	}
}
