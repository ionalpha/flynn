package mission

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/llm"
)

// Briefer is an optional interface on a [Tool]. After the run's tools settle, the
// executor asks each Briefer what to add to the same user turn that carries their
// results, so the text reaches the model on its next call without a model request and
// stays in the stored conversation next to the results it answers.
//
// The text rides in the turn it is added to because a tool_use must be followed by its
// tool_result in the very next message: a separate message between them is rejected by
// the provider, and a rewrite of a stored message is dropped by the checkpoint's
// per-message cache. A Briefer that is a pure function of its input and state replays
// the same way after a crash.
type Briefer interface {
	// Brief is called once before the first model call of a conversation, with Start
	// set, and after every turn whose tools have run. State is what the previous call
	// returned for this tool, empty on the first.
	Brief(ctx context.Context, in BriefInput) (BriefOutput, error)
}

// BriefInput is what a [Briefer] sees.
type BriefInput struct {
	Start bool
	// Calls are the tool calls that settled this turn, in call order. A call made by the
	// spawn tool is not included.
	Calls []ObservedCall
	// State is the Briefer's own state from the checkpoint.
	State json.RawMessage
}

// ObservedCall is one settled tool call. Elapsed times the tool itself, not the wait for
// an approval.
type ObservedCall struct {
	Tool    string
	Input   json.RawMessage
	Elapsed time.Duration
	IsError bool
}

// BriefOutput is a Briefer's contribution to the next turn.
type BriefOutput struct {
	// Text is added to the user turn as one text block; empty adds none.
	Text string
	// State replaces the stored state; empty leaves it unchanged.
	State json.RawMessage
	// Notes are reported as [EventSkill] events so the run record shows each load.
	Notes []BriefNote
}

// BriefNote records one thing a Briefer loaded or declined to load. Via says why it
// was considered: "start" for a skill named for the run, "trigger" for one a declared
// trigger loaded, "skipped" for a named skill that could not load and "suppressed" for
// a trigger that fired past the cap.
type BriefNote struct {
	Name   string
	Via    string
	Reason string
}

// WithToolClock sets the clock that times tool calls for [ObservedCall.Elapsed]. A test
// passes a manual clock its tool advances; the default is the system clock.
func WithToolClock(c clock.Clock) Option {
	return func(e *Executor) {
		if c != nil {
			e.toolClock = c
		}
	}
}

type namedBriefer struct {
	name string
	b    Briefer
}

// findBriefers lists the registered tools that brief, in name order so two Briefers
// always contribute in the same order.
func findBriefers(tools map[string]Tool) []namedBriefer {
	var out []namedBriefer
	for name, t := range tools {
		if b, ok := t.(Briefer); ok {
			out = append(out, namedBriefer{name, b})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// brief asks every Briefer for text and returns it as blocks to append to the user turn
// being built. A Briefer's error is dropped: an optional hint must not fail a step that
// is otherwise sound. Its state advances only on success.
func (e *Executor) brief(ctx context.Context, goalID string, turn int, cp *checkpoint, start bool, calls []ObservedCall) []llm.Block {
	var blocks []llm.Block
	for _, nb := range e.briefers {
		out, err := nb.b.Brief(ctx, BriefInput{Start: start, Calls: calls, State: cp.Briefs[nb.name]})
		if err != nil {
			continue
		}
		if len(out.State) > 0 {
			if cp.Briefs == nil {
				cp.Briefs = make(map[string]json.RawMessage)
			}
			cp.Briefs[nb.name] = slices.Clone(out.State)
		}
		for _, n := range out.Notes {
			e.reporter.Report(ctx, Event{Kind: EventSkill, Goal: goalID, Turn: turn, Skill: n.Name, Via: n.Via, Result: n.Reason})
		}
		if out.Text != "" {
			blocks = append(blocks, llm.Block{Kind: llm.KindText, Text: out.Text})
		}
	}
	return blocks
}
