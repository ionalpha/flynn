package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/state"
)

// newSlashSession builds a session with everything the slash commands reach for: a
// durable store, a data directory, an instance signer (so /seal can sign), and a model
// spec to report. Output is captured in the returned buffer.
func newSlashSession(t *testing.T, model llm.Model) (*replSession, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	dir := t.TempDir()
	return &replSession{
		out:       &syncWriter{w: buf},
		model:     model,
		verbose:   true,
		cwd:       dir,
		dataDir:   dir,
		store:     memStore(t),
		reg:       mustRegistry(t),
		signer:    selfCertifyingSigner(t),
		modelSpec: "openai:gpt-5.5",
	}, buf
}

// replaceStdin points os.Stdin at a regular file holding content, so code that reaches
// for the process's terminal deterministically finds one that is not a terminal (raw
// mode fails on a file). The original is restored when the test ends.
func replaceStdin(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path) //nolint:gosec // a fixed path under the test's temp dir
	if err != nil {
		t.Fatal(err)
	}
	prior := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = prior
		_ = f.Close()
	})
}

// captureStdout points os.Stdout at a temp file and returns a reader for what was
// written to it, so a command that prints to the process's own stdout can be asserted on
// without spilling into the test log.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdout")
	f, err := os.Create(path) //nolint:gosec // a fixed path under the test's temp dir
	if err != nil {
		t.Fatal(err)
	}
	prior := os.Stdout
	os.Stdout = f
	t.Cleanup(func() {
		os.Stdout = prior
		_ = f.Close()
	})
	return func() string {
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path) //nolint:gosec // the file created just above
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
}

// TestReplCommandDispatch drives every slash command the session answers before a turn
// has run: each one is claimed off the line, the read-only ones report, and the ones that
// need a run refuse by name instead of failing the session. A line that is not a command
// is left for the model.
func TestReplCommandDispatch(t *testing.T) {
	cases := []struct {
		name        string
		line        string
		wantHandled bool
		wantErr     string // substring of the returned error; empty means no error
		wantOut     string // substring of what the command wrote
	}{
		{name: "help", line: "/help", wantHandled: true, wantOut: "/seal"},
		{name: "help alias", line: "?", wantHandled: true, wantOut: "/seal"},
		{name: "case insensitive", line: "/HELP", wantHandled: true, wantOut: "/seal"},
		{name: "clear", line: "/clear", wantHandled: true, wantOut: "context cleared"},
		{name: "memory", line: "/memory", wantHandled: true},
		{name: "skills", line: "/skills", wantHandled: true},
		{name: "tokens", line: "/tokens", wantHandled: true, wantOut: "tokens"},
		{name: "models", line: "/models", wantHandled: true},
		{name: "model with no argument reports", line: "/model", wantHandled: true, wantOut: "current model: openai:gpt-5.5"},
		{name: "seal before a turn", line: "/seal", wantHandled: true, wantErr: "nothing to seal"},
		{name: "verify before a turn", line: "/verify", wantHandled: true, wantErr: "nothing to verify"},
		{name: "export before a turn", line: "/export", wantHandled: true, wantErr: "nothing to export"},
		{name: "fork before a turn", line: "/fork", wantHandled: true, wantErr: "nothing to fork"},
		{name: "compact before a turn", line: "/compact", wantHandled: true, wantErr: "nothing to compact"},
		{name: "a message is not a command", line: "hello there", wantHandled: false},
		{name: "a bare slash word is not a command", line: "/nope", wantHandled: false},
		{name: "blank", line: "   ", wantHandled: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, buf := newSlashSession(t, llmtest.NewScripted())
			handled, err := s.replCommand(context.Background(), c.line)
			if handled != c.wantHandled {
				t.Fatalf("replCommand(%q) handled = %v, want %v", c.line, handled, c.wantHandled)
			}
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("replCommand(%q) = %v, want no error", c.line, err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("replCommand(%q) err = %v, want one naming %q", c.line, err, c.wantErr)
			}
			if c.wantOut != "" && !strings.Contains(buf.String(), c.wantOut) {
				t.Fatalf("replCommand(%q) wrote:\n%s\nwant it to contain %q", c.line, buf.String(), c.wantOut)
			}
		})
	}
}

