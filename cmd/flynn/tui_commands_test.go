package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/input"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/session"
)

// newSlashHost builds a session host over a session that can reach the record commands:
// a signer to seal with, a data directory, and a durable store. It drives the recording
// fake shell instead of a terminal.
func newSlashHost(t *testing.T, model llm.Model) (*sessionHost, *fakeUI) {
	t.Helper()
	s, _ := newSlashSession(t, model)
	ui := &fakeUI{}
	th := theme.Default()
	host := &sessionHost{
		ctx:      context.Background(),
		s:        s,
		ui:       ui,
		th:       th,
		tv:       newTranscriptView(th),
		live:     &activity{th: th},
		panel:    &govPanel{th: th},
		approval: &approvalPrompt{th: th},
		proj:     session.NewProjection(),
	}
	host.liveComp = liveStack{host.approval, host.panel, host.live}
	s.notice = func(text string) { ui.Append(th.Render(theme.Muted, "  "+text)) }
	return host, ui
}

// TestLoadKeymap covers the composer's binding file: absent means the defaults, a valid
// file layers over them, and a file that cannot be parsed is an error naming the file
// rather than a silently discarded set of bindings.
func TestLoadKeymap(t *testing.T) {
	km, err := loadKeymap(t.TempDir())
	if err != nil {
		t.Fatalf("loadKeymap with no file: %v", err)
	}
	if km != nil {
		t.Fatalf("no keymap file should select the defaults, got %v", km)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keymap.json"), []byte(`{"bindings":{"ctrl+w":"editor.kill-word-back"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	km, err = loadKeymap(dir)
	if err != nil {
		t.Fatalf("loadKeymap: %v", err)
	}
	chord, err := editor.ParseChord("ctrl+w")
	if err != nil {
		t.Fatal(err)
	}
	if km[chord] != editor.CmdKillWordBack {
		t.Fatalf("ctrl+w bound to %q, want the command from the file", km[chord])
	}
	if len(km) < 2 {
		t.Fatal("a user keymap must layer over the defaults, not replace them")
	}

	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "keymap.json"), []byte(`{"bindings":{"ctrl+w":"not-an-action"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = loadKeymap(bad)
	if err == nil {
		t.Fatal("an unknown action should fail loadKeymap")
	}
	if !strings.Contains(err.Error(), "keymap.json") {
		t.Fatalf("err = %v, want it to name the offending file", err)
	}
}

// TestShellHostRecordCommandsBeforeRun proves every command that needs a run refuses by
// name in the scrollback before one exists, and never opens one behind the user's back.
func TestShellHostRecordCommandsBeforeRun(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"/seal", "nothing to seal"},
		{"/verify", "nothing to verify"},
		{"/export", "nothing to export"},
		{"/fork", "nothing to fork"},
		{"/compact", "nothing to compact"},
		{"/replay", "nothing recorded to replay yet"},
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			host, ui := newSlashHost(t, llmtest.NewScripted())
			host.submit(c.cmd, nil)
			waitIdle(t, host)
			got := ui.transcript()
			if !strings.Contains(got, c.want) {
				t.Fatalf("%s before a run:\n%s\nwant it to contain %q", c.cmd, got, c.want)
			}
			if host.s.started {
				t.Fatalf("%s opened a run", c.cmd)
			}
		})
	}
}

// TestShellHostRecordCommandsOverRun drives the record commands over one real run in the
// shell: the seal moves the badge to sealed, verify reports every tier and moves it to
// verified, export writes the portable record, fork branches onto a new run, and replay
// re-renders the recorded conversation.
func TestShellHostRecordCommandsOverRun(t *testing.T) {
	t.Chdir(t.TempDir()) // /export writes its file relative to the working directory
	host, ui := newSlashHost(t, llmtest.NewScripted(llmtest.SayText("done")))
	host.submit("do the thing", nil)
	waitIdle(t, host)
	original := host.s.runID
	if original == "" {
		t.Fatal("the turn did not open a run")
	}

	host.submit("/seal", nil)
	waitIdle(t, host)
	if got := ui.transcript(); !strings.Contains(got, "run sealed") {
		t.Fatalf("/seal:\n%s", got)
	}
	if !strings.Contains(host.statusNow(), "sealed") {
		t.Fatalf("the badge did not move to sealed: %q", host.statusNow())
	}

	host.submit("/verify", nil)
	waitIdle(t, host)
	got := ui.transcript()
	for _, want := range []string{"integrity:", "VERIFIED", "record verified"} {
		if !strings.Contains(got, want) {
			t.Fatalf("/verify report missing %q:\n%s", want, got)
		}
	}

	host.submit("/tokens", nil)
	waitIdle(t, host)
	if !strings.Contains(ui.transcript(), "tokens") {
		t.Fatalf("/tokens printed no breakdown:\n%s", ui.transcript())
	}

	host.submit("/export", nil)
	waitIdle(t, host)
	path := original + ".flynnrecord"
	if !strings.Contains(ui.transcript(), path) {
		t.Fatalf("/export did not name the written file:\n%s", ui.transcript())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("exported record not written: %v", err)
	}

	host.submit("/replay", nil)
	waitIdle(t, host)
	if !strings.Contains(ui.transcript(), "end of replay") {
		t.Fatalf("/replay did not render:\n%s", ui.transcript())
	}

	host.submit("/fork", nil)
	waitIdle(t, host)
	if host.s.runID == original {
		t.Fatal("/fork left the session on the original run")
	}
	if !strings.Contains(ui.transcript(), "forked to run "+host.s.runID) {
		t.Fatalf("/fork did not name the new run:\n%s", ui.transcript())
	}
}

// TestShellHostVerifyOfUnsealedRun proves verifying a run that has been driven but never
// sealed reports that it carries no record, and leaves the badge where it was rather than
// claiming a pass.
func TestShellHostVerifyOfUnsealedRun(t *testing.T) {
	host, ui := newSlashHost(t, llmtest.NewScripted(llmtest.SayText("done")))
	host.submit("do the thing", nil)
	waitIdle(t, host)

	host.submit("/verify", nil)
	waitIdle(t, host)
	if strings.Contains(ui.transcript(), "record verified") {
		t.Fatalf("an unsealed run verified:\n%s", ui.transcript())
	}
	if strings.Contains(host.statusNow(), "verified") {
		t.Fatalf("the badge claimed a verified record: %q", host.statusNow())
	}
}

// TestShellHostModelCommands proves /models prints the catalog and /model reports the
// current model, both into the scrollback rather than into the discarded turn output, and
// that a model that cannot be resolved is reported without ending the session.
func TestShellHostModelCommands(t *testing.T) {
	host, ui := newSlashHost(t, llmtest.NewScripted())
	host.submit("/models", nil)
	waitIdle(t, host)
	if !strings.Contains(ui.transcript(), "/models") {
		t.Fatalf("/models was not echoed:\n%s", ui.transcript())
	}

	host.submit("/model", nil)
	waitIdle(t, host)
	if !strings.Contains(ui.transcript(), "current model: openai:gpt-5.5") {
		t.Fatalf("/model did not report the current model:\n%s", ui.transcript())
	}

	host.submit("/model notaprovider:x", nil)
	waitIdle(t, host)
	if !strings.Contains(ui.transcript(), "notaprovider") {
		t.Fatalf("an unresolvable /model was not reported:\n%s", ui.transcript())
	}
	if host.s.modelSpec != "openai:gpt-5.5" {
		t.Fatalf("a failed switch changed the session model to %q", host.s.modelSpec)
	}
}

// TestShellHostCompactAndClear proves /compact summarizes through the model and detaches
// the session from its run, and /clear starts a fresh conversation with the badge reset.
func TestShellHostCompactAndClear(t *testing.T) {
	host, ui := newSlashHost(t, llmtest.NewScripted(
		llmtest.SayText("done"),
		llmtest.SayText("the user asked for the thing; it is done"),
	))
	host.submit("do the thing", nil)
	waitIdle(t, host)

	host.submit("/compact", nil)
	waitIdle(t, host)
	if !strings.Contains(ui.transcript(), "compacted 1 messages") {
		t.Fatalf("/compact did not report the summary:\n%s", ui.transcript())
	}
	if host.s.started {
		t.Fatal("/compact left the session attached to the compacted run")
	}
	if !strings.Contains(host.s.carriedContext, "it is done") {
		t.Fatalf("the summary was not carried forward: %q", host.s.carriedContext)
	}

	host.submit("/clear", nil)
	waitIdle(t, host)
	if !strings.Contains(ui.transcript(), "context cleared") {
		t.Fatalf("/clear did not report:\n%s", ui.transcript())
	}
	if host.s.carriedContext != "" {
		t.Fatal("/clear kept the carried summary")
	}
}

// TestShellHostMemoryCommands proves /remember pins a fact, keeping its interior spacing,
// and /memory shows it back, both through the scrollback.
func TestShellHostMemoryCommands(t *testing.T) {
	host, ui := newSlashHost(t, llmtest.NewScripted())
	host.submit("/remember  the deploy target is fly.io", nil)
	waitIdle(t, host)
	host.submit("/memory", nil)
	waitIdle(t, host)
	host.submit("/skills", nil)
	waitIdle(t, host)
	host.submit("/help", nil)
	waitIdle(t, host)

	got := ui.transcript()
	if !strings.Contains(got, "the deploy target is fly.io") {
		t.Fatalf("/memory did not show the remembered fact:\n%s", got)
	}
	if !strings.Contains(got, "/seal") {
		t.Fatalf("/help did not print the commands:\n%s", got)
	}
}

// TestShellHostPanelToggle proves Ctrl+O opens the governance overlay and Escape closes
// it before it is the interrupt for a turn.
func TestShellHostPanelToggle(t *testing.T) {
	host, _ := newSlashHost(t, llmtest.NewScripted())
	if !host.key(input.Key{Code: 'o', Mods: input.ModCtrl}) {
		t.Fatal("Ctrl+O was not claimed")
	}
	if !host.panel.isOpen() {
		t.Fatal("Ctrl+O did not open the governance panel")
	}
	host.onEsc()
	if host.panel.isOpen() {
		t.Fatal("Escape did not close the open panel")
	}
	// With the panel closed Escape falls through to the interrupt, which is a no-op at
	// idle: the session stays live.
	host.onEsc()
	if host.s.started {
		t.Fatal("Escape at idle started something")
	}
}

// TestShellHostExternalEdit proves Ctrl+G hands the draft to the wired editor and puts
// the result back in the composer, that the shell is suspended for its lifetime, that an
// editor failure is reported instead of clobbering the draft, and that a host with no
// editor leaves the key unclaimed.
func TestShellHostExternalEdit(t *testing.T) {
	ctrlG := input.Key{Code: 'g', Mods: input.ModCtrl}

	host, _ := newSlashHost(t, llmtest.NewScripted())
	if host.key(ctrlG) {
		t.Fatal("Ctrl+G was claimed with no editor wired")
	}

	var seen string
	host, ui := newSlashHost(t, llmtest.NewScripted())
	ui.SetDraft("half a thought")
	host.edit = func(initial string) (string, error) {
		seen = initial
		return "a whole thought\n", nil
	}
	if !host.key(ctrlG) {
		t.Fatal("Ctrl+G with an editor was not claimed")
	}
	if seen != "half a thought" {
		t.Fatalf("the editor got %q, want the current draft", seen)
	}
	if got := ui.Draft(); got != "a whole thought" {
		t.Fatalf("draft after the edit = %q, want the edited text with its trailing newline trimmed", got)
	}
	ui.mu.Lock()
	suspends := ui.suspends
	ui.mu.Unlock()
	if suspends != 1 {
		t.Fatalf("shell suspended %d times, want exactly 1 for the editor handoff", suspends)
	}

	host, ui = newSlashHost(t, llmtest.NewScripted())
	ui.SetDraft("keep me")
	host.edit = func(string) (string, error) { return "", errors.New("editor exploded") }
	if !host.key(ctrlG) {
		t.Fatal("Ctrl+G was not claimed")
	}
	if !strings.Contains(ui.transcript(), "editor exploded") {
		t.Fatalf("the editor failure was not reported:\n%s", ui.transcript())
	}
	if got := ui.Draft(); got != "keep me" {
		t.Fatalf("a failed edit changed the draft to %q", got)
	}
}

// noopEditor points the editor handoff at a command that exits without touching the file,
// so the round trip runs for real without a terminal or a text editor.
func noopEditor(t *testing.T) {
	t.Helper()
	cmd := "true"
	if runtime.GOOS == "windows" {
		cmd = "cmd /c rem"
	}
	t.Setenv("VISUAL", cmd)
	t.Setenv("EDITOR", cmd)
}

// TestEditExternalRoundTrip drives the editor handoff itself: the draft is written to a
// temp file, the terminal is released and reacquired around the editor, and the file's
// contents come back. The temp file never survives the call.
func TestEditExternalRoundTrip(t *testing.T) {
	noopEditor(t)
	var released, reacquired int
	got, err := editExternal("half a thought", func() (func() error, error) {
		released++
		return func() error { reacquired++; return nil }, nil
	})
	if err != nil {
		t.Fatalf("editExternal: %v", err)
	}
	if got != "half a thought" {
		t.Fatalf("editExternal = %q, want the unchanged draft back", got)
	}
	if released != 1 || reacquired != 1 {
		t.Fatalf("terminal round trip = (%d released, %d reacquired), want one of each", released, reacquired)
	}
}

// TestEditExternalReleaseFails proves the editor is never launched when the terminal
// cannot be handed over: the failure surfaces and nothing is reacquired.
func TestEditExternalReleaseFails(t *testing.T) {
	noopEditor(t)
	want := errors.New("cannot leave raw mode")
	if _, err := editExternal("draft", func() (func() error, error) { return nil, want }); !errors.Is(err, want) {
		t.Fatalf("editExternal err = %v, want %v", err, want)
	}
}

// TestEditExternalEditorFails proves an editor that cannot run is reported, and that the
// terminal is reclaimed anyway: the shell always gets its terminal back, whatever the
// editor did.
func TestEditExternalEditorFails(t *testing.T) {
	t.Setenv("VISUAL", "flynn-no-such-editor")
	t.Setenv("EDITOR", "flynn-no-such-editor")
	var reacquired int
	_, err := editExternal("draft", func() (func() error, error) {
		return func() error { reacquired++; return nil }, nil
	})
	if err == nil {
		t.Fatal("a missing editor reported no error")
	}
	if reacquired != 1 {
		t.Fatalf("the terminal was reacquired %d times after a failed editor, want 1", reacquired)
	}
}

// TestRunInteractiveTUIFallsBackWithoutRawMode proves the full-screen shell refuses to
// start when standard input cannot be put into raw mode (it is not a terminal) and hands
// the session to the line interface instead of failing. The shell itself needs a real
// terminal, so this is the branch that is reachable without one.
func TestRunInteractiveTUIFallsBackWithoutRawMode(t *testing.T) {
	replaceStdin(t, "")
	s, buf := newSlashSession(t, llmtest.NewScripted())
	if err := runInteractiveTUI(context.Background(), s); err != nil {
		t.Fatalf("runInteractiveTUI: %v", err)
	}
	if !strings.Contains(buf.String(), "flynn interactive session in") {
		t.Fatalf("the line interface did not take over:\n%s", buf.String())
	}
}

// TestStatusHint pins the one-row hint under the badge: the composer's keys at idle, the
// cancel affordance and the queue depth while a turn runs.
func TestStatusHint(t *testing.T) {
	idle := statusHint(false, 0)
	if !strings.Contains(idle, "ctrl+d quits") {
		t.Fatalf("idle hint = %q", idle)
	}
	busy := statusHint(true, 0)
	if !strings.Contains(busy, "cancels") || strings.Contains(busy, "queued") {
		t.Fatalf("busy hint with an empty queue = %q", busy)
	}
	queued := statusHint(true, 3)
	if !strings.Contains(queued, "3 queued") {
		t.Fatalf("busy hint with a queue = %q", queued)
	}
}
