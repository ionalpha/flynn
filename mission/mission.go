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

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
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

// TrustedWork is the optional interface a Tool implements to declare how far its work is
// trusted, which sets the containment the waist requires before the tool runs. A tool
// that does not implement it is the agent's own trusted code (it runs at any tier); a
// tool that executes model-authored content, such as a shell command, declares a lower
// trust so the waist refuses it on a host that cannot contain it.
type TrustedWork interface {
	WorkTrust() sandbox.Trust
}

// toolTrust returns the trust level a tool's work carries: the level it declares through
// TrustedWork, or TrustTrusted for a built-in tool that declares none. A missing tool
// (an unknown name) is treated as trusted here; the unknown-tool error is raised when the
// work runs.
func toolTrust(t Tool) sandbox.Trust {
	if tw, ok := t.(TrustedWork); ok {
		return tw.WorkTrust()
	}
	return sandbox.TrustTrusted
}

// Executor drives a goal as a conversation with a model. It implements
// goal.StepExecutor.
type Executor struct {
	model         llm.Model
	tools         map[string]Tool
	defs          []llm.Tool
	system        string
	maxTokens     int
	compactBudget int
	sampling      *llm.Sampling
	reporter      Reporter
	grant         capability.Grant
	hasGrant      bool
	dispatchOpts  []dispatch.Option
	dispatcher    *dispatch.Dispatcher
}

// Option configures an Executor.
type Option func(*Executor)

// WithAdmitter sets the governance gate every tool call is admitted through
// (capability, budget, approval). The default is the capability admitter, which is
// permissive until a grant is bound (see WithGrant), so standalone behaviour is
// unchanged until a policy is supplied; a later WithAdmitter overrides it.
func WithAdmitter(a dispatch.Admitter) Option {
	return func(e *Executor) { e.dispatchOpts = append(e.dispatchOpts, dispatch.WithAdmitter(a)) }
}

// WithGrant binds a capability grant to every step the executor runs, so each tool
// call is admitted only if the grant permits its action. Without a grant the
// default capability admitter is permissive, so the agent runs unconstrained; with
// one the posture is default-deny. The grant is carried on the step's context, so
// it also reaches the sandbox layer below the waist.
func WithGrant(g capability.Grant) Option {
	return func(e *Executor) { e.grant, e.hasGrant = g, true }
}

// WithSandbox wires the run's sandbox into the waist so every action is gated on
// containment sufficiency, alongside the capability grant: a work kind whose trust needs
// stronger isolation than the sandbox provides is refused before it runs, rather than
// downgraded. Without it the containment gate is absent and only the grant governs, which
// keeps the zero-config default permissive.
func WithSandbox(sb sandbox.Sandbox) Option {
	return func(e *Executor) {
		e.dispatchOpts = append(e.dispatchOpts, dispatch.WithHook(capability.NewContainmentGate(sb)))
	}
}

// WithEventSink records every tool call's lifecycle on the event spine (for audit
// and replay). The default discards, so standalone behaviour is unchanged.
func WithEventSink(s dispatch.EventSink) Option {
	return func(e *Executor) { e.dispatchOpts = append(e.dispatchOpts, dispatch.WithEventSink(s)) }
}

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

// WithObserver streams the mission's conversational events (turns, the model's
// text, tool calls and results) to r as the loop runs. The default is a no-op, so
// standalone behaviour is unchanged until an observer is supplied. It is the seam
// the session/stream front door wires a live event stream onto.
func WithObserver(r Reporter) Option {
	return func(e *Executor) {
		if r != nil {
			e.reporter = r
		}
	}
}

// WithMaxTokens caps the output length requested of the model per turn.
func WithMaxTokens(n int) Option {
	return func(e *Executor) {
		if n > 0 {
			e.maxTokens = n
		}
	}
}

// WithCompactionBudget sets the input-token budget above which the oldest middle
// turns are elided from the transcript sent to the model (the objective and the
// recent tail are always kept). Zero, the default, disables compaction, so an
// embedder that wants the full transcript every turn is unaffected. The elision is a
// view over the durable checkpoint, never an overwrite, so nothing is lost. Set this
// to roughly half the model's context window so a long session stays well clear of
// the limit.
func WithCompactionBudget(tokens int) Option {
	return func(e *Executor) {
		if tokens > 0 {
			e.compactBudget = tokens
		}
	}
}

// WithSampling pins the decoding parameters for every model call, so a run can be made
// reproducible. The default is nil, which leaves each call free-running on the server's
// defaults; setting it sends a fixed seed and sampler on every turn.
func WithSampling(s *llm.Sampling) Option {
	return func(e *Executor) { e.sampling = s }
}

