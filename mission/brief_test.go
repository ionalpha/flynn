package mission

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// briefTool is a tool that also briefs. It counts the calls it has seen in its own
// state, the way a real Briefer keeps run-once and repeat counters.
type briefTool struct {
	name  string
	run   func(ctx context.Context, in json.RawMessage) (string, error)
	brief func(in BriefInput) (BriefOutput, error)

	seen []BriefInput
}

func (b *briefTool) Def() llm.Tool {
	return llm.Tool{Name: b.name, Description: "t", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (b *briefTool) Invoke(ctx context.Context, in json.RawMessage) (string, error) {
	if b.run != nil {
		return b.run(ctx, in)
	}
	return "ok", nil
}

func (b *briefTool) Brief(_ context.Context, in BriefInput) (BriefOutput, error) {
	b.seen = append(b.seen, in)
	return b.brief(in)
}

// countingBrief loads text once the tool has seen n settled calls, and records the count
// in state so a resumed run does not start again from zero.
func countingBrief(n int, text string) func(BriefInput) (BriefOutput, error) {
	return func(in BriefInput) (BriefOutput, error) {
		var st struct{ Calls int }
		if len(in.State) > 0 {
			_ = json.Unmarshal(in.State, &st)
		}
		before := st.Calls
		st.Calls += len(in.Calls)
		raw, _ := json.Marshal(st)
		out := BriefOutput{State: raw}
		if before < n && st.Calls >= n {
			out.Text = text
			out.Notes = []BriefNote{{Name: "feedback-loop", Via: "trigger", Reason: "repeat"}}
		}
		return out, nil
	}
}

func lastUser(t *testing.T, cp checkpoint) llm.Message {
	t.Helper()
	m := cp.Messages[len(cp.Messages)-1]
	if m.Role != llm.RoleUser {
		t.Fatalf("last message is %s, want user", m.Role)
	}
	return m
}

// TestBriefRidesTheToolResultTurn: the brief text is a block of the user message that
// carries the tool results, after them, so the provider sees tool_result first and the
// stored conversation has no message between a tool_use and its result.
func TestBriefRidesTheToolResultTurn(t *testing.T) {
	bt := &briefTool{name: "bt", brief: countingBrief(1, "LOADED BODY")}
	model := llmtest.NewScripted(
		llmtest.CallTool("t1", "bt", json.RawMessage(`{}`)),
		llmtest.SayText("done"),
	)
	exec := NewExecutor(model, WithTools(bt))

	_, cp := step(t, exec, nil)

	if len(cp.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (goal, tool_use, results+brief)", len(cp.Messages))
	}
	m := lastUser(t, cp)
	if len(m.Blocks) != 2 || m.Blocks[0].Kind != llm.KindToolResult || m.Blocks[1].Kind != llm.KindText || m.Blocks[1].Text != "LOADED BODY" {
		t.Fatalf("user turn blocks = %+v, want tool_result then the brief text", m.Blocks)
	}
	raw, err := encodeCheckpoint(cp)
	if err != nil {
		t.Fatal(err)
	}
	back, err := decodeCheckpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := lastUser(t, back).Blocks; len(got) != 2 || got[1].Text != "LOADED BODY" {
		t.Fatalf("the brief did not survive a checkpoint round trip: %+v", got)
	}
}

// TestBriefReachesTheNextModelCall proves the text is in the request the model sees next,
// not only in the stored checkpoint.
func TestBriefReachesTheNextModelCall(t *testing.T) {
	bt := &briefTool{name: "bt", brief: countingBrief(1, "LOADED BODY")}
	model := llmtest.NewScripted(
		llmtest.CallTool("t1", "bt", json.RawMessage(`{}`)),
		llmtest.SayText("done"),
	)
	driveToDone(t, NewExecutor(model, WithTools(bt)), 5)

	reqs := model.Requests()
	if got := reqs[1].Messages[len(reqs[1].Messages)-1].TextContent(); !strings.Contains(got, "LOADED BODY") {
		t.Fatalf("the second request did not carry the brief: %q", got)
	}
}

// TestBriefStateSurvivesResume: every step re-decodes the checkpoint, so a counter that
// reached 2 across two steps proves the Briefer state is stored and read back.
func TestBriefStateSurvivesResume(t *testing.T) {
	bt := &briefTool{name: "bt", brief: countingBrief(2, "TWICE")}
	model := llmtest.NewScripted(
		llmtest.CallTool("t1", "bt", json.RawMessage(`{}`)),
		llmtest.CallTool("t2", "bt", json.RawMessage(`{}`)),
		llmtest.SayText("done"),
	)
	exec := NewExecutor(model, WithTools(bt))

	raw, cp := step(t, exec, nil)
	if strings.Contains(lastUser(t, cp).TextContent(), "TWICE") {
		t.Fatal("fired after one call")
	}
	if len(cp.Briefs["bt"]) == 0 {
		t.Fatal("the briefer state was not stored in the checkpoint")
	}
	_, cp = step(t, exec, raw)
	if !strings.Contains(lastUser(t, cp).TextContent(), "TWICE") {
		t.Fatalf("did not fire on the second call after a resume: %+v", lastUser(t, cp).Blocks)
	}
}

// TestBriefStartLoadsInTheFirstUserTurn: the start brief is part of the first message,
// ahead of any model call, and runs once.
func TestBriefStartLoadsInTheFirstUserTurn(t *testing.T) {
	bt := &briefTool{name: "bt", brief: func(in BriefInput) (BriefOutput, error) {
		if in.Start {
			return BriefOutput{Text: "START BODY", Notes: []BriefNote{{Name: "a", Via: "start"}}}, nil
		}
		return BriefOutput{}, nil
	}}
	model := llmtest.NewScripted(llmtest.CallTool("t1", "bt", json.RawMessage(`{}`)), llmtest.SayText("done"))
	rec := &recordingReporter{}
	exec := NewExecutor(model, WithTools(bt), WithObserver(rec))

	driveToDone(t, exec, 5)

	first := model.Requests()[0].Messages[0]
	if !strings.Contains(first.TextContent(), "do the thing") || !strings.Contains(first.TextContent(), "START BODY") {
		t.Fatalf("first message = %q, want the objective and the start brief", first.TextContent())
	}
	starts := 0
	for _, in := range bt.seen {
		if in.Start {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("start brief requested %d times, want 1", starts)
	}
	var notes int
	for _, ev := range rec.events() {
		if ev.Kind == EventSkill && ev.Skill == "a" && ev.Via == "start" {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("skill start events = %d, want 1", notes)
	}
}

// TestBriefSlowUsesTheToolClock: Elapsed is measured with the configured clock around the
// tool call, so a test advances it from inside the tool.
func TestBriefSlowUsesTheToolClock(t *testing.T) {
	clk := clock.NewManual(time.Unix(1000, 0))
	bt := &briefTool{
		name: "bt",
		run: func(context.Context, json.RawMessage) (string, error) {
			clk.Advance(90 * time.Second)
			return "ok", nil
		},
		brief: func(BriefInput) (BriefOutput, error) { return BriefOutput{}, nil },
	}
	model := llmtest.NewScripted(llmtest.CallTool("t1", "bt", json.RawMessage(`{"command":"go test"}`)), llmtest.SayText("done"))
	driveToDone(t, NewExecutor(model, WithTools(bt), WithToolClock(clk)), 5)

	var got []ObservedCall
	for _, in := range bt.seen {
		got = append(got, in.Calls...)
	}
	if len(got) != 1 || got[0].Tool != "bt" || got[0].Elapsed != 90*time.Second || string(got[0].Input) != `{"command":"go test"}` {
		t.Fatalf("observed = %+v, want one bt call that took 90s", got)
	}
}

// TestBriefErrorDoesNotFailTheStep: a hint must not fail an otherwise sound step, and a
// failed call leaves the Briefer's stored state alone.
func TestBriefErrorDoesNotFailTheStep(t *testing.T) {
	bt := &briefTool{name: "bt", brief: func(BriefInput) (BriefOutput, error) {
		return BriefOutput{Text: "ignored", State: json.RawMessage(`{"x":1}`)}, errors.New("boom")
	}}
	model := llmtest.NewScripted(llmtest.CallTool("t1", "bt", json.RawMessage(`{}`)), llmtest.SayText("done"))

	_, cp := step(t, NewExecutor(model, WithTools(bt)), nil)

	if got := lastUser(t, cp); len(got.Blocks) != 1 {
		t.Fatalf("blocks = %+v, want the tool result only", got.Blocks)
	}
	if len(cp.Briefs) != 0 {
		t.Fatalf("state advanced on a failed brief: %v", cp.Briefs)
	}
}

// TestBriefFanoutKeepsToolResultsFirst: a spawning turn parks, and the brief built when
// the children fold in joins the same user message after every tool_result, so the
// tool_use/tool_result pairing the provider checks is unbroken.
func TestBriefFanoutKeepsToolResultsFirst(t *testing.T) {
	bt := &briefTool{name: "bt", brief: countingBrief(1, "AFTER FOLD")}
	fan := &fakeFanout{}
	two := llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "a", Name: "bt", Input: json.RawMessage(`{}`)}},
			{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "s", Name: ActionSpawn, Input: json.RawMessage(`{"objective":"x"}`)}},
		}},
		StopReason: llm.StopToolUse,
	}
	model := llmtest.NewScripted(two, llmtest.SayText("done"))
	exec := NewExecutor(model, WithTools(bt), WithFanout(fan))

	raw, cp := step(t, exec, nil)
	if len(cp.Pending) != 2 || cp.Messages[len(cp.Messages)-1].Role != llm.RoleAssistant {
		t.Fatalf("a spawning turn must park with no user message yet: %+v", cp)
	}
	if len(bt.seen) != 1 {
		t.Fatalf("brief ran before the fold: %d calls (only the start brief expected)", len(bt.seen))
	}

	fan.done = true
	_, cp = step(t, exec, raw)
	m := lastUser(t, cp)
	if len(m.Blocks) != 3 || m.Blocks[0].Kind != llm.KindToolResult || m.Blocks[1].Kind != llm.KindToolResult || m.Blocks[2].Kind != llm.KindText || m.Blocks[2].Text != "AFTER FOLD" {
		t.Fatalf("folded turn = %+v, want two tool_results then the brief", m.Blocks)
	}
	// Only the non-spawn call is observed.
	last := bt.seen[len(bt.seen)-1]
	if len(last.Calls) != 1 || last.Calls[0].Tool != "bt" {
		t.Fatalf("observed after fold = %+v, want just the bt call", last.Calls)
	}
}

