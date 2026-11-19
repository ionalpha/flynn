package mission

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// ActionSpawn is the action a fan-out runs under, so a run whose grant omits it cannot
// delegate.
const ActionSpawn = "spawn"

// SubGoal is a child of a fan-out. Its Actions or Agent narrow the parent's authority and
// never exceed it.
type SubGoal struct {
	Objective string   `json:"objective"`
	Actions   []string `json:"actions,omitempty"`
	// Agent names an archetype whose prompt and capabilities replace Actions.
	Agent string `json:"agent,omitempty"`
}

// ChildResult becomes the result of the parent's spawn call.
type ChildResult struct {
	ID     string
	Result string
	Failed bool
}

// Fanout runs sub-goals concurrently. An implementation owns each child under the parent,
// narrows its grant and shares the parent's budget, so a fan-out escalates neither
// authority nor spend.
type Fanout interface {
	Spawn(ctx context.Context, parent resource.Resource, sub SubGoal) (id string, err error)
	// Poll reports allDone once every child has finished.
	Poll(ctx context.Context, ids []string) (results []ChildResult, allDone bool, err error)
}

// WithFanout offers the model a spawn tool backed by f. Nil offers none.
func WithFanout(f Fanout) Option {
	return func(e *Executor) { e.fanout = f }
}

// spawnToolDef returns the child's final answer, so a fan-out reads to the model as a slow
// tool call.
var spawnToolDef = llm.Tool{
	Name: ActionSpawn,
	Description: "Delegate a self-contained sub-task to a child agent that runs concurrently. " +
		"Provide a complete objective and either the minimal set of tool actions the child needs, or " +
		"the name of an agent to run it as. The result is the child's final answer. Use this to " +
		"parallelize independent sub-tasks or hand work to a specialist.",
	InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["objective"],
  "properties": {
    "objective": {"type": "string", "minLength": 1},
    "actions": {"type": "array", "items": {"type": "string"}},
    "agent": {"type": "string"}
  },
  "additionalProperties": false
}`),
}

// resultSlot is a tool result owed to the model while a fan-out is in flight. Slots stay in
// call order so the folded results match the order the model issued them.
type resultSlot struct {
	ToolUseID string `json:"toolUseID"`
	ChildID   string `json:"childID,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"isError,omitempty"`
	// ElapsedMs is how long a tool call ran, kept so the brief built when the children
	// fold in sees the timings the first pass measured.
	ElapsedMs int64 `json:"elapsedMs,omitempty"`
}

// dispatchToolUses returns blocks for a turn with no live spawn and pending slots otherwise.
// A failed spawn is an error result for the model, so it does not fail the step. observed
// lists the calls that settled, for [Briefer].
func (e *Executor) dispatchToolUses(ctx context.Context, r resource.Resource, turn int, calls []llm.ToolUse) (blocks []llm.Block, observed []ObservedCall, pending []resultSlot, err error) {
	scope := state.Scope(r.Scope)
	if e.fanout == nil || !containsSpawn(calls) {
		blocks, observed = e.runTools(ctx, r.Name, scope, turn, calls)
		return blocks, observed, nil, nil
	}

	slots := make([]resultSlot, 0, len(calls))
	live := false
	for _, c := range calls {
		if c.Name == ActionSpawn {
			id, objective, serr := e.spawnChild(ctx, r, c)
			if serr != nil {
				slots = append(slots, resultSlot{ToolUseID: c.ID, Content: serr.Error(), IsError: true})
				e.reporter.Report(ctx, Event{Kind: EventToolResult, Goal: r.Name, Turn: turn, Tool: ActionSpawn, ToolUseID: c.ID, Result: serr.Error(), IsError: true})
				continue
			}
			live = true
			slots = append(slots, resultSlot{ToolUseID: c.ID, ChildID: id})
			// ToolUseID pairs this spawn with the child's completion in a tree projection.
			e.reporter.Report(ctx, Event{Kind: EventChildSpawned, Goal: r.Name, Child: id, Turn: turn, Tool: ActionSpawn, ToolUseID: c.ID, Text: objective})
			continue
		}
		content, elapsed, terr := e.invoke(ctx, r.Name, scope, c)
		slot := resultSlot{ToolUseID: c.ID, Content: content, ElapsedMs: elapsed.Milliseconds()}
		if terr != nil {
			slot.IsError, slot.Content = true, terr.Error()
		}
		e.reporter.Report(ctx, Event{Kind: EventToolResult, Goal: r.Name, Turn: turn, Tool: c.Name, ToolUseID: c.ID, Result: slot.Content, IsError: slot.IsError})
		slots = append(slots, slot)
	}
	// Every spawn was rejected, so there is nothing to wait for.
	if !live {
		return slotsToBlocks(slots), observedFromSlots(slots, calls), nil, nil
	}
	return nil, nil, slots, nil
}