// TestReplCommandRemember proves /remember pins the rest of the line, interior spacing
// intact, into durable memory rather than sending it to the model.
func TestReplCommandRemember(t *testing.T) {
	ctx := context.Background()
	s, buf := newSlashSession(t, llmtest.NewScripted())

	handled, err := s.replCommand(ctx, "/remember the deploy target is fly.io")
	if !handled || err != nil {
		t.Fatalf("/remember = (%v, %v), want (true, nil)", handled, err)
	}
	got, err := s.store.Memory().Recall(ctx, state.RecallQuery{Query: "deploy", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range got {
		if strings.Contains(m.Content, "the deploy target is fly.io") {
			found = true
		}
	}
	if !found {
		t.Fatalf("/remember did not store the fact: %+v\n%s", got, buf.String())
	}
}

// TestReplCommandRecordLifecycle drives the record commands over one real run: sealing
// signs it, verifying reports every tier, exporting writes the portable record, forking
// branches onto a new id, and replay re-renders the recorded conversation. It is the
// whole in-session record surface, in the order a user reaches it.
func TestReplCommandRecordLifecycle(t *testing.T) {
	t.Chdir(t.TempDir()) // /export writes its file relative to the working directory
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, buf := newSlashSession(t, llmtest.NewScripted(llmtest.SayText("done")))
	if _, err := s.runTurn(ctx, "do the thing", nil, nil); err != nil {
		t.Fatalf("turn: %v\n%s", err, buf.String())
	}
	original := s.runID

	// Verifying before the seal reports the run carries no record, and does not claim a pass.
	if _, err := s.replCommand(ctx, "/verify"); err == nil {
		t.Fatal("/verify of an unsealed run reported no error")
	}

	buf.Reset()
	if handled, err := s.replCommand(ctx, "/seal"); !handled || err != nil {
		t.Fatalf("/seal = (%v, %v)\n%s", handled, err, buf.String())
	}
	if !strings.Contains(buf.String(), "run sealed") {
		t.Fatalf("/seal did not report the seal:\n%s", buf.String())
	}

	buf.Reset()
	if handled, err := s.replCommand(ctx, "/verify"); !handled || err != nil {
		t.Fatalf("/verify after seal = (%v, %v)\n%s", handled, err, buf.String())
	}
	for _, want := range []string{"integrity:", "VERIFIED", "governance:"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("/verify report missing %q:\n%s", want, buf.String())
		}
	}

	buf.Reset()
	if handled, err := s.replCommand(ctx, "/export"); !handled || err != nil {
		t.Fatalf("/export = (%v, %v)\n%s", handled, err, buf.String())
	}
	path := original + ".flynnrecord"
	if !strings.Contains(buf.String(), path) {
		t.Fatalf("/export did not name the written file:\n%s", buf.String())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("exported record not written: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("exported record is empty")
	}

	buf.Reset()
	if handled, err := s.replCommand(ctx, "/tokens"); !handled || err != nil {
		t.Fatalf("/tokens = (%v, %v)", handled, err)
	}
	if !strings.Contains(buf.String(), "turn") {
		t.Fatalf("/tokens did not report the run's turns:\n%s", buf.String())
	}

	buf.Reset()
	if handled, err := s.replCommand(ctx, "/replay"); !handled || err != nil {
		t.Fatalf("/replay = (%v, %v)\n%s", handled, err, buf.String())
	}
	if !strings.Contains(buf.String(), "do the thing") {
		t.Fatalf("/replay did not re-render the conversation:\n%s", buf.String())
	}

	buf.Reset()
	if handled, err := s.replCommand(ctx, "/fork"); !handled || err != nil {
		t.Fatalf("/fork = (%v, %v)\n%s", handled, err, buf.String())
	}
	if s.runID == original {
		t.Fatal("/fork left the session on the original run")
	}
	if !strings.Contains(buf.String(), s.runID) {
		t.Fatalf("/fork did not name the new run:\n%s", buf.String())
	}

	// The fork's own stream is empty until its first turn, so replaying it says so
	// rather than showing the parent's history.
	buf.Reset()
	if handled, err := s.replCommand(ctx, "/replay"); !handled || err != nil {
		t.Fatalf("/replay on the fork = (%v, %v)", handled, err)
	}
	if !strings.Contains(buf.String(), "nothing recorded to replay yet") {
		t.Fatalf("replay of a fresh fork:\n%s", buf.String())
	}
}

// TestReplCommandCompact proves /compact summarizes the conversation through the model
// and continues from the summary: the session detaches from its run and carries the
// summary into the next one's standing instructions.
func TestReplCommandCompact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, buf := newSlashSession(t, llmtest.NewScripted(
		llmtest.SayText("done"),
		llmtest.SayText("the user asked for the thing; it is done"),
	))
	if _, err := s.runTurn(ctx, "do the thing", nil, nil); err != nil {
		t.Fatalf("turn: %v\n%s", err, buf.String())
	}

	buf.Reset()
	if handled, err := s.replCommand(ctx, "/compact"); !handled || err != nil {
		t.Fatalf("/compact = (%v, %v)\n%s", handled, err, buf.String())
	}
	if !strings.Contains(buf.String(), "compacted") {
		t.Fatalf("/compact did not report:\n%s", buf.String())
	}
	if s.started || s.runID != "" {
		t.Fatal("/compact left the session attached to the compacted run")
	}
	if !strings.Contains(s.carriedContext, "it is done") {
		t.Fatalf("the summary was not carried into the next run: %q", s.carriedContext)
	}
}

// TestReplLoopRunsCommandsAndReportsErrors drives the loop over a scripted script of
// lines: a command is claimed and never reaches the model, its error is reported without
// ending the session, and the exit line still closes it.
func TestReplLoopRunsCommandsAndReportsErrors(t *testing.T) {
	s, buf := newSlashSession(t, llmtest.NewScripted())
	lines := &scriptedLines{lines: []string{"/help", "/seal", "exit"}}
	if err := s.loop(context.Background(), lines, nil); err != nil {
		t.Fatalf("loop: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "nothing to seal") {
		t.Fatalf("the command's error was not reported:\n%s", out)
	}
	if !strings.Contains(out, "goodbye") {
		t.Fatalf("the session did not end:\n%s", out)
	}
}

// errLines is a lineReader whose read fails, standing in for a terminal that breaks
// mid-session.
type errLines struct{ err error }

func (r errLines) ReadLine() (string, error) { return "", r.err }

// TestReplLoopInputErrorEndsSession proves a broken input stream is reported and closes
// the session cleanly rather than spinning on the failing reader.
func TestReplLoopInputErrorEndsSession(t *testing.T) {
	s, buf := newSlashSession(t, llmtest.NewScripted())
	if err := s.loop(context.Background(), errLines{err: os.ErrClosed}, nil); err != nil {
		t.Fatalf("loop: %v", err)
	}
	if !strings.Contains(buf.String(), "input error") {
		t.Fatalf("the input failure was not reported:\n%s", buf.String())
	}
}

// TestRunLineModeWithoutTerminal proves the line interface refuses to read when standard
// input is not a terminal: it prints its banner, reports that it cannot enter line
// editing, and leaves the session cleanly instead of blocking on a pipe.
func TestRunLineModeWithoutTerminal(t *testing.T) {
	replaceStdin(t, "hello\n")
	s, buf := newSlashSession(t, llmtest.NewScripted())
	if err := s.runLineMode(context.Background(), s.cwd); err != nil {
		t.Fatalf("runLineMode: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"flynn interactive session in", "openai:gpt-5.5", "input error", "goodbye"} {
		if !strings.Contains(out, want) {
			t.Fatalf("line mode output missing %q:\n%s", want, out)
		}
	}
	if s.started {
		t.Fatal("no turn should have run without a readable terminal")
	}
}

// TestRunInteractiveWithoutTerminal drives the whole no-subcommand entry point with no
// terminal attached: it resolves the model, opens the store, skips the resume picker
// (there is nobody to prompt), takes the line interface, and leaves cleanly. It is the
// assembly the interactive session is built from, exercised end to end.
func TestRunInteractiveWithoutTerminal(t *testing.T) {
	noProviderKeys(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Chdir(t.TempDir())
	replaceStdin(t, "")
	stdout := captureStdout(t)

	if err := runInteractive("openai:gpt-5.5", t.TempDir(), false, false, true); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	out := stdout()
	for _, want := range []string{"flynn interactive session in", "openai:gpt-5.5", "goodbye"} {
		if !strings.Contains(out, want) {
			t.Fatalf("interactive output missing %q:\n%s", want, out)
		}
	}
}

// TestRunInteractiveReportsUnresolvableModel proves the entry point surfaces a model that
// cannot be resolved instead of opening a session on nothing.
func TestRunInteractiveReportsUnresolvableModel(t *testing.T) {
	noProviderKeys(t)
	t.Chdir(t.TempDir())
	replaceStdin(t, "")
	captureStdout(t)

	err := runInteractive("notaprovider:x", t.TempDir(), false, false, true)
	if err == nil {
		t.Fatal("an unresolvable model opened a session")
	}
}

// TestTerminalDetection pins what the session's front-door checks actually measure: a
// regular file is never a terminal, the null device is a character device, and the
// process-wide checks read exactly that of their own stream.
func TestTerminalDetection(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "plain"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if isCharDevice(f) {
		t.Fatal("a regular file reported as a character device")
	}

	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	if !isCharDevice(null) {
		t.Fatalf("%s did not report as a character device", os.DevNull)
	}

	// A closed file cannot be stat'd, which reads as "no terminal" rather than a panic.
	closed, err := os.Create(filepath.Join(t.TempDir(), "closed"))
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	if isCharDevice(closed) {
		t.Fatal("a closed file reported as a character device")
	}

	// The process-wide checks are that same test over the process's own streams.
	replaceStdin(t, "")
	if stdinIsTerminal() {
		t.Fatal("a regular file on standard input reported as a terminal")
	}
	captureStdout(t)
	if stdoutIsTerminal() {
		t.Fatal("a regular file on standard output reported as a terminal")
	}
}