// TestBriefFanoutResumeKeepsElapsed: the timing measured when the tools ran survives the
// park, so a slow command is still slow when the brief is built after the fold.
func TestBriefFanoutResumeKeepsElapsed(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0))
	bt := &briefTool{
		name: "bt",
		run: func(context.Context, json.RawMessage) (string, error) {
			clk.Advance(2 * time.Minute)
			return "ok", nil
		},
		brief: func(BriefInput) (BriefOutput, error) { return BriefOutput{}, nil },
	}
	fan := &fakeFanout{}
	two := llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "a", Name: "bt", Input: json.RawMessage(`{}`)}},
			{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "s", Name: ActionSpawn, Input: json.RawMessage(`{"objective":"x"}`)}},
		}},
		StopReason: llm.StopToolUse,
	}
	exec := NewExecutor(llmtest.NewScripted(two, llmtest.SayText("done")), WithTools(bt), WithFanout(fan), WithToolClock(clk))

	raw, _ := step(t, exec, nil)
	fan.done = true
	step(t, exec, raw)

	last := bt.seen[len(bt.seen)-1]
	if len(last.Calls) != 1 || last.Calls[0].Elapsed != 2*time.Minute {
		t.Fatalf("observed after fold = %+v, want the 2m the call took", last.Calls)
	}
}

// TestBriefersRunInNameOrder keeps two Briefers' contributions in a fixed order.
func TestBriefersRunInNameOrder(t *testing.T) {
	mk := func(name string) *briefTool {
		return &briefTool{name: name, brief: func(in BriefInput) (BriefOutput, error) {
			if in.Start {
				return BriefOutput{Text: name}, nil
			}
			return BriefOutput{}, nil
		}}
	}
	exec := NewExecutor(llmtest.NewScripted(llmtest.SayText("x")), WithTools(mk("zeta"), mk("alpha")))
	cp := checkpoint{}
	blocks := exec.brief(context.Background(), "g", 1, &cp, true, nil)
	if len(blocks) != 2 || blocks[0].Text != "alpha" || blocks[1].Text != "zeta" {
		t.Fatalf("blocks = %+v, want alpha then zeta", blocks)
	}
}
