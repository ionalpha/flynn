package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// newREPL builds an interactive session over an in-memory store and a scripted
// model, capturing its output in the returned buffer. Output is written through a
// syncWriter so the per-turn signal goroutine and the render loop never race on it.
func newREPL(t *testing.T, dir string, store *sqlite.Store, model llm.Model) (*replSession, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	return &replSession{
		out:     &syncWriter{w: buf},
		model:   model,
		verbose: true,
		cwd:     dir,
		store:   store,
		reg:     mustRegistry(t),
	}, buf
}

// assertMultiTurnStream checks the invariants of one multi-turn session's durable
// stream: it opens with exactly one session.started (only the first turn opens the
// run), carries a dense Seq from 1, pairs every tool result with an earlier call,
// never lets the turn index run backwards, and records exactly one converged event
// per turn, the last of which ends the stream.
func assertMultiTurnStream(t *testing.T, evs []session.Event, wantTurns int) {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("empty stream")
	}
	if evs[0].Kind != session.KindSessionStarted {
		t.Fatalf("first event = %q, want %q", evs[0].Kind, session.KindSessionStarted)
	}
	starts, converged, lastTurn := 0, 0, 0
	open := map[string]bool{}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d (stream not dense)", i, e.Seq, i+1)
		}
		switch e.Kind {
		case session.KindSessionStarted:
			starts++
		case session.KindConverged:
			converged++
		case session.KindToolCall:
			open[e.ToolUseID] = true
		case session.KindToolResult:
			if !open[e.ToolUseID] {
				t.Fatalf("tool result %q has no preceding call", e.ToolUseID)
			}
		}
		if e.Turn > 0 {
			if e.Turn < lastTurn {
				t.Fatalf("turn went backwards: %d after %d", e.Turn, lastTurn)
			}
			lastTurn = e.Turn
		}
	}
	if starts != 1 {
		t.Fatalf("session.started count = %d, want exactly 1 for the whole session", starts)
	}
	if converged != wantTurns {
		t.Fatalf("converged count = %d, want %d (one per turn)", converged, wantTurns)
	}
	if last := evs[len(evs)-1].Kind; last != session.KindConverged {
		t.Fatalf("last event = %q, want %q", last, session.KindConverged)
	}
}

// TestInteractiveMultiTurnContinuesOneRun is the headline proof: two lines typed at
// the prompt are two turns of one durable run. The run keeps a single id across
// turns, the model is handed the first turn's question and answer when answering the
// second (continuity, not a cold restart), and the recorded stream is one coherent
// multi-turn conversation.
func TestInteractiveMultiTurnContinuesOneRun(t *testing.T) {
	dir := t.TempDir()
	store := memStore(t)
	model := llmtest.NewScripted(
		llmtest.SayText("first answer"),
		llmtest.SayText("second answer"),
	)
	s, buf := newREPL(t, dir, store, model)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	r1, err := s.runTurn(ctx, "first question", nil)
	if err != nil || r1 != "first answer" {
		t.Fatalf("turn 1 = (%q, %v), want (\"first answer\", nil)\n%s", r1, err, buf.String())
	}
	id := s.runID
	if id == "" {
		t.Fatal("first turn did not assign a run id")
	}

	r2, err := s.runTurn(ctx, "second question", nil)
	if err != nil || r2 != "second answer" {
		t.Fatalf("turn 2 = (%q, %v), want (\"second answer\", nil)\n%s", r2, err, buf.String())
	}
	if s.runID != id {
		t.Fatalf("run id changed across turns: %s -> %s (a follow-up must continue the same run)", id, s.runID)
	}

	// Continuity: the second turn's request carries the whole prior conversation.
	reqs := model.Requests()
	var blob strings.Builder
	for _, m := range reqs[len(reqs)-1].Messages {
		blob.WriteString(m.TextContent())
		blob.WriteByte('\n')
	}
	for _, want := range []string{"first question", "first answer", "second question"} {
		if !strings.Contains(blob.String(), want) {
			t.Fatalf("turn 2 lost continuity: history missing %q\n%s", want, blob.String())
		}
	}

	// The durable stream is one coherent two-turn conversation, addressable by id.
	evs, err := session.History(ctx, store.Log(), id)
	if err != nil {
		t.Fatal(err)
	}
	assertMultiTurnStream(t, evs, 2)
}

