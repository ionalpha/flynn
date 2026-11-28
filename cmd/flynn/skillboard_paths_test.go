package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/skill/skillab"
)

// The command writes a board even when no arm can run, saying why for each, and a
// resume continues the file it wrote.
func TestSkillBoardCommandRecordsSkippedArmsAndResumes(t *testing.T) {
	was := modelAskIO
	modelAskIO = func() modelAsk { return modelAsk{in: strings.NewReader(""), out: io.Discard} }
	t.Cleanup(func() { modelAskIO = was })

	root := writeExerciseSet(t, "alpha", "[one] do it | exit 0\n")
	writeFixtures(t, root, "alpha", "one")
	dir := t.TempDir()
	out, md := filepath.Join(dir, "b.json"), filepath.Join(dir, "b.md")
	args := []string{"skill", "board", "--exercises", root, "--arms", "flynn,claude-code,codex", "--out", out, "--markdown", md}
	data := t.TempDir()
	for _, extra := range [][]string{nil, {"--resume"}} {
		got := runCLIIn(t, data, append(append([]string(nil), args...), extra...)...)
		if got.code != 0 {
			t.Fatalf("%v: exit %d: %s", extra, got.code, got.stderr)
		}
	}
	file, err := readBoardFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Arms) != 3 || len(file.Runs) != 0 {
		t.Fatalf("board = %+v, want three skipped arms and no attempts", file)
	}
	for _, a := range file.Arms {
		if !a.Skipped || a.Reason == "" {
			t.Errorf("arm %s = %+v, want skipped with a reason", a.Name, a)
		}
	}
	if _, err := os.Stat(md); err != nil {
		t.Error(err)
	}
}

