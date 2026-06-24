// Package mission turns a Goal into real work: it drives a goal as a tool-using
// conversation with a language model, through the provider-agnostic llm port.
//
// It supplies the two seams the goal reconciler runs on. Executor is a
// goal.StepExecutor: each step advances the conversation by exactly one model
// turn (call the model, run any tools it asked for, append the results), so
// progress is checkpointed at turn granularity and a crashed step resumes from the
// persisted message history rather than restarting the conversation. Convergence
// is the matching goal.StopEvaluator: the mission is met once the model ends its
// turn with a final answer. The reconciler's step budget bounds how many turns a
// goal may spend, so a conversation that never settles stalls instead of looping
// forever.
//
// Nothing here knows which model is behind the llm.Model port: the same loop runs
// against a hosted API client, an agent-CLI subprocess, or a local model. Tests
// run it against a scripted fake (llm/llmtest).
package mission

import (
	"context"
	"encoding/json"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
)

// Tool is an executable capability the model may call during a mission. Def is the
// declaration handed to the model (name, description, argument schema); Invoke runs
// the call and returns its result as text. An Invoke error is not fatal: the loop
// reports it back to the model as a failed tool result so it can adapt, the same
// way a real tool failure would surface.
type Tool interface {
	Def() llm.Tool
	Invoke(ctx context.Context, input json.RawMessage) (string, error)
}

// Executor drives a goal as a conversation with a model. It implements
// goal.StepExecutor.
type Executor struct {
	model     llm.Model
	tools     map[string]Tool
	defs      []llm.Tool
	system    string
	maxTokens int
}

// Option configures an Executor.
type Option func(*Executor)

// WithTools registers the tools the model may call. Later registrations of the
// same name win, so a caller can override a default tool.
func WithTools(tools ...Tool) Option {
	return func(e *Executor) {
		for _, t := range tools {
			def := t.Def()
			if _, ok := e.tools[def.Name]; !ok {
				e.defs = append(e.defs, def)
			}
			e.tools[def.Name] = t
		}
	}
}

// WithSystem sets the standing system instructions framing every turn.
func WithSystem(system string) Option {
	return func(e *Executor) { e.system = system }
}

// WithMaxTokens caps the output length requested of the model per turn.
func WithMaxTokens(n int) Option {
	return func(e *Executor) {
		if n > 0 {
			e.maxTokens = n
		}
	}
}

// NewExecutor builds a mission executor over the given model and options.
func NewExecutor(model llm.Model, opts ...Option) *Executor {
	e := &Executor{model: model, tools: map[string]Tool{}}
	for _, o := range opts {
		o(e)
	}
	return e
}

var _ goal.StepExecutor = (*Executor)(nil)

// Execute advances the goal's conversation by one model turn and returns the
// updated conversation as the checkpoint. A turn that calls tools runs them and
// appends their results so the next step continues; a turn that ends naturally
// marks the conversation done, which Convergence then observes.
func (e *Executor) Execute(ctx context.Context, r resource.Resource) (json.RawMessage, error) {
	spec, err := goal.DecodeSpec(r)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "mission_spec_decode", err)
	}
	status, err := goal.DecodeStatus(r)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "mission_status_decode", err)
	}
	cp, err := decodeCheckpoint(status.Checkpoint)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "mission_checkpoint_decode", err)
	}
	if cp.Done {
		return status.Checkpoint, nil // already complete; nothing to advance
	}

	if len(cp.Messages) == 0 {
		cp.Messages = []llm.Message{llm.Text(llm.RoleUser, e.prompt(spec))}
	}

	resp, err := e.model.Generate(ctx, llm.Request{
		System:    e.system,
		Messages:  cp.Messages,
		Tools:     e.defs,
		MaxTokens: e.maxTokens,
	})
	if err != nil {
		return nil, err // the model classifies its own errors; the worker retries transient ones
	}
	cp.Messages = append(cp.Messages, resp.Message)

	switch resp.StopReason {
	case llm.StopToolUse:
		// Run the calls and feed their results back for the next turn.
		cp.Messages = append(cp.Messages, llm.Message{
			Role:   llm.RoleUser,
			Blocks: e.runTools(ctx, resp.Message.ToolUses()),
		})
	case llm.StopMaxTokens:
		// The turn was cut off, not finished: ask the model to continue rather than
		// converge on a truncated answer. The reconciler's step budget bounds how
		// long a turn that keeps truncating may run before the goal stalls.
		cp.Messages = append(cp.Messages, llm.Text(llm.RoleUser, "Continue."))
	default:
		// EndTurn (or any provider-specific terminal reason): the model is done.
		cp.Done = true
		cp.Result = resp.Message.TextContent()
	}
	return encodeCheckpoint(cp)
}

