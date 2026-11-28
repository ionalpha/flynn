package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/skill/skillab"
)

// fakeCLIEnv makes this test binary act as an agent CLI, in the way its value names,
// so the bare arms are tested without a real CLI, key or account.
const fakeCLIEnv = "FLYNN_TEST_FAKE_AGENT_CLI"

func runFakeAgentCLI(mode string, args []string) int {
	login := len(args) > 0 && args[0] == "login"
	if !login {
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	switch mode {
	case "codex-no-credit":
		if login {
			return 0
		}
		_, _ = fmt.Fprintln(os.Stdout, `{"type":"turn.failed","error":{"message":"stream disconnected before completion: You have no credits remaining."}}`)
	case "codex-login-refused":
		_, _ = fmt.Fprintln(os.Stderr, "401 Unauthorized")
	case "codex-silent":
		if login {
			return 0
		}
	case "codex-ok":
		if login {
			return 0
		}
		_, _ = fmt.Fprintln(os.Stdout, `{"type":"item.completed","item":{"type":"file_change"}}`)
		_, _ = fmt.Fprintln(os.Stdout, `{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":1}}`)
		return 0
	case "claude-refused":
		_, _ = fmt.Fprintln(os.Stdout, `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"result":"API Error: 401 authentication_error"}`)
	case "claude-out-of-turns":
		_, _ = fmt.Fprintln(os.Stdout, `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":9}`)
	case "claude-crash":
	}
	return 1
}

// boardRow runs one attempt of arm on an exercise whose verifier passes untouched, the
// case where a refused run would otherwise count as a solve.
func boardRow(t *testing.T, arm boardArm) (skillab.BoardRun, skillab.Cost) {
	t.Helper()
	root := writeExerciseSet(t, "alpha", "[idle] change nothing | exit 0\n")
	writeFixtures(t, root, "alpha", "idle")
	entries, err := boardEntries(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	row, cost, err := boardTrial(context.Background(), &out, arm, entries[0], 1, boardFlags{timeout: time.Minute})
	if err != nil {
		t.Fatalf("trial: %v\n%s", err, out.String())
	}
	return row, cost
}

func TestBareArmsDoNotCountARefusalAsAnAttempt(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	creds := fakeCreds{mustKeyRef(t, "anthropic"): "sk-test", mustKeyRef(t, "openai"): "sk-test"}
	env := boardTestEnv(nil, creds)
	env.lookPath = func(string) (string, error) { return self, nil }
	f, err := parseBoardArgs(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode, arm string
		why       string // empty: the attempt is evaluated
	}{
		{"codex-no-credit", armCodex, "no credits remaining"},
		{"codex-login-refused", armCodex, "401 Unauthorized"},
		{"codex-silent", armCodex, "finished no turn"},
		{"codex-ok", armCodex, ""},
		{"claude-refused", armClaudeCode, "API Error: 401"},
		{"claude-crash", armClaudeCode, "printed no result"},
		{"claude-out-of-turns", armClaudeCode, ""},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv(fakeCLIEnv, tc.mode)
			row, cost := boardRow(t, prepareArm(context.Background(), env, tc.arm, f, boardPrices(t), governed.ShellPolicy{}))
			if tc.why == "" {
				if row.Unevaluated != "" || !row.Solved {
					t.Fatalf("an evaluated attempt = %+v", row)
				}
				return
			}
			if row.Solved || !strings.Contains(row.Unevaluated, tc.why) {
				t.Fatalf("row = %+v, want not evaluated because of %q", row, tc.why)
			}
			// A launch that reached no model spent nothing, so it must not stop a
			// capped board as an unknown amount would.
			if tc.mode == "claude-crash" && cost.Unpriced != 0 {
				t.Errorf("cost = %+v, want nothing spent", cost)
			}
		})
	}
}

type refusingModel struct{ err error }

func (m refusingModel) Generate(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, m.err
}

func TestNativeArmDoesNotCountARefusalAsAnAttempt(t *testing.T) {
	env := boardTestEnv(nil, nil)
	env.native = func(context.Context) (llm.Model, harness.Plan, string, error) {
		err := fault.New(fault.Terminal, "anthropic_status", "anthropic: HTTP 400: Your credit balance is too low")
		return refusingModel{err}, harness.Plan{}, "anthropic:claude-sonnet-5-5", nil
	}
	row, _ := boardRow(t, prepareArm(context.Background(), env, armFlynn, boardFlags{}, boardPrices(t), governed.ShellPolicy{}))
	if row.Solved || !strings.Contains(row.Unevaluated, "credit balance") {
		t.Fatalf("row = %+v, want not evaluated for the empty account", row)
	}
}