// TestInteractiveStreamWellFormedAcrossTurns drives a range of turn counts and
// confirms the recorded stream stays well formed however many turns the session
// runs: one open, one converged per turn, dense and ordered throughout.
func TestInteractiveStreamWellFormedAcrossTurns(t *testing.T) {
	for _, turns := range []int{1, 2, 3, 5} {
		t.Run(fmt.Sprintf("turns_%d", turns), func(t *testing.T) {
			dir := t.TempDir()
			store := memStore(t)
			responses := make([]llm.Response, turns)
			for i := range responses {
				responses[i] = llmtest.SayText(fmt.Sprintf("answer %d", i))
			}
			s, buf := newREPL(t, dir, store, llmtest.NewScripted(responses...))
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			for i := range turns {
				if _, err := s.runTurn(ctx, fmt.Sprintf("question %d", i), nil); err != nil {
					t.Fatalf("turn %d: %v\n%s", i, err, buf.String())
				}
			}
			evs, err := session.History(ctx, store.Log(), s.runID)
			if err != nil {
				t.Fatal(err)
			}
			assertMultiTurnStream(t, evs, turns)
		})
	}
}

// scriptedLines is a lineReader that returns a fixed sequence of messages and then
// io.EOF, standing in for the terminal so the loop is testable without a tty.
type scriptedLines struct {
	lines []string
	i     int
}

func (r *scriptedLines) ReadLine() (string, error) {
	if r.i >= len(r.lines) {
		return "", io.EOF
	}
	line := r.lines[r.i]
	r.i++
	return line, nil
}

// TestInteractiveLoopExitCommand: an exit command leaves the loop cleanly, and a
// session that ran no turn just says goodbye.
func TestInteractiveLoopExitCommand(t *testing.T) {
	s, buf := newREPL(t, t.TempDir(), memStore(t), llmtest.NewScripted())
	if err := s.loop(context.Background(), &scriptedLines{lines: []string{"exit"}}, nil); err != nil {
		t.Fatalf("loop: %v", err)
	}
	if !strings.Contains(buf.String(), "goodbye") {
		t.Fatalf("expected goodbye on an empty session, got:\n%s", buf.String())
	}
}

// TestInteractiveLoopEOF: input ending (Ctrl-D) leaves the loop, and blank lines
// before it are skipped without driving a turn (the scripted model has no turns, so
// a stray drive would error).
func TestInteractiveLoopEOF(t *testing.T) {
	s, buf := newREPL(t, t.TempDir(), memStore(t), llmtest.NewScripted())
	if err := s.loop(context.Background(), &scriptedLines{lines: []string{"", "   "}}, nil); err != nil {
		t.Fatalf("loop: %v", err)
	}
	if !strings.Contains(buf.String(), "goodbye") {
		t.Fatalf("expected goodbye at EOF, got:\n%s", buf.String())
	}
}

// TestInteractiveTurnErrorDoesNotEndSession: a turn that fails is reported but
// returns the user to the prompt; the session continues and can still be left.
func TestInteractiveTurnErrorDoesNotEndSession(t *testing.T) {
	// An empty script makes the first model call fail terminally, so the turn stalls.
	s, buf := newREPL(t, t.TempDir(), memStore(t), llmtest.NewScripted())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.loop(ctx, &scriptedLines{lines: []string{"do the thing", "exit"}}, nil); err != nil {
		t.Fatalf("loop: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "error:") {
		t.Fatalf("a failed turn was not reported:\n%s", out)
	}
	if !strings.Contains(out, "ended") {
		t.Fatalf("session did not end after the error:\n%s", out)
	}
}

// gateModel blocks in Generate until released, signalling when it is first entered.
// It lets a test interrupt a turn deterministically while the model is mid-call,
// then release the gate so a later turn succeeds.
type gateModel struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *gateModel) Generate(ctx context.Context, _ llm.Request) (llm.Response, error) {
	m.once.Do(func() { close(m.entered) })
	select {
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	case <-m.release:
		return llmtest.SayText("ok"), nil
	}
}