// runTools executes each requested call and returns the matching tool_result
// blocks. A call to an unregistered tool, or a tool that errors, becomes an error
// result rather than failing the step, so the model can recover on the next turn.
func (e *Executor) runTools(ctx context.Context, calls []llm.ToolUse) []llm.Block {
	out := make([]llm.Block, 0, len(calls))
	for _, c := range calls {
		res := &llm.ToolResult{ToolUseID: c.ID}
		switch tool, ok := e.tools[c.Name]; {
		case !ok:
			res.IsError, res.Content = true, "unknown tool: "+c.Name
		default:
			content, err := tool.Invoke(ctx, c.Input)
			if err != nil {
				res.IsError, res.Content = true, err.Error()
			} else {
				res.Content = content
			}
		}
		out = append(out, llm.Block{Kind: llm.KindToolResult, ToolResult: res})
	}
	return out
}

// prompt renders the goal into the opening user message: the objective, and the
// stop condition as the explicit definition of done.
func (e *Executor) prompt(spec goal.Spec) string {
	s := spec.Objective
	if spec.StopCondition != "" {
		s += "\n\nYou are done when: " + spec.StopCondition
	}
	return s
}

// Convergence is the goal.StopEvaluator paired with Executor: a mission has
// converged once its conversation reached a final turn. It reads the same
// checkpoint the executor writes, so the model's own decision to stop is the
// convergence signal.
type Convergence struct{}

var _ goal.StopEvaluator = Convergence{}

// Met reports whether the conversation has finished, returning the model's final
// text as the reason.
func (Convergence) Met(_ context.Context, _ goal.Spec, status goal.Status) (bool, string, error) {
	cp, err := decodeCheckpoint(status.Checkpoint)
	if err != nil {
		return false, "", fault.Wrap(fault.Terminal, "mission_checkpoint_decode", err)
	}
	if !cp.Done {
		return false, "", nil
	}
	reason := cp.Result
	if reason == "" {
		reason = "conversation reached a final turn"
	}
	return true, reason, nil
}

// checkpoint is the mission's resumable state: the full conversation, whether the
// model has finished, and its final answer. It is opaque to the reconciler and
// owned by this package; the executor writes it and Convergence reads it.
type checkpoint struct {
	Messages []llm.Message `json:"messages"`
	Done     bool          `json:"done"`
	Result   string        `json:"result,omitempty"`
}

func decodeCheckpoint(raw json.RawMessage) (checkpoint, error) {
	var cp checkpoint
	if len(raw) == 0 {
		return cp, nil
	}
	return cp, json.Unmarshal(raw, &cp)
}

func encodeCheckpoint(cp checkpoint) (json.RawMessage, error) {
	return json.Marshal(cp)
}

// --- tool helpers -----------------------------------------------------------

// Func adapts a plain function to a Tool, so a caller can register a capability
// without declaring a type.
func Func(def llm.Tool, fn func(ctx context.Context, input json.RawMessage) (string, error)) Tool {
	return funcTool{def: def, fn: fn}
}

type funcTool struct {
	def llm.Tool
	fn  func(ctx context.Context, input json.RawMessage) (string, error)
}

func (t funcTool) Def() llm.Tool { return t.def }

func (t funcTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	return t.fn(ctx, input)
}
