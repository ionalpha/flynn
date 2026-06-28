package mission

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// ActionSpawn is the dispatch action a fan-out runs under, so spawning a sub-goal is admitted
// against the run's grant, metered, and recorded on the spine like any other action. A run whose
// grant does not list it cannot fan out, which keeps delegation a least-privilege decision.
const ActionSpawn = "spawn"

// SubGoal is a child goal a parent asks to run as part of a fan-out: an objective and either the
// actions the child may take or a named Agent to run it as. The actions (or the named Agent's
// capabilities) narrow the parent's authority, since a delegated run can never exceed the authority
// of the run that spawned it.
type SubGoal struct {
	Objective string   `json:"objective"`
	Actions   []string `json:"actions,omitempty"`
	// Agent, when set, names an Agent archetype to run the child as: its system prompt and
	// capabilities configure the child (intersected with the parent's authority), instead of the
	// ad-hoc Actions list. Empty runs an ad-hoc child from Actions.
	Agent string `json:"agent,omitempty"`
}

// ChildResult is the outcome of a spawned child, folded back into the parent's conversation as the
// result of its spawn call.
type ChildResult struct {
	ID     string
	Result string
	Failed bool
}

// Fanout spawns child goals and reports their outcomes, so a parent goal can run sub-goals
// concurrently and fold their results into its own conversation. The implementation attaches each
// child to the parent (ownership and cascade teardown), narrows the grant to the requested
// actions, and shares the parent's budget, so a fan-out can neither escalate authority nor exceed
// the run's budget. A nil Fanout disables fan-out: the spawn tool is not offered and a goal runs as
// a single conversation, which is the n=1 case of the same mechanism.
type Fanout interface {
	// Spawn creates a child goal owned by parent and returns its id.
	Spawn(ctx context.Context, parent resource.Resource, sub SubGoal) (id string, err error)
	// Poll reports the children's outcomes and whether all of them have finished, so the parent
	// knows when to fold. While any child is still running, allDone is false.
	Poll(ctx context.Context, ids []string) (results []ChildResult, allDone bool, err error)
}

// WithFanout enables fan-out: the model is offered a spawn tool, and a call to it creates a child
// goal through f. The default is nil, which leaves a goal as a single conversation.
func WithFanout(f Fanout) Option {
	return func(e *Executor) { e.fanout = f }
}

// spawnToolDef is the tool the model calls to fan out. Its result is the child's final answer,
// delivered once the child finishes, so to the model a fan-out reads as an ordinary tool call that
// happens to take a while.
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

// resultSlot is one tool-result the turn owes the model, held on the checkpoint while a fan-out is
// in flight. A normal tool's result is captured immediately in Content; a spawn's result is filled
// from its child once the child finishes (ChildID identifies which). Keeping the slots in tool-call
// order means the folded results are appended in the order the model issued the calls.
type resultSlot struct {
	ToolUseID string `json:"toolUseID"`
	ChildID   string `json:"childID,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"isError,omitempty"`
}

// dispatchToolUses runs a turn's tool calls. A turn with no spawn call takes the ordinary path
// unchanged, returning the result blocks to append now. A turn that spawns at least one child
// instead returns pending slots, in call order, so the parent waits and folds the children's
// results in later: a normal call's result is captured immediately, a spawn's is left for its
// child. A spawn that fails (rejected, malformed, or a spawner error) becomes an error result the
// model sees rather than a step failure.
func (e *Executor) dispatchToolUses(ctx context.Context, r resource.Resource, turn int, calls []llm.ToolUse) (blocks []llm.Block, pending []resultSlot, err error) {
	scope := state.Scope(r.Scope)
	if e.fanout == nil || !containsSpawn(calls) {
		return e.runTools(ctx, scope, turn, calls), nil, nil
	}

	slots := make([]resultSlot, 0, len(calls))
	live := false
	for _, c := range calls {
		if c.Name == ActionSpawn {
			id, serr := e.spawnChild(ctx, r, c)
			if serr != nil {
				slots = append(slots, resultSlot{ToolUseID: c.ID, Content: serr.Error(), IsError: true})
				e.reporter.Report(ctx, Event{Kind: EventToolResult, Turn: turn, Tool: ActionSpawn, ToolUseID: c.ID, Result: serr.Error(), IsError: true})
				continue
			}
			live = true
			slots = append(slots, resultSlot{ToolUseID: c.ID, ChildID: id})
			continue
		}
		content, terr := e.invokeTool(ctx, scope, c)
		slot := resultSlot{ToolUseID: c.ID, Content: content}
		if terr != nil {
			slot.IsError, slot.Content = true, terr.Error()
		}
		e.reporter.Report(ctx, Event{Kind: EventToolResult, Turn: turn, Tool: c.Name, ToolUseID: c.ID, Result: slot.Content, IsError: slot.IsError})
		slots = append(slots, slot)
	}
	// With no live child (every spawn was rejected), there is nothing to wait for, so fold the
	// results now rather than entering a wait that would resolve immediately.
	if !live {
		return slotsToBlocks(slots), nil, nil
	}
	return nil, slots, nil
}

