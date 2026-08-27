package main

// The session's slash commands, as one table.
//
// Four things have to agree about which commands exist: the /help listing, the
// composer's completion menu, the line interface's dispatch and the full-screen
// interface's dispatch. Until this table they were four hand-kept lists in four files
// with nothing checking them against each other, so a command added to three of them
// shipped missing from the fourth and nothing went red. A command is now written once,
// here, with the handler each interface runs, and the three lists are readings of it.

import (
	"context"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/session"
)

// sessionCommand is one slash command of the interactive session.
//
// line and tui are the same command as each interface has to run it, and they are not
// the same code: the line interface writes plain text to the session writer and hands
// its error back to the loop to report, while the full-screen one renders themed lines
// into the scrollback and runs the command as a queued turn. What the two must never
// disagree about is which commands exist and what they are called, which is the part
// this table holds. Both are required; TestCommandTableRowsAreComplete fails on a row
// that leaves either nil, because a missing handler is a nil call at the keystroke
// rather than something the compiler catches.
type sessionCommand struct {
	name string
	// alias is a second spelling that runs the same command ("?" for /help), listed in
	// /help beside the name but not offered as a completion candidate.
	alias string
	// arg is the argument's placeholder in the /help listing, empty when the command
	// takes none. It doubles as the answer to whether the composer leaves a trailing
	// space after completing the name, so the argument can be typed straight away.
	arg  string
	desc string
	line func(s *replSession, ctx context.Context, arg string) error
	tui  func(h *sessionHost, ctx context.Context, arg string)
}

