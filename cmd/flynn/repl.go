package main

// The interactive session itself: the state one conversation carries across its turns,
// the memory every turn reads and writes through, and the read-eval-print loop the line
// interface runs. The front door that assembles it is in repl_start.go, a turn is driven
// in repl_turn.go, and the slash commands are in repl_command.go and repl_record.go.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/memory/curate"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// replSession holds the state of one interactive session across its turns: the
// assembly inputs (model, store, working directory), the identity and cursor of the
// durable run the turns share, and what the session has accumulated for the learning
// pass at the end.
type replSession struct {
	out io.Writer
	// model and plan drive a native session; ext drives an external agent CLI instead.
	// Exactly one of model and ext is set, chosen by the --model spec, and every place
	// that assembles a turn branches on ext rather than asking the model what it is.
	model        llm.Model
	plan         harness.Plan
	ext          *externAgent
	distiller    learn.Distiller
	learnEnabled bool
	// provDeclared records that the run's external provenance has been written to its
	// stream. A record carries one declaration (the first is what a verifier reads), so
	// it is appended once, at the end of the session, when the tallies are complete.
	provDeclared bool
	verbose      bool
	cwd          string
	store        *sqlite.Store
	// mem is the session's memory: the curated write path and the wake digest over
	// it. Every read and write of memory in a session goes through this rather than
	// through store.Memory(), which is the raw durable store with no write policy.
	mem   *memoryStack
	reg   *resource.Registry
	keys  editor.Keymap // composer bindings; nil selects the default map
	theme *theme.Theme  // session theme; nil selects the default theme

	// signer is the instance identity the session seals its run under; nil when no key
	// could be loaded, in which case /seal reports the run cannot be sealed rather than
	// failing. dataDir roots the store the run is verified from.
	signer  chain.RootSigner
	dataDir string

	// modelSpec is the "provider:model" string of the model the session currently
	// drives, shown by /model and updated when /model switches it.
	modelSpec string

	// notice, when set, surfaces an out-of-band session note (currently the recall
	// summary) to the user. The full-screen shell appends it to the transcript; the
	// line interface prints it. Nil discards it, so a non-interactive run is quiet.
	notice func(string)

	// gates carries the session's approval policy and the prompter that resolves a
	// pause. The full-screen shell installs its own prompter (the modal overlay) when it
	// builds the host; the line interface installs none, so a listed action is refused
	// there rather than silently taken.
	gates gateSetup

	// observer, when set, receives every session event as the turn renders. The
	// interactive shell installs it to render the typed stream itself (transcript,
	// governance, status badge); the line interface leaves it nil and reads the
	// flat text renderStream writes to out.
	observer func(session.Event)

	// Per-session run state, set on the first turn and continued by the rest.
	started   bool
	runID     string
	system    string
	objective string
	lastSeq   int64
	converged bool

	// carriedContext is a compacted summary a prior /compact produced, folded into the
	// next fresh run's standing instructions so the thread continues with less context.
	// /clear drops it; /compact sets it.
	carriedContext string

	recalled []string
	// skillset serves skill bodies for every turn and accumulates what the session
	// read across all of them. One set for the session, not one per turn: the outcome
	// the reads are credited against is the session's, so a skill read on turn two of
	// six has to still be on the record when the session ends.
	skillset   *skilltool.Set
	transcript []llm.Message
	lastResult string
}

// memory returns the session's memory: the curated write path and the wake digest
// over it, built on first use.
//
// Every read and write of memory in a session goes through this rather than
// through store.Memory(), so a fact the user pins supersedes the standing answer
// on its subject instead of stacking a second one beside it. Conflict notices go
// wherever the session can speak: the interface's own notice channel once one is
// installed, and the session writer before that. It is built lazily because the
// notice sink is not known when the session is constructed, and because a session
// assembled by hand (a test, a host embedding the shell) then gets the same
// memory the front door does rather than none.
func (s *replSession) memory() *memoryStack {
	if s.mem == nil {
		s.mem = newMemoryStack(s.store.Memory(), func(_ context.Context, n curate.Notice) {
			if s.notice != nil {
				s.notice("memory: " + n.Detail)
				return
			}
			_, _ = fmt.Fprintf(s.out, "  (memory: %s)\n", n.Detail)
		})
	}
	return s.mem
}

// loop is the read-eval-print loop. It reads a message, then drives it as a turn,
// until input ends (Ctrl-D or Ctrl-C at the prompt) or the user types an exit
// command, at which point it runs the session's learning pass and returns. A turn
// error is reported but does not end the session, so a transient failure or a
// cancelled turn returns the user to the prompt.
func (s *replSession) loop(ctx context.Context, in lineReader, sigCh <-chan os.Signal) error {
	for {
		line, err := in.ReadLine()
		if errors.Is(err, io.EOF) {
			_, _ = fmt.Fprintln(s.out)
			return s.finish(ctx)
		}
		if err != nil {
			_, _ = fmt.Fprintf(s.out, "  input error: %v\n", err)
			return s.finish(ctx)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if isExit(line) {
			return s.finish(ctx)
		}
		if handled, err := s.replCommand(ctx, line); handled {
			// verify writes its own per-tier report to out; only a plain error (an
			// unsealed run, a missing key) needs a line of its own here.
			if err != nil && !errors.Is(err, errChecksFailed) {
				_, _ = fmt.Fprintf(s.out, "  %v\n", err)
			}
			continue
		}
		if _, err := s.runTurn(ctx, line, nil, sigCh); err != nil {
			if errors.Is(err, context.Canceled) {
				_, _ = fmt.Fprintln(s.out, "  (turn cancelled)")
			} else {
				_, _ = fmt.Fprintf(s.out, "  error: %v\n", err)
			}
		}
	}
}

// isExit reports whether a line is a command to leave the session.
func isExit(line string) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "exit", "quit", ":q", "/exit", "/quit":
		return true
	}
	return false
}