func TestOnlyAProvidersOwnFailureIsARefusal(t *testing.T) {
	for msg, want := range map[string]bool{
		"anthropic_status: anthropic: HTTP 429: slow down":     true,
		"openai_status: openai: HTTP 503: unavailable":         true,
		"openai_status: openai: HTTP 400: bad tool schema":     false,
		"mission stalled: the test prints rate limit exceeded": false,
	} {
		if got := apiRefusal(msg) != ""; got != want {
			t.Errorf("apiRefusal(%q) = %t, want %t", msg, got, want)
		}
	}
	for out, want := range map[string]bool{
		"exit 0\nok":                              true,
		"exit 1\nassert":                          true,
		"exit 3221225794\n":                       false,
		"the command could not run: no shell":     false,
		"the sandbox could not start: no profile": false,
	} {
		if got := verifierStarted(out); got != want {
			t.Errorf("verifierStarted(%q) = %t, want %t", out, got, want)
		}
	}
}

// A resumed board keeps what was evaluated, makes the rest, and leaves alone the arms
// it was not asked to run.
func TestPlayBoardResumeRunsOnlyWhatIsMissing(t *testing.T) {
	root := writeExerciseSet(t, "alpha", "[one] do it | exit 0\n")
	writeFixtures(t, root, "alpha", "one")
	entries, err := boardEntries(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	row := func(arm string, repeat int, why string) skillab.BoardRun {
		return skillab.BoardRun{Skill: "alpha", Fixture: "one", Arm: arm, Repeat: repeat, Unevaluated: why, Priced: true}
	}
	board := skillab.Board{
		Arms: []skillab.ArmStatus{{Name: "a", Note: "first run"}, {Name: "other"}, {Name: "gone", Note: "kept"}},
		Runs: []skillab.BoardRun{row("a", 1, ""), row("a", 2, "crashed"), row("other", 1, "no credit"), row("gone", 1, "")},
	}
	var made []string
	arm := boardArm{
		status: skillab.ArmStatus{Name: "a", Note: "second run"},
		run: func(context.Context, string, string) (boardAttempt, error) {
			made = append(made, "a")
			return boardAttempt{cost: skillab.Cost{Turns: 1}}, nil
		},
	}
	gone := boardArm{status: skillab.ArmStatus{Name: "gone", Skipped: true, Reason: "no key"}}
	f := boardFlags{repeats: 3, timeout: time.Minute, resume: true, keep: t.TempDir()}
	// Evidence left by the attempt being made again.
	old := map[string]string{"stale.txt": "first run"}
	stale, err := keepAttempt(filepath.Join(f.keep, "alpha"), "one", "a", 2, old)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := playBoard(context.Background(), &out, &board, entries, []boardArm{arm, gone}, f, skillab.Limit{}); err != nil {
		t.Fatalf("play: %v\n%s", err, out.String())
	}
	if len(made) != 2 {
		t.Errorf("made %d attempts, want repeat 2 again and repeat 3", len(made))
	}
	if len(board.Runs) != 5 || board.Planned != 9 {
		t.Errorf("%d rows of %d planned: %+v", len(board.Runs), board.Planned, board.Runs)
	}
	if _, err := os.Stat(filepath.Join(stale, "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("the first run's evidence is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stale, "checks.txt")); err != nil {
		t.Errorf("the second run's evidence was not kept: %v", err)
	}
	if board.Arms[0].Note != "second run" || board.Arms[2].Skipped || board.Arms[2].Note != "kept" {
		t.Errorf("arms = %+v", board.Arms)
	}
	for _, r := range board.Runs {
		if r.Unevaluated != "" && r.Arm != "other" {
			t.Errorf("a rerun row is still unevaluated: %+v", r)
		}
	}
}

func TestResumeNeedsTheBoardItContinues(t *testing.T) {
	if _, err := parseBoardArgs([]string{"--resume"}, io.Discard); err == nil {
		t.Fatal("--resume without --out was accepted")
	}
	if _, err := readBoardFile("no-such-board.json"); err == nil {
		t.Fatal("a missing board was read")
	}
}
