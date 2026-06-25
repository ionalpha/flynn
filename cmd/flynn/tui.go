package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// runInteractiveTUI runs the full-screen interactive session: a scrollable
// transcript above a multi-line composer. It reuses the whole turn driver (recall,
// reopen, streaming, cancellation, learning) by pointing the session's output at the
// model, so the agent behaviour is identical to the line-based session; only the
// presentation differs. When the program exits, output is restored to stdout and the
// session's learning pass runs there.
func runInteractiveTUI(ctx context.Context, s *replSession, seed string) error {
	p := tea.NewProgram(newTUIModel(ctx, s, seed), tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx))
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		err = nil
	}
	s.out = &syncWriter{w: os.Stdout}
	if ferr := s.finish(ctx); ferr != nil && err == nil {
		err = ferr
	}
	return err
}

// Messages bridging a running turn into the model: each line the turn writes, the
// turn's terminal result, and the end of the turn's output stream.
type (
	outLineMsg      string
	turnDoneMsg     struct{ err error }
	streamClosedMsg struct{}
)

// tuiModel is the interactive session UI: a viewport showing the transcript, a
// textarea composer, and a status line, over one replSession whose turns it drives.
type tuiModel struct {
	s   *replSession
	ctx context.Context

	ta textarea.Model
	vp viewport.Model

	// transcript is a plain string, not a strings.Builder: the model is copied by
	// value on every Update, which a Builder forbids.
	transcript string
	ready      bool
	busy       bool

	bridge     chan tea.Msg
	turnCancel context.CancelFunc

	width, height int
	contentWidth  int // transcript wrap width (inner width minus the frame)
}

const (
	composerHeight = 3
	statusHeight   = 1
	padX           = 2 // left/right breathing room so content is not flush to the edge
	padTop         = 1 // a blank row above the transcript
	padBottom      = 1 // a blank row below the composer
)

// appStyle frames the whole UI with a little padding so nothing sits against the
// terminal edge. The inner width and height are reduced to match in layout.
var appStyle = lipgloss.NewStyle().PaddingTop(padTop).PaddingBottom(padBottom).PaddingLeft(padX)

func newTUIModel(ctx context.Context, s *replSession, seed string) tuiModel {
	ta := textarea.New()
	ta.Placeholder = "Send a message..."
	ta.Prompt = "> "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(composerHeight)
	ta.Focus()
	// seed is a resumed run's rendered history, shown above the composer so the user
	// picks up the conversation with its context already in view.
	return tuiModel{s: s, ctx: ctx, ta: ta, transcript: seed}
}

func (m tuiModel) Init() tea.Cmd { return textarea.Blink }

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.layout(msg.Width, msg.Height)
		return m, nil

	case tea.KeyMsg:
		return m.onKey(msg)

	case tea.MouseMsg:
		// The wheel always scrolls the transcript, whether a turn is running or the
		// session is idle, so reading back is never blocked by the composer focus.
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd

	case outLineMsg:
		m.appendLine(string(msg))
		return m, m.readNext()

	case turnDoneMsg:
		switch {
		case errors.Is(msg.err, context.Canceled):
			m.appendLine("  (turn cancelled)")
		case msg.err != nil:
			m.appendLine("  error: " + msg.err.Error())
		}
		return m, m.readNext() // keep draining until the stream closes

	case streamClosedMsg:
		m.busy = false
		m.turnCancel = nil
		m.bridge = nil
		return m, m.ta.Focus()
	}

	// Anything else (mouse, paste) goes to whichever component is active.
	var cmd tea.Cmd
	if m.busy {
		m.vp, cmd = m.vp.Update(msg)
	} else {
		m.ta, cmd = m.ta.Update(msg)
	}
	return m, cmd
}

// onKey handles the session-level keys and otherwise forwards to the composer (when
// idle) or the viewport (while a turn streams, so the transcript can be scrolled).
func (m tuiModel) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		if m.busy {
			if m.turnCancel != nil {
				m.turnCancel() // cancel the in-flight turn, keep the session
			}
			return m, nil
		}
		return m, tea.Quit
	case tea.KeyCtrlD:
		if !m.busy {
			return m, tea.Quit
		}
		return m, nil
	case tea.KeyEnter:
		if m.busy {
			return m, nil
		}
		text := strings.TrimSpace(m.ta.Value())
		if text == "" {
			return m, nil
		}
		if isExit(text) {
			return m, tea.Quit
		}
		m.ta.Reset()
		return m.startTurn(text)
	case tea.KeyPgUp, tea.KeyPgDown:
		// Page keys always scroll the transcript, idle or busy. They do not conflict
		// with composing (unlike the arrows, which the textarea needs for the cursor).
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	default:
		// Any other key is editing or scrolling, handled below.
	}

	var cmd tea.Cmd
	if m.busy {
		m.vp, cmd = m.vp.Update(msg) // scroll the transcript while the turn runs
	} else {
		m.ta, cmd = m.ta.Update(msg)
	}
	return m, cmd
}