// NewExecutor builds a mission executor over the given model and options. Tool
// calls run through a dispatch waist so governance, event recording, and tracing
// are applied once at the chokepoint rather than scattered across the loop.
func NewExecutor(model llm.Model, opts ...Option) *Executor {
	e := &Executor{model: model, tools: map[string]Tool{}, reporter: nopReporter{}}
	// Seed the capability admitter as the base governance gate; it is permissive
	// until a grant is bound, and a caller's WithAdmitter (applied later) overrides
	// it. Seeding first means a bound grant is enforced with zero extra wiring.
	e.dispatchOpts = append(e.dispatchOpts, dispatch.WithAdmitter(capability.Admitter{}))
	for _, o := range opts {
		o(e)
	}
	e.dispatcher = dispatch.New(e.dispatchOpts...)
	return e
}

var _ goal.StepExecutor = (*Executor)(nil)

// ActionModelGenerate is the dispatch action name a model call runs under, so the
// model call is admitted, traced, metered, and recorded on the spine like any tool
// call. It is a normal action, not implicitly allowed: a least-privilege grant must
// list it for the agent to call the model, which keeps the grant the complete record
// of what a run may do. A run that should not call the model omits it.
const ActionModelGenerate = "model.generate"

// Execute advances the goal's conversation by one model turn and returns the
// updated conversation as the checkpoint. A turn that calls tools runs them and
// appends their results so the next step continues; a turn that ends naturally
// marks the conversation done, which Convergence then observes.
func (e *Executor) Execute(ctx context.Context, r resource.Resource) (json.RawMessage, error) {
	// Bind the run's capability grant so the admitter at the waist enforces it and
	// the sandbox layer below reads the same policy from the context.
	if e.hasGrant {
		ctx = capability.Into(ctx, e.grant)
	}
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

	// The turn index is the count of model turns taken so far plus this one, derived
	// from the persisted history so it stays correct across a crash-resumed step.
	turn := assistantTurns(cp.Messages) + 1
	e.reporter.Report(ctx, Event{Kind: EventTurnStarted, Turn: turn})

	// Send a token-lean view of the transcript: older and duplicate large tool
	// outputs are replaced by one-line summaries before the call, while the durable
	// checkpoint (cp.Messages) keeps every result in full. Pruning is deterministic
	// and preserves the message count, so it does not disturb the cacheable prefix.
	// Compaction is the coarse fallback beneath it: if even the pruned transcript
	// would overflow the context budget, the oldest middle turns are elided too. Both
	// are views over the lossless checkpoint, so nothing is overwritten.
	reqMessages := pruneTranscript(cp.Messages, e.summarizerFor)
	reqMessages = compactView(reqMessages, e.compactBudget)

	// The model call goes through the same waist as tool calls: admitted against
	// the run grant, metered for tokens, and bracketed with lifecycle events on the
	// spine. The typed request and response stay here; dispatch sees only the action
	// name, scope, and token cost.
	var resp llm.Response
	err = e.dispatcher.Govern(ctx, dispatch.Action{Name: ActionModelGenerate, Scope: state.Scope(r.Scope), Trust: sandbox.TrustTrusted},
		func(ctx context.Context) (dispatch.Metering, error) {
			var gerr error
			resp, gerr = e.model.Generate(ctx, llm.Request{
				System:    e.system,
				Messages:  reqMessages,
				Tools:     e.defs,
				MaxTokens: e.maxTokens,
				// The conversation only ever grows: the system prompt and tools are
				// fixed, and an earlier turn is never edited. So the whole prefix is
				// stable and worth caching. Declaring that lets a provider reuse the
				// work of reading it back on the next turn instead of reprocessing the
				// entire transcript every call, which is the dominant cost of a long
				// tool-using loop. The run id keys the cache to this conversation, so a
				// provider that routes by cache affinity keeps its turns together. The
				// hint is advisory: a backend without caching ignores it and the result
				// is identical.
				Cache: llm.CacheHint{Prefix: true, StableMessages: len(reqMessages), Key: r.Name},
				// Pin decoding when the run asks for it, so a deterministic run sends the same
				// seed and sampler on every turn; nil leaves the call free-running.
				Sampling: e.sampling,
			})
			return dispatch.Metering{Tokens: resp.Usage.InputTokens + resp.Usage.OutputTokens}, gerr
		})
	if err != nil {
		return nil, err // the model classifies its own errors; the worker retries transient ones
	}
	cp.Messages = append(cp.Messages, resp.Message)

	if text := resp.Message.TextContent(); text != "" {
		e.reporter.Report(ctx, Event{Kind: EventAssistantText, Turn: turn, Text: text})
	}
	for _, tu := range resp.Message.ToolUses() {
		e.reporter.Report(ctx, Event{Kind: EventToolCall, Turn: turn, Tool: tu.Name, ToolUseID: tu.ID, Input: tu.Input})
	}

	switch resp.StopReason {
	case llm.StopToolUse:
		// Run the calls and feed their results back for the next turn.
		cp.Messages = append(cp.Messages, llm.Message{
			Role:   llm.RoleUser,
			Blocks: e.runTools(ctx, state.Scope(r.Scope), turn, resp.Message.ToolUses()),
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

	e.reporter.Report(ctx, Event{Kind: EventTurnCompleted, Turn: turn, StopReason: string(resp.StopReason), Usage: resp.Usage})
	return encodeCheckpoint(cp)
}

// assistantTurns counts the model turns already in a conversation (one assistant
// message per turn), so the next turn's index is one more than this.
func assistantTurns(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == llm.RoleAssistant {
			n++
		}
	}
	return n
}

// summarizerFor returns the one-line result summarizer of a registered tool, or nil
// when the tool is unknown or offers none. Pruning uses it to elide an older large
// result down to a meaningful line rather than a generic size note.
func (e *Executor) summarizerFor(tool string) ResultSummarizer {
	if t, ok := e.tools[tool]; ok {
		if s, ok := t.(ResultSummarizer); ok {
			return s
		}
	}
	return nil
}

// runTools dispatches each requested call through the waist and returns the
// matching tool_result blocks. A rejected, unregistered, or failing call becomes
// an error result rather than failing the step, so the model can recover on the
// next turn. scope is the goal's scope, carried on each action for governance and
// audit.
func (e *Executor) runTools(ctx context.Context, scope state.Scope, turn int, calls []llm.ToolUse) []llm.Block {
	out := make([]llm.Block, 0, len(calls))
	for _, c := range calls {
		res := &llm.ToolResult{ToolUseID: c.ID}
		content, err := e.invokeTool(ctx, scope, c)
		if err != nil {
			res.IsError, res.Content = true, err.Error()
		} else {
			res.Content = content
		}
		e.reporter.Report(ctx, Event{
			Kind: EventToolResult, Turn: turn, Tool: c.Name, ToolUseID: c.ID,
			Result: res.Content, IsError: res.IsError,
		})
		out = append(out, llm.Block{Kind: llm.KindToolResult, ToolResult: res})
	}
	return out
}

// invokeTool governs one tool call through the waist and returns its text output.
// Resolving the tool name and running it is the work the dispatcher brackets; the
// tool's JSON arguments and string result stay here and never reach dispatch. This
// is the single place tool execution happens, so the sandbox isolation boundary
// attaches here.
func (e *Executor) invokeTool(ctx context.Context, scope state.Scope, c llm.ToolUse) (string, error) {
	var content string
	err := e.dispatcher.Govern(ctx, dispatch.Action{Name: c.Name, Scope: scope, Trust: toolTrust(e.tools[c.Name])},
		func(ctx context.Context) (dispatch.Metering, error) {
			tool, ok := e.tools[c.Name]
			if !ok {
				return dispatch.Metering{}, fault.New(fault.Terminal, "unknown_tool", "unknown tool: "+c.Name)
			}
			out, err := tool.Invoke(ctx, c.Input)
			content = out
			return dispatch.Metering{}, err
		})
	return content, err
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

// ContinueConversation reopens a converged goal for another user turn: it appends
// text as a new user message onto the recorded conversation and clears the done
// flag, so re-driving the goal advances the same exchange instead of stopping on
// the prior turn's convergence. The returned status must be persisted onto the goal
// and the goal re-enqueued (runtime.Resume) for the turn to run.
//
// This is the mechanism behind a multi-turn session: each user line after the first
// continues one durable goal, so the model is handed the whole history and the run
// stays addressable, replayable, and auditable by a single id. The phase is reset
// off its settled value so the reconciler re-evaluates rather than no-op-skipping a
// converged goal, and the step counter is cleared so the new turn runs with a fresh
// step budget rather than inheriting the prior turn's spend.
func ContinueConversation(status goal.Status, text string) (goal.Status, error) {
	cp, err := decodeCheckpoint(status.Checkpoint)
	if err != nil {
		return status, fault.Wrap(fault.Terminal, "mission_checkpoint_decode", err)
	}
	cp.Messages = append(cp.Messages, llm.Text(llm.RoleUser, text))
	cp.Done = false
	cp.Result = ""
	raw, err := encodeCheckpoint(cp)
	if err != nil {
		return status, fault.Wrap(fault.Terminal, "mission_checkpoint_encode", err)
	}
	status.Checkpoint = raw
	status.Phase = goal.PhasePending
	status.Message = ""
	status.Steps = 0
	// Drop any record of an in-flight step: the prior turn has ended (converged, or
	// cancelled mid-step), so a fresh turn must dispatch a new step rather than wait
	// on a job that belongs to a runtime that is gone.
	status.InFlight = nil
	return status, nil
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