// TestInteractiveCancelTurnKeepsSession proves Ctrl-C cancels only the in-flight
// turn: the cancelled turn returns a cancellation, the session survives, and a later
// turn on the same run completes normally once the model is responsive again.
func TestInteractiveCancelTurnKeepsSession(t *testing.T) {
	m := &gateModel{entered: make(chan struct{}), release: make(chan struct{})}
	s, buf := newREPL(t, t.TempDir(), memStore(t), m)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sig := make(chan os.Signal, 1)
	go func() {
		<-m.entered
		sig <- os.Interrupt
	}()

	_, err := s.runTurn(ctx, "long running", sig)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled turn err = %v, want context.Canceled\n%s", err, buf.String())
	}
	id := s.runID
	if id == "" {
		t.Fatal("a cancelled first turn still opens the run, but no id was recorded")
	}

	// The model is responsive now; the next line continues the same session.
	close(m.release)
	r2, err := s.runTurn(ctx, "carry on", sig)
	if err != nil || r2 != "ok" {
		t.Fatalf("turn after cancel = (%q, %v), want (\"ok\", nil)\n%s", r2, err, buf.String())
	}
	if s.runID != id {
		t.Fatalf("run id changed after a cancelled turn: %s -> %s", id, s.runID)
	}
}

// TestInteractiveRecallsOnceAndReuses proves recall happens once, on the opening
// line, and the same standing instructions carry across turns: a stored memory
// sharing a keyword with the first line is folded into the system prompt, and the
// second turn runs under the identical prompt rather than recalling again.
func TestInteractiveRecallsOnceAndReuses(t *testing.T) {
	dir := t.TempDir()
	store := memStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := store.Memory().Write(ctx, state.MemoryItem{Kind: "lesson", Content: "the deploy target is fly.io"}); err != nil {
		t.Fatal(err)
	}

	model := llmtest.NewScripted(llmtest.SayText("a"), llmtest.SayText("b"))
	s, buf := newREPL(t, dir, store, model)

	if _, err := s.runTurn(ctx, "deploy the service", nil); err != nil {
		t.Fatalf("turn 1: %v\n%s", err, buf.String())
	}
	if _, err := s.runTurn(ctx, "now check it", nil); err != nil {
		t.Fatalf("turn 2: %v\n%s", err, buf.String())
	}

	reqs := model.Requests()
	if !strings.Contains(reqs[0].System, "fly.io") {
		t.Fatalf("opening turn did not recall the stored memory into its prompt:\n%s", reqs[0].System)
	}
	if reqs[len(reqs)-1].System != reqs[0].System {
		t.Fatalf("system prompt changed across turns (recall should be once per session):\nfirst:\n%s\nlast:\n%s", reqs[0].System, reqs[len(reqs)-1].System)
	}
}

// TestInteractiveLearnsAtSessionEnd proves the learning loop closes over a whole
// session: ending the session distills the conversation into durable knowledge that
// a later session can recall.
func TestInteractiveLearnsAtSessionEnd(t *testing.T) {
	dir := t.TempDir()
	store := memStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	s, buf := newREPL(t, dir, store, llmtest.NewScripted(llmtest.SayText("set things up")))
	s.distiller = &fakeDistiller{lessons: []learn.Lesson{
		{Kind: learn.LessonMemory, Body: "the project uses pnpm for installs"},
	}}

	if _, err := s.runTurn(ctx, "set up the project", nil); err != nil {
		t.Fatalf("turn: %v\n%s", err, buf.String())
	}
	if err := s.finish(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}

	got, err := store.Memory().Recall(ctx, state.RecallQuery{Query: "pnpm", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range got {
		if strings.Contains(m.Content, "pnpm for installs") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the session's lesson was not distilled into durable memory: %+v", got)
	}
}

// TestIsExit covers the commands that leave the session.
func TestIsExit(t *testing.T) {
	for _, in := range []string{"exit", "quit", " EXIT ", ":q", "/exit", "/quit", "Quit"} {
		if !isExit(in) {
			t.Errorf("isExit(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"", "exits", "go exit", "help", "q"} {
		if isExit(in) {
			t.Errorf("isExit(%q) = true, want false", in)
		}
	}
}
