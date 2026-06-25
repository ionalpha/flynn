package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// newTUIForTest builds a TUI model over an in-memory session and the given model,
// already sized so the viewport is live.
func newTUIForTest(t *testing.T, model llm.Model) tuiModel {
	t.Helper()
	s, _ := newREPL(t, t.TempDir(), memStore(t), model)
	m := newTUIModel(context.Background(), s, "")
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return sized.(tuiModel)
}

// pumpTurn drives the manual update loop the way the bubbletea runtime would: it
// runs each command, feeds the resulting message back into Update, and stops once the
// turn's output stream closes. It is bounded by a timeout so a stuck turn fails the
// test loudly instead of hanging.
func pumpTurn(t *testing.T, m tuiModel, cmd tea.Cmd) tuiModel {
	t.Helper()
	type result struct{ m tuiModel }
	ch := make(chan result, 1)
	go func() {
		cur := m
		for cmd != nil {
			msg := cmd()
			next, c := cur.Update(msg)
			cur = next.(tuiModel)
			cmd = c
			if _, done := msg.(streamClosedMsg); done {
				break
			}
		}
		ch <- result{cur}
	}()
	select {
	case r := <-ch:
		return r.m
	case <-time.After(15 * time.Second):
		t.Fatal("TUI turn did not complete (the update loop stalled)")
		return m
	}
}

// TestTUIRunsTurnAndShowsTranscript is the end-to-end proof for the full-screen
// interface: submitting a message drives a real turn through the same driver the
// line interface uses, the user's message and the model's answer both land in the
// transcript, and the composer is freed when the turn ends.
func TestTUIRunsTurnAndShowsTranscript(t *testing.T) {
	m := newTUIForTest(t, llmtest.NewScripted(llmtest.SayText("first answer")))
	started, cmd := m.startTurn("hello there")
	final := pumpTurn(t, started.(tuiModel), cmd)

	got := final.transcript
	for _, want := range []string{"> hello there", "first answer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("transcript missing %q:\n%s", want, got)
		}
	}
	if final.busy {
		t.Fatal("composer still marked busy after the turn ended")
	}
}

// TestTUICancelTurnKeepsSession proves Ctrl-C cancels the in-flight turn in the
// full-screen interface without ending the session: a blocked turn is cancelled, the
// transcript notes it, and the model returns to idle.
func TestTUICancelTurnKeepsSession(t *testing.T) {
	gm := &gateModel{entered: make(chan struct{}), release: make(chan struct{})}
	s, _ := newREPL(t, t.TempDir(), memStore(t), gm)
	m := newTUIModel(context.Background(), s, "")
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	started, cmd := sized.(tuiModel).startTurn("long running")
	cur := started.(tuiModel)

	// Wait until the model is mid-call, then interrupt.
	select {
	case <-gm.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never reached the model")
	}
	interrupted, _ := cur.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	final := pumpTurn(t, interrupted.(tuiModel), cmd)

	if final.busy {
		t.Fatal("session still busy after cancel")
	}
	if !strings.Contains(final.transcript, "cancelled") {
		t.Fatalf("transcript did not note the cancellation:\n%s", final.transcript)
	}
}

// TestTUIIdleKeys covers the session-level keys when no turn is running: Ctrl-C and
// Ctrl-D quit, an exit command quits, and an empty enter is a no-op.
func TestTUIIdleKeys(t *testing.T) {
	isQuit := func(cmd tea.Cmd) bool {
		if cmd == nil {
			return false
		}
		_, ok := cmd().(tea.QuitMsg)
		return ok
	}

	for _, k := range []tea.KeyType{tea.KeyCtrlC, tea.KeyCtrlD} {
		m := newTUIForTest(t, llmtest.NewScripted())
		_, cmd := m.Update(tea.KeyMsg{Type: k})
		if !isQuit(cmd) {
			t.Fatalf("key %v at idle did not quit", k)
		}
	}

	// An exit command typed into the composer quits.
	m := newTUIForTest(t, llmtest.NewScripted())
	m.ta.SetValue("exit")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !isQuit(cmd) {
		t.Fatal(`typing "exit" did not quit`)
	}

	// An empty enter does nothing and does not start a turn.
	m = newTUIForTest(t, llmtest.NewScripted())
	got, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got.(tuiModel).busy {
		t.Fatal("empty enter started a turn")
	}
}

// TestLineSinkChunkInvarianceProperty: the lines a lineSink emits do not depend on
// how the writes are chunked, so the transcript is identical however the turn's
// output happens to be flushed to it.
func TestLineSinkChunkInvarianceProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		text := rapid.StringMatching(`([a-z ]|\n){0,40}`).Draw(rt, "text")

		emit := func(chunks []string) []string {
			ch := make(chan tea.Msg, len(text)+8)
			s := &lineSink{ctx: context.Background(), ch: ch}
			for _, c := range chunks {
				_, _ = s.Write([]byte(c))
			}
			s.flush()
			var lines []string
			for {
				select {
				case msg := <-ch:
					lines = append(lines, string(msg.(outLineMsg)))
				default:
					return lines
				}
			}
		}

		whole := emit([]string{text})

		// Re-chunk at a random split point and compare.
		split := rapid.IntRange(0, len(text)).Draw(rt, "split")
		chunked := emit([]string{text[:split], text[split:]})

		if fmt.Sprint(whole) != fmt.Sprint(chunked) {
			t.Fatalf("chunking changed the emitted lines:\nwhole=%q\nchunked=%q", whole, chunked)
		}
	})
}