// sessionCommands returns every slash command the session understands, in the order
// /help lists them. That order is the one a user learns, so it is the one the completion
// menu offers as well.
//
// It is a function rather than a package variable because /help's own row runs
// renderHelp and renderHelp reads the table: as a variable that is an initialisation
// cycle, which the compiler refuses. Between functions the same loop is legal, and
// building fourteen rows per call costs nothing next to rendering the menu they feed.
func sessionCommands() []sessionCommand {
	return []sessionCommand{
		{
			name: "/model", arg: "[provider:model]",
			desc: "show the current model, or switch to one and save it as the default",
			line: func(s *replSession, ctx context.Context, arg string) error {
				return s.switchModel(ctx, strings.Fields(arg), s.out)
			},
			tui: func(h *sessionHost, ctx context.Context, arg string) { h.doModel(ctx, strings.Fields(arg)) },
		},
		{
			name: "/models",
			desc: "list the model catalog",
			line: func(s *replSession, _ context.Context, _ string) error { return s.showCatalog(s.out) },
			tui:  func(h *sessionHost, ctx context.Context, _ string) { h.doModels(ctx) },
		},
		{
			name: "/seal",
			desc: "seal the run into a verifiable record",
			line: func(s *replSession, ctx context.Context, _ string) error {
				if err := s.seal(ctx); err != nil {
					return err
				}
				_, _ = fmt.Fprintln(s.out, "  run sealed; /verify to check it")
				return nil
			},
			tui: func(h *sessionHost, ctx context.Context, _ string) { h.doSeal(ctx) },
		},
		{
			name: "/verify",
			desc: "verify the sealed record, tier by tier",
			line: func(s *replSession, ctx context.Context, _ string) error { return s.verify(ctx, s.out) },
			tui:  func(h *sessionHost, ctx context.Context, _ string) { h.doVerify(ctx) },
		},
		{
			name: "/export",
			desc: "write the sealed record to a portable file",
			line: func(s *replSession, ctx context.Context, _ string) error {
				path, err := s.export(ctx, "")
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(s.out, "  record exported to %s; verify anywhere with: flynn spine verify --file %s\n", path, path)
				return nil
			},
			tui: func(h *sessionHost, ctx context.Context, _ string) { h.doExport(ctx) },
		},
		{
			name: "/fork",
			desc: "branch the run into a new one, leaving this one untouched",
			line: func(s *replSession, ctx context.Context, _ string) error {
				forkID, err := s.fork(ctx)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(s.out, "  forked to run %s; the original is untouched\n", forkID)
				return nil
			},
			tui: func(h *sessionHost, ctx context.Context, _ string) { h.doFork(ctx) },
		},
		{
			name: "/replay",
			desc: "re-render the run from its record",
			line: func(s *replSession, ctx context.Context, _ string) error {
				hist, _, err := renderHistory(ctx, s.store, s.runID, s.verbose)
				if err != nil {
					return err
				}
				if strings.TrimSpace(hist) == "" {
					_, _ = fmt.Fprintln(s.out, "  nothing recorded to replay yet")
					return nil
				}
				_, _ = fmt.Fprint(s.out, hist)
				return nil
			},
			tui: func(h *sessionHost, ctx context.Context, _ string) { h.doReplay(ctx) },
		},
		{
			name: "/tokens",
			desc: "break down this run's token usage",
			line: func(s *replSession, ctx context.Context, _ string) error {
				u, turns := session.Usage{}, 0
				if s.runID != "" {
					if events, herr := session.History(ctx, s.store.Log(), s.runID); herr == nil {
						p := session.Project(events)
						u, turns = p.Usage, p.Turns
					}
				}
				renderTokens(s.out, u, turns)
				return nil
			},
			tui: func(h *sessionHost, ctx context.Context, _ string) { h.doTokens(ctx) },
		},
		{
			name: "/memory",
			desc: "show what the agent remembers across runs",
			line: func(s *replSession, ctx context.Context, _ string) error {
				renderMemory(ctx, s.out, s.memory().store)
				return nil
			},
			tui: func(h *sessionHost, ctx context.Context, _ string) { h.doMemory(ctx) },
		},
		{
			name: "/remember", arg: "<fact>",
			desc: "pin a fact into memory, so it is recalled in later runs",
			line: func(s *replSession, ctx context.Context, arg string) error {
				rememberFact(ctx, s.out, s.memory().store, arg)
				return nil
			},
			tui: func(h *sessionHost, ctx context.Context, arg string) { h.doRemember(ctx, arg) },
		},
		{
			name: "/skills",
			desc: "show the skills the agent has learned",
			line: func(s *replSession, ctx context.Context, _ string) error {
				renderSkills(ctx, s.out, s.store.Skills())
				return nil
			},
			tui: func(h *sessionHost, ctx context.Context, _ string) { h.doSkills(ctx) },
		},
		{
			name: "/compact",
			desc: "summarize the conversation to continue with less context",
			line: func(s *replSession, ctx context.Context, _ string) error {
				n, err := s.compact(ctx)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(s.out, "  compacted %d messages into a summary; continuing with less context\n", n)
				return nil
			},
			tui: func(h *sessionHost, ctx context.Context, _ string) { h.doCompact(ctx) },
		},
		{
			name: "/clear",
			desc: "start a fresh conversation",
			line: func(s *replSession, _ context.Context, _ string) error {
				s.clear()
				_, _ = fmt.Fprintln(s.out, "  context cleared; starting a fresh conversation")
				return nil
			},
			tui: func(h *sessionHost, ctx context.Context, _ string) { h.doClear(ctx) },
		},
		{
			name: "/help", alias: "?",
			desc: "show this list",
			line: func(s *replSession, _ context.Context, _ string) error {
				renderHelp(s.out)
				return nil
			},
			tui: func(h *sessionHost, ctx context.Context, _ string) { h.doHelp(ctx) },
		},
	}
}

// lookupCommand matches a typed line against the table and returns the command it
// names along with the rest of the line as its argument.
//
// It matches on the first word rather than the whole line, so a command that ignores
// arguments still runs when the user types something after it, and it lowercases that
// word, so a command is a command however it was capitalised. Both interfaces route
// through here, which is what stops them disagreeing about which lines are commands and
// which reach the model as prompts. The argument keeps its interior spacing, because
// /remember pins the fact as the user wrote it.
func lookupCommand(line string) (cmd sessionCommand, arg string, ok bool) {
	line = strings.TrimSpace(line)
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return sessionCommand{}, "", false
	}
	word := strings.ToLower(fields[0])
	for _, c := range sessionCommands() {
		if word == c.name || (c.alias != "" && word == c.alias) {
			return c, strings.TrimSpace(strings.TrimPrefix(line, fields[0])), true
		}
	}
	return sessionCommand{}, "", false
}

// helpKey is how /help spells a command: the name, its alias when it has one, and the
// placeholder for its argument.
func (c sessionCommand) helpKey() string {
	key := c.name
	if c.alias != "" {
		key += ", " + c.alias
	}
	if c.arg != "" {
		key += " " + c.arg
	}
	return key
}