// startTurn echoes the user's message, points the session output at a per-turn
// bridge channel, and drives the turn in the background. The turn writes its
// rendered lines through the bridge (as outLineMsg), then a turnDoneMsg and the
// channel close mark the end.
func (m tuiModel) startTurn(text string) (tea.Model, tea.Cmd) {
	m.busy = true
	m.appendLine("> " + text)
	m.ta.Blur()

	bridge := make(chan tea.Msg, 256)
	m.bridge = bridge
	turnCtx, cancel := context.WithCancel(m.ctx)
	m.turnCancel = cancel

	sink := &lineSink{ctx: turnCtx, ch: bridge}
	m.s.out = sink
	go func() {
		_, err := m.s.runTurn(turnCtx, text, nil)
		sink.flush()
		bridge <- turnDoneMsg{err: err}
		close(bridge)
	}()
	return m, m.readNext()
}

// readNext yields the next bridged message, or streamClosedMsg once the turn's
// output channel is closed.
func (m tuiModel) readNext() tea.Cmd {
	ch := m.bridge
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return streamClosedMsg{}
		}
		return msg
	}
}

func (m tuiModel) View() string {
	if !m.ready {
		return "starting flynn..."
	}
	return appStyle.Render(strings.Join([]string{m.vp.View(), m.statusLine(), m.ta.View()}, "\n"))
}

var statusStyle = lipgloss.NewStyle().Faint(true)

func (m tuiModel) statusLine() string {
	hint := "enter: send   ctrl+j: newline   ctrl+d: quit"
	if m.busy {
		hint = "working...   ctrl+c: cancel turn"
	}
	return statusStyle.Render(hint)
}

// layout sizes the viewport and composer to the terminal, seeding the viewport on
// first sight and keeping it pinned to the latest output.
func (m *tuiModel) layout(w, h int) {
	m.width, m.height = w, h
	innerW := w - 2*padX
	if innerW < 1 {
		innerW = 1
	}
	m.contentWidth = innerW
	vpHeight := h - composerHeight - statusHeight - padTop - padBottom
	if vpHeight < 1 {
		vpHeight = 1
	}
	if !m.ready {
		m.vp = viewport.New(innerW, vpHeight)
		m.ready = true
	} else {
		m.vp.Width = innerW
		m.vp.Height = vpHeight
	}
	m.ta.SetWidth(innerW)
	m.refreshViewport()
}

// appendLine adds one line to the transcript and keeps the viewport at the bottom.
func (m *tuiModel) appendLine(s string) {
	m.transcript += s + "\n"
	if m.ready {
		m.refreshViewport()
	}
}

// refreshViewport word-wraps the transcript to the content width and pins the view
// to the latest output, so long lines reflow instead of overflowing the edge.
func (m *tuiModel) refreshViewport() {
	content := m.transcript
	if m.contentWidth > 0 {
		content = lipgloss.NewStyle().Width(m.contentWidth).Render(m.transcript)
	}
	m.vp.SetContent(content)
	m.vp.GotoBottom()
}

// lineSink turns the writes a turn makes into per-line messages on ch, so the
// viewport shows the same transcript the line renderer produces. Sends honor ctx, so
// a cancelled turn never blocks the writing goroutine.
type lineSink struct {
	ctx context.Context
	ch  chan<- tea.Msg
	buf []byte
}

func (s *lineSink) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		line := string(s.buf[:i])
		s.buf = s.buf[i+1:]
		if !s.send(outLineMsg(line)) {
			return len(p), nil
		}
	}
	return len(p), nil
}

// flush emits any trailing partial line (one not ended by a newline).
func (s *lineSink) flush() {
	if len(s.buf) > 0 {
		_ = s.send(outLineMsg(string(s.buf)))
		s.buf = nil
	}
}

func (s *lineSink) send(m tea.Msg) bool {
	select {
	case s.ch <- m:
		return true
	case <-s.ctx.Done():
		return false
	}
}