// spawnChild returns the objective too, so the caller can record the parent-to-child edge.
func (e *Executor) spawnChild(ctx context.Context, r resource.Resource, c llm.ToolUse) (id, objective string, err error) {
	var sub SubGoal
	if err := json.Unmarshal(c.Input, &sub); err != nil {
		return "", "", fault.New(fault.Terminal, "spawn_args", "invalid spawn arguments: "+err.Error())
	}
	if strings.TrimSpace(sub.Objective) == "" {
		return "", "", fault.New(fault.Terminal, "spawn_objective", "spawn requires a non-empty objective")
	}
	err = e.dispatcher.Govern(ctx, dispatch.Action{Name: ActionSpawn, Scope: state.Scope(r.Scope), Trust: sandbox.TrustTrusted, Goal: r.Name},
		func(ctx context.Context) (dispatch.Metering, error) {
			child, serr := e.fanout.Spawn(ctx, r, sub)
			id = child
			return dispatch.Metering{}, serr
		})
	return id, sub.Objective, err
}

// advanceFanout returns goal.ErrWaiting while a child runs, which parks the parent with no
// checkpoint write and no step counted, until a child settles.
func (e *Executor) advanceFanout(ctx context.Context, r resource.Resource, cp checkpoint) (json.RawMessage, error) {
	results, allDone, err := e.fanout.Poll(ctx, childIDs(cp.Pending))
	if err != nil {
		return nil, fault.Wrap(fault.Transient, "mission_fanout_poll", err)
	}
	if !allDone {
		return nil, goal.ErrWaiting // children still running; park until one settles
	}
	byID := make(map[string]ChildResult, len(results))
	for _, cr := range results {
		byID[cr.ID] = cr
	}
	for i := range cp.Pending {
		if id := cp.Pending[i].ChildID; id != "" {
			cr := byID[id]
			cp.Pending[i].Content, cp.Pending[i].IsError = cr.Result, cr.Failed
			// The only place a successful child's result reaches the stream.
			e.reporter.Report(ctx, Event{Kind: EventChildCompleted, Goal: r.Name, Child: id, Tool: ActionSpawn, ToolUseID: cp.Pending[i].ToolUseID, Result: cr.Result, IsError: cr.Failed})
		}
	}
	blocks := slotsToBlocks(cp.Pending)
	if n := len(cp.Messages); n > 0 {
		// The assistant message that issued these calls is the last one stored.
		calls := cp.Messages[n-1].ToolUses()
		blocks = append(blocks, e.brief(ctx, r.Name, cp.Turns, &cp, false, observedFromSlots(cp.Pending, calls))...)
	}
	cp.Messages = append(cp.Messages, llm.Message{Role: llm.RoleUser, Blocks: blocks})
	cp.Pending = nil
	return encodeCheckpoint(cp)
}

// observedFromSlots pairs each settled slot with the call that produced it. A spawn is
// left out: it is a delegation, not a command the run ran.
func observedFromSlots(slots []resultSlot, calls []llm.ToolUse) []ObservedCall {
	byID := make(map[string]llm.ToolUse, len(calls))
	for _, c := range calls {
		byID[c.ID] = c
	}
	var out []ObservedCall
	for _, s := range slots {
		c, ok := byID[s.ToolUseID]
		if !ok || s.ChildID != "" || c.Name == ActionSpawn {
			continue
		}
		out = append(out, ObservedCall{Tool: c.Name, Input: c.Input, Elapsed: time.Duration(s.ElapsedMs) * time.Millisecond, IsError: s.IsError})
	}
	return out
}

func containsSpawn(calls []llm.ToolUse) bool {
	for _, c := range calls {
		if c.Name == ActionSpawn {
			return true
		}
	}
	return false
}

func childIDs(pending []resultSlot) []string {
	ids := make([]string, 0, len(pending))
	for _, s := range pending {
		if s.ChildID != "" {
			ids = append(ids, s.ChildID)
		}
	}
	return ids
}

func slotsToBlocks(slots []resultSlot) []llm.Block {
	blocks := make([]llm.Block, 0, len(slots))
	for _, s := range slots {
		blocks = append(blocks, llm.Block{Kind: llm.KindToolResult, ToolResult: &llm.ToolResult{ToolUseID: s.ToolUseID, Content: s.Content, IsError: s.IsError}})
	}
	return blocks
}