// Everything the command can refuse, it refuses before anything is spent.
func TestSkillBoardRefusesBadInput(t *testing.T) {
	// One case gets far enough to look for keys, so the developer's own are put out of reach.
	noProviderKeys(t)
	root := writeExerciseSet(t, "alpha", "[one] do it | exit 0\n")
	writeFixtures(t, root, "alpha", "one")
	empty := t.TempDir()
	missing := filepath.Join(empty, "missing")
	for name, args := range map[string][]string{
		"unknown flag":    {"--nope"},
		"no arm":          {"--arms", ""},
		"bad cap":         {"--max-cost", "lots"},
		"no root":         {"--exercises", missing},
		"no such skill":   {"--exercises", root, "--skills", "absent"},
		"no exercises":    {"--exercises", empty},
		"nothing resumed": {"--exercises", root, "--resume", "--out", missing, "--arms", "codex"},
	} {
		if err := runSkillBoard(args, "", t.TempDir(), governed.ShellPolicy{}, io.Discard); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A driven arm reads no usage back, so its rows are unpriced, and a driver that cannot
// start leaves the attempt unevaluated.
func TestDrivenArmIsUnpricedAndAFailedStartIsNotAnAttempt(t *testing.T) {
	env := boardTestEnv(nil, nil)
	starts := 0
	env.driven = func(context.Context, string, string, string) (*externAgent, error) {
		starts++
		if starts > 2 {
			return nil, errors.New("the CLI went away")
		}
		return &externAgent{driver: &plainDriver{stubHarnessDriver{name: "claude"}}}, nil
	}
	f := boardFlags{claudeModel: "c", codexModel: "x", timeout: time.Minute}
	arm := prepareArm(context.Background(), env, armFlynnClaude, f, nil, governed.ShellPolicy{})
	if arm.status.Skipped || arm.status.Model != "c" {
		t.Fatalf("status = %+v", arm.status)
	}
	row, _ := boardRow(t, arm)
	if row.Priced || !strings.Contains(row.Error, "builds no loop") {
		t.Errorf("row = %+v, want an unpriced attempt that records why it stopped", row)
	}
	row, _ = boardRow(t, arm)
	if row.Solved || !strings.Contains(row.Unevaluated, "could not start") {
		t.Errorf("row = %+v, want not evaluated", row)
	}
	if a := prepareArm(context.Background(), env, "gemini", f, nil, governed.ShellPolicy{}); !a.status.Skipped {
		t.Error("an unknown arm was prepared")
	}
	if key := env.apiKey(context.Background(), "no-such-provider"); key != "" {
		t.Errorf("an unknown provider has key %q", key)
	}
}

func TestBoardTrialEndings(t *testing.T) {
	root := writeExerciseSet(t, "alpha", "[one] do it | exit 0\n")
	writeFixtures(t, root, "alpha", "one")
	entries, err := boardEntries(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	arm := func(err error) boardArm {
		return boardArm{status: skillab.ArmStatus{Name: "a"}, run: func(context.Context, string, string) (boardAttempt, error) {
			return boardAttempt{evidence: map[string]string{"extra.txt": "x"}}, err
		}}
	}
	ctx, f := context.Background(), boardFlags{timeout: time.Minute}

	if _, _, err := boardTrial(ctx, io.Discard, arm(errors.New("no store")), entries[0], 1, f); err == nil {
		t.Error("an arm's harness error did not end the board")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := boardTrial(cancelled, io.Discard, arm(nil), entries[0], 1, f); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	// The arm waits out its deadline: a coarse clock can return before a short timer fires.
	slow := arm(nil)
	slow.run = func(actx context.Context, _, _ string) (boardAttempt, error) {
		<-actx.Done()
		return boardAttempt{}, nil
	}
	row, _, err := boardTrial(ctx, io.Discard, slow, entries[0], 1, boardFlags{timeout: time.Millisecond})
	if err != nil || row.Ended != skillab.Aborted || !strings.Contains(row.Error, "timed out") {
		t.Errorf("timeout: %+v, %v", row, err)
	}

	f.keep = t.TempDir()
	var out bytes.Buffer
	for range 2 {
		if _, _, err := boardTrial(ctx, &out, arm(nil), entries[0], 1, f); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(out.String(), "kept: ") || !strings.Contains(out.String(), "could not be kept") {
		t.Errorf("evidence is kept once and never overwritten without --resume:\n%s", out.String())
	}

	gone := entries[0]
	gone.Exercise.Fixture = "absent"
	if _, _, err := boardTrial(ctx, io.Discard, arm(nil), gone, 1, f); err == nil {
		t.Error("a fixture that is not there was run")
	}
}

// A board that cannot be written says so, during the run and at its end.
func TestPlayBoardReportsAnUnwritableBoard(t *testing.T) {
	root := writeExerciseSet(t, "alpha", "[one] do it | exit 0\n")
	writeFixtures(t, root, "alpha", "one")
	entries, err := boardEntries(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	taken := t.TempDir()
	if err := os.WriteFile(filepath.Join(taken, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	arm := boardArm{status: skillab.ArmStatus{Name: "a"}, run: func(context.Context, string, string) (boardAttempt, error) {
		return boardAttempt{}, nil
	}}
	for name, f := range map[string]boardFlags{
		"json":     {out: taken},
		"markdown": {markdown: taken},
	} {
		f.timeout = time.Minute
		var out bytes.Buffer
		board := skillab.Board{Generated: clock.System{}.Now()}
		if err := playBoard(context.Background(), &out, &board, entries, []boardArm{arm}, f, skillab.Limit{}); err == nil {
			t.Errorf("%s: an unwritable board was reported as written", name)
		}
		if name == "json" && !strings.Contains(out.String(), "could not be checkpointed") {
			t.Errorf("the failed checkpoint is not reported:\n%s", out.String())
		}
	}
}

func TestStreamParsersReadTheirOtherShapes(t *testing.T) {
	claude := parseClaudeCodeStream([]byte(`{"type":"assistant"}`+"\n"+
		`{"type":"result","subtype":"success","num_turns":1,"usage":{"input_tokens":5,"output_tokens":2}}`+"\n"), "m")
	if got := claude.perModel["m"]; got.Input.N != 5 || got.Output.N != 2 {
		t.Errorf("a result without modelUsage = %+v, want its usage under the asked model", claude.perModel)
	}
	codex := parseCodexStream([]byte("not json\n" + `{"type":"error","message":" lost the stream "}` + "\n"))
	if codex.failure != "lost the stream" {
		t.Errorf("failure = %q", codex.failure)
	}
}

type scriptedModel struct{ resp llm.Response }

func (m *scriptedModel) Generate(context.Context, llm.Request) (llm.Response, error) {
	return m.resp, nil
}

// The first useful action is the first returned call that asks to change a file.
func TestMeteredModelMarksTheFirstEdit(t *testing.T) {
	inner := &scriptedModel{}
	m := newMeteredModel(inner, nil, clock.System{})
	for _, tool := range []string{"read", "edit", "write"} {
		inner.resp = llm.Response{Message: llm.Message{Blocks: []llm.Block{{ToolUse: &llm.ToolUse{Name: tool}}}}}
		if _, err := m.Generate(context.Background(), llm.Request{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.firstUseful(); got != 2 {
		t.Errorf("first useful turn = %d, want 2", got)
	}
}