// spawnChild governs a spawn through the waist and creates the child goal. The action is admitted
// against the run's grant, so a run whose grant omits spawn cannot fan out, and it is metered and
// recorded on the spine like any other action.
func (e *Executor) spawnChild(ctx context.Context, r resource.Resource, c llm.ToolUse) (string, error) {
	var sub SubGoal
	if err := json.Unmarshal(c.Input, &sub); err != nil {
		return "", fault.New(fault.Terminal, "spawn_args", "invalid spawn arguments: "+err.Error())
	}
	if strings.TrimSpace(sub.Objective) == "" {
		return "", fault.New(fault.Terminal, "spawn_objective", "spawn requires a non-empty objective")
	}
	var id string
	err := e.dispatcher.Govern(ctx, dispatch.Action{Name: ActionSpawn, Scope: state.Scope(r.Scope), Trust: sandbox.TrustTrusted},
		func(ctx context.Context) (dispatch.Metering, error) {
			child, serr := e.fanout.Spawn(ctx, r, sub)
			id = child
			return dispatch.Metering{}, serr
		})
	return id, err
}

// advanceFanout is the waiting step of a fan-out: poll the children, and either fold their results
// into the conversation once they have all finished or leave the checkpoint unchanged while any is
// still running. Folding fills each spawn slot from its child and preserves call order, so the
// model receives the spawn results as ordinary tool results in the order it issued them.
func (e *Executor) advanceFanout(ctx context.Context, _ resource.Resource, cp checkpoint) (json.RawMessage, error) {
	results, allDone, err := e.fanout.Poll(ctx, childIDs(cp.Pending))
	if err != nil {
		return nil, fault.Wrap(fault.Transient, "mission_fanout_poll", err)
	}
	if !allDone {
		return encodeCheckpoint(cp) // children still running; wait
	}
	byID := make(map[string]ChildResult, len(results))
	for _, cr := range results {
		byID[cr.ID] = cr
	}
	for i := range cp.Pending {
		if id := cp.Pending[i].ChildID; id != "" {
			cr := byID[id]
			cp.Pending[i].Content, cp.Pending[i].IsError = cr.Result, cr.Failed
		}
	}
	cp.Messages = append(cp.Messages, llm.Message{Role: llm.RoleUser, Blocks: slotsToBlocks(cp.Pending)})
	cp.Pending = nil
	return encodeCheckpoint(cp)
}

// containsSpawn reports whether any call in a turn is a fan-out spawn.
func containsSpawn(calls []llm.ToolUse) bool {
	for _, c := range calls {
		if c.Name == ActionSpawn {
			return true
		}
	}
	return false
}

// childIDs is the set of child goal ids a pending fan-out is waiting on.
func childIDs(pending []resultSlot) []string {
	ids := make([]string, 0, len(pending))
	for _, s := range pending {
		if s.ChildID != "" {
			ids = append(ids, s.ChildID)
		}
	}
	return ids
}

// slotsToBlocks renders result slots as tool_result blocks in order.
func slotsToBlocks(slots []resultSlot) []llm.Block {
	blocks := make([]llm.Block, 0, len(slots))
	for _, s := range slots {
		blocks = append(blocks, llm.Block{Kind: llm.KindToolResult, ToolResult: &llm.ToolResult{ToolUseID: s.ToolUseID, Content: s.Content, IsError: s.IsError}})
	}
	return blocks
}
