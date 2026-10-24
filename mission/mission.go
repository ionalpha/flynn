// Package mission turns a Goal into real work: it drives a goal as a tool-using
// conversation with a language model, through the provider-agnostic llm port.
//
// It supplies the two ports the goal reconciler runs on. Executor is a
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
	"fmt"
	"strings"
	"time"

	"github.com/ionalpha/flynn/allowance"
	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/brakes"
	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/clock"
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
	model           llm.Model
	tools           map[string]Tool
	defs            []llm.Tool
	system          string
	maxTokens       int
	compactBudget   int
	verifyPasses    int
	simplifySchemas bool
	fanout          Fanout
	sampling        *llm.Sampling
	recorder        GenerationRecorder
	reporter        Reporter
	grant           capability.Grant
	hasGrant        bool
	brakes          bool
	budgeted        bool
	dispatchOpts    []dispatch.Option
	dispatcher      *dispatch.Dispatcher

	// prompter, approvalSigner, approvalHost, approvalGrace, and approvalClock resolve a
	// privileged action the waist pauses for approval: the prompter decides, the signer
	// mints the single-use approval, and the clock stamps its window. All are unset by
	// default, so a NeedsApproval rejection surfaces to the model unchanged (see
	// WithApprovalPrompter).
	prompter       ApprovalPrompter
	approvalSigner approval.Signer
	approvalHost   string
	approvalGrace  time.Duration
	approvalClock  clock.Clock
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

// WithGrant binds a default capability grant for goals that do not carry their own
// (see goal.Spec.Grant), so each tool call is admitted only if the grant permits
// its action. Without a grant the default capability admitter is permissive; with
// one the posture is default-deny. A goal that carries its own grant is governed by
// that grant instead.
func WithGrant(g capability.Grant) Option {
	return func(e *Executor) { e.grant, e.hasGrant = g, true }
}

// systemFor resolves the system prompt a goal runs under: the prompt carried on the
// goal itself takes precedence (so a delegated child runs as its bound agent),
// falling back to the executor's default prompt.
func systemFor(spec goal.Spec, fallback string) string {
	if spec.System != "" {
		return spec.System
	}
	return fallback
}

// grantFor resolves the capability grant a goal runs under: the grant carried on
// the goal itself takes precedence (authority travels with the work, so a delegated
// child runs narrowed), falling back to the executor's default grant, and finally
// to no grant (permissive) for an ungoverned standalone run.
func (e *Executor) grantFor(spec goal.Spec) (capability.Grant, bool) {
	if len(spec.Grant) > 0 {
		return capability.NewGrant(spec.Grant...), true
	}
	if e.hasGrant {
		return e.grant, true
	}
	return capability.Grant{}, false
}

// WithSandbox wires the run's sandbox into the waist so every action is gated on
// containment sufficiency, alongside the capability grant: a work kind whose trust needs
// stronger isolation than the sandbox provides is refused before it runs, rather than
// downgraded. Without it only the grant governs.
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

// WithBrakes wires a safety brake into the waist so the run is halted from outside
// the model loop when its observed behaviour trips a breaker or the kill-switch is
// engaged. The brake hook observes every action the run dispatches and refuses
// further work once halted; the caller keeps the hook so it can engage the
// kill-switch (h.Switch()) out of band. Without it no brake is applied. A nil hook
// is ignored.
func WithBrakes(h *brakes.Hook) Option {
	return func(e *Executor) {
		if h != nil {
			e.dispatchOpts = append(e.dispatchOpts, dispatch.WithHook(h))
			e.brakes = true
		}
	}
}

// WithBudget wires a run's spend ceiling into the waist so every model and tool
// call is charged against the run's pool and refused once the ceiling is reached.
// A fan-out shares one pool, so a single ceiling bounds the whole graph rather than
// a budget per goal. Without it no budget is applied, and a run whose pool has no
// budget resource is unlimited. A nil hook is ignored.
func WithBudget(h *budget.Hook) Option {
	return func(e *Executor) {
		if h != nil {
			e.dispatchOpts = append(e.dispatchOpts, dispatch.WithHook(h))
			e.budgeted = true
		}
	}
}

// WithAllowance wires the pre-declaration gate into the waist, so an action the policy
// marks as reaching outside the workspace irreversibly is refused unless the goal declares
// it. It composes above the capability grant the way approval does, and differs from
// approval in when the authority is given: an approval is signed while the run is going,
// and an allowance was written before it started, for a run nobody will be watching.
// The declarations come off the goal spec. Without the gate every action is admitted.
// A nil policy is ignored.
func WithAllowance(p allowance.Policy) Option {
	return func(e *Executor) {
		if p != nil {
			e.dispatchOpts = append(e.dispatchOpts, dispatch.WithHook(allowance.NewGate(p)))
		}
	}
}

// WithApproval wires a cryptographic approval gate into the waist, so a privileged
// action the gate's policy lists is refused unless a sufficient quorum of valid
// signed approvals is presented on the run's context. It composes above the
// capability grant: the grant decides an action is allowed in principle, the gate
// requires a fresh authorization for the specific privileged instance. Without it
// no approval is required, which keeps the standalone agent zero-config. A nil gate
// is ignored.
func WithApproval(g *approval.Gate) Option {
	return func(e *Executor) {
		if g != nil {
			e.dispatchOpts = append(e.dispatchOpts, dispatch.WithHook(g))
		}
	}
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
// text, tool calls and results) to r as the loop runs. The default is a no-op.
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
// recent tail are always kept). Zero, the default, disables compaction. The elision
// is a view over the durable checkpoint, so nothing is lost. Roughly half the model's
// context window keeps a long session clear of the limit.
func WithCompactionBudget(tokens int) Option {
	return func(e *Executor) {
		if tokens > 0 {
			e.compactBudget = tokens
		}
	}
}

// WithVerifyPasses adds up to n self-check passes before a turn is allowed to converge.
// When the model signals it is finished, it is instead asked to re-examine its work against
// the objective and fix anything incomplete; only after the passes are spent (or the model
// keeps the conversation going of its own accord) does the run conclude. Zero, the default,
// trusts the model's first claim of completion. Each pass is a normal turn, so the
// reconciler's step budget bounds it.
func WithVerifyPasses(n int) Option {
	return func(e *Executor) {
		if n > 0 {
			e.verifyPasses = n
		}
	}
}

// WithSimplifiedSchemas trims each tool definition before it is offered to the model: prose
// descriptions are shortened and per-field documentation and examples are dropped from the
// input schema, while the callable surface (every property and which are required) is left
// intact, so a weaker model reasons over a smaller surface without changing what it can
// call. The default leaves the full schemas in place.
func WithSimplifiedSchemas() Option {
	return func(e *Executor) { e.simplifySchemas = true }
}

// WithSampling pins the decoding parameters for every model call, so a run can be made
// reproducible. The default is nil, which leaves each call on the server's defaults.
func WithSampling(s *llm.Sampling) Option {
	return func(e *Executor) { e.sampling = s }
}

// WithGenerationRecorder records the decoding envelope of every model call, so a run's
// reproducibility parameters become part of its durable history. The default discards them:
// nothing here pins sampling, so the envelope is the same zero value on every call. See
// [GenerationRecorder] for what would have to change first.
func WithGenerationRecorder(r GenerationRecorder) Option {
	return func(e *Executor) {
		if r != nil {
			e.recorder = r
		}
	}
}

// NewExecutor builds a mission executor over the given model and options. Tool
// calls run through a dispatch waist so governance, event recording, and tracing
// are applied once at the chokepoint rather than scattered across the loop.
func NewExecutor(model llm.Model, opts ...Option) *Executor {
	e := &Executor{model: model, tools: map[string]Tool{}, reporter: nopReporter{}, recorder: nopGenerationRecorder{}}
	// Seeded first so a bound grant is enforced with no extra wiring and a caller's
	// WithAdmitter overrides it.
	e.dispatchOpts = append(e.dispatchOpts, dispatch.WithAdmitter(capability.Admitter{}))
	for _, o := range opts {
		o(e)
	}
	// A normal tool definition, governed by the run's grant like any other action.
	if e.fanout != nil {
		e.defs = append(e.defs, spawnToolDef)
	}
	if e.simplifySchemas {
		for i := range e.defs {
			e.defs[i] = simplifyTool(e.defs[i])
		}
	}
	e.dispatcher = dispatch.New(e.dispatchOpts...)
	return e
}

var _ goal.StepExecutor = (*Executor)(nil)

// Dispatcher returns the waist this executor governs every action through: the
// admitter, the hooks, the metering and the event sink a composition configured.
//
// It is exported for plan-driven fan-out, where the reconciler rather than a model turn
// decides to spawn. That path has to be admitted, metered and recorded the same way as a
// model-driven spawn, which only the same dispatcher guarantees.
func (e *Executor) Dispatcher() *dispatch.Dispatcher { return e.dispatcher }

// ActionModelGenerate is the dispatch action name a model call runs under, so the
// model call is admitted, traced, metered, and recorded on the spine like any tool
// call. It is not implicitly allowed: a least-privilege grant must list it, which keeps
// the grant the complete record of what a run may do.
const ActionModelGenerate = "model.generate"

// verifyPrompt is the self-check a verify pass injects when the model claims completion
// (see [WithVerifyPasses]).
const verifyPrompt = "Before finishing, re-check that the objective is fully accomplished. " +
	"Inspect your work with the tools rather than assuming. If anything is incomplete or " +
	"incorrect, fix it now. If everything is correct and complete, briefly confirm and stop."

// Execute advances the goal's conversation by one model turn and returns the
// updated conversation as the checkpoint. A turn that calls tools runs them and
// appends their results so the next step continues; a turn that ends naturally
// marks the conversation done, which Convergence then observes.
func (e *Executor) Execute(ctx context.Context, r resource.Resource) (json.RawMessage, error) {
	spec, err := goal.DecodeSpec(r)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "mission_spec_decode", err)
	}
	ctx = e.bindRunContext(ctx, r, spec)
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
	// A goal waiting on a fan-out folds its children in before anything else, and makes no
	// model call on that check.
	if len(cp.Pending) > 0 {
		return e.advanceFanout(ctx, r, cp)
	}
	if len(cp.Messages) == 0 {
		cp.Messages = []llm.Message{userTurn(e.prompt(spec), spec.Attachments)}
	}
	briefTurn(&cp, spec, status)

	// The count is carried on the checkpoint, so the index stays correct across a
	// crash-resumed step without rescanning the history.
	turn := cp.Turns + 1
	e.reporter.Report(ctx, Event{Kind: EventTurnStarted, Goal: r.Name, Turn: turn})

	resp, err := e.generate(ctx, r, spec, cp.Messages)
	if err != nil {
		return nil, err // the model classifies its own errors; the worker retries transient ones
	}
	cp.Messages = append(cp.Messages, resp.Message)
	cp.Turns++ // one assistant message per model turn; keep the carried count in step
	e.reportResponse(ctx, r.Name, turn, resp.Message)

	if err := e.settleTurn(ctx, r, turn, &cp, resp); err != nil {
		return nil, err
	}
	e.reporter.Report(ctx, Event{Kind: EventTurnCompleted, Goal: r.Name, Turn: turn, StopReason: string(resp.StopReason), Usage: resp.Usage})
	return encodeCheckpoint(cp)
}

// bindRunContext binds onto ctx what the waist and the sandbox below it read to govern
// this goal. Each binding is per goal rather than per executor, so one executor drives
// goals of differing authority and a standalone run with nothing to bind is unchanged.
func (e *Executor) bindRunContext(ctx context.Context, r resource.Resource, spec goal.Spec) context.Context {
	if g, ok := e.grantFor(spec); ok {
		ctx = capability.Into(ctx, g)
	}
	// Checked whether or not anyone is watching: a run that reaches an irreversible action
	// it was not given is refused and paused by its reconciler, not asked a question nobody
	// is there to answer.
	if decls := goal.Declarations(spec.Allowances); len(decls) > 0 {
		ctx = allowance.Into(ctx, decls...)
	}
	// The run id is the resource name, the identity the conversation cache and budget key
	// on, so breakers track behaviour per run and the kill-switch halts the right one.
	if e.brakes {
		ctx = brakes.Into(ctx, r.Name)
	}
	if e.budgeted {
		// A fan-out shares one pool: descendants inherit the root's (goal.Spec.BudgetPool),
		// so one ceiling bounds the whole graph. A root carries none and is the pool.
		pool := spec.BudgetPool
		if pool == "" {
			pool = r.Name
		}
		ctx = budget.Into(ctx, pool)
		// A goal with no model defers to the host default and reads as untiered.
		if spec.Model != "" {
			ctx = budget.TierInto(ctx, spec.Model)
		}
	}
	return ctx
}

// briefTurn appends to the transcript what the run must be told this turn. Each item is
// rendered from the durable record, because a message folded into the transcript when it
// arrived is one the pruner, the compactor and a reseed may drop before the turn that
// claims completion.
func briefTurn(cp *checkpoint, spec goal.Spec, status goal.Status) {
	// Only at an item boundary: re-stating the same item each step would copy it into the
	// transcript once per turn for no new information.
	if item, ok := status.CurrentItem(spec.Ledger); ok && item.ID != cp.Item {
		cp.Messages = appendNudge(cp.Messages, itemBrief(spec, status, item))
		cp.Item = item.ID
	}
	// Every turn the failure survives: the item has not moved, and the failing output is
	// the most useful thing the run can be told.
	if status.ItemFeedback != "" {
		cp.Messages = appendNudge(cp.Messages, "The last run of this item's check did not pass: "+status.ItemFeedback)
	}
	// Empty on a turn nobody has redirected.
	if brief := goal.SteerBrief(spec, status); brief != "" {
		cp.Messages = appendNudge(cp.Messages, brief)
	}
	if status.ProgressNudge != "" {
		cp.Messages = appendNudge(cp.Messages, status.ProgressNudge)
	}
}

// generate runs one model call through the same waist as tool calls, so it is admitted
// against the run grant, metered for tokens and bracketed with lifecycle events. The typed
// request and response stay here; dispatch sees only the action name, scope and token cost.
//
// The model is sent a token-lean view of msgs: pruning replaces older and duplicate large
// tool outputs with one-line summaries (preserving the message count, so the cacheable
// prefix is undisturbed), and compaction elides the oldest middle turns if the pruned
// transcript would still overflow. Both are views; the checkpoint keeps every result.
func (e *Executor) generate(ctx context.Context, r resource.Resource, spec goal.Spec, msgs []llm.Message) (llm.Response, error) {
	reqMessages := compactView(pruneTranscript(msgs, e.summarizerFor), e.compactBudget)
	var resp llm.Response
	err := e.governWithApproval(ctx, dispatch.Action{Name: ActionModelGenerate, Scope: state.Scope(r.Scope), Trust: sandbox.TrustTrusted, Goal: r.Name},
		func(ctx context.Context) (dispatch.Metering, error) {
			// Inside the dispatch span, so the decoding identity is kept beside the run's
			// lifecycle without typed model input reaching the payload-agnostic waist.
			e.recorder.RecordGeneration(ctx, envelopeOf(e.sampling))
			var gerr error
			resp, gerr = e.model.Generate(ctx, llm.Request{
				System:    systemFor(spec, e.system),
				Messages:  reqMessages,
				Tools:     e.defs,
				MaxTokens: e.maxTokens,
				// The conversation only grows (system prompt and tools are fixed, an earlier
				// turn is never edited), so the whole prefix is cacheable. The run id keys the
				// cache to this conversation. Advisory: a backend without caching ignores it.
				Cache: llm.CacheHint{Prefix: true, StableMessages: len(reqMessages), Key: r.Name},
				// nil leaves the call free-running.
				Sampling: e.sampling,
			})
			return dispatch.Metering{Tokens: resp.Usage.InputTokens + resp.Usage.OutputTokens}, gerr
		})
	return resp, err
}

// reportResponse reports the model's text and each tool call it asked for.
func (e *Executor) reportResponse(ctx context.Context, goalID string, turn int, msg llm.Message) {
	if text := msg.TextContent(); text != "" {
		e.reporter.Report(ctx, Event{Kind: EventAssistantText, Goal: goalID, Turn: turn, Text: text})
	}
	for _, tu := range msg.ToolUses() {
		e.reporter.Report(ctx, Event{Kind: EventToolCall, Goal: goalID, Turn: turn, Tool: tu.Name, ToolUseID: tu.ID, Input: tu.Input})
	}
}

// settleTurn acts on why the model stopped, extending cp so the next step continues, or
// marking it done.
func (e *Executor) settleTurn(ctx context.Context, r resource.Resource, turn int, cp *checkpoint, resp llm.Response) error {
	switch resp.StopReason {
	case llm.StopToolUse:
		blocks, pending, err := e.dispatchToolUses(ctx, r, turn, resp.Message.ToolUses())
		if err != nil {
			return err
		}
		// A spawn launches a child goal whose result is folded in once it finishes, so a
		// turn that spawns leaves the goal waiting rather than appending results now.
		if len(pending) > 0 {
			cp.Pending = pending
			return nil
		}
		cp.Messages = append(cp.Messages, llm.Message{Role: llm.RoleUser, Blocks: blocks})
	case llm.StopMaxTokens:
		// Cut off, not finished. The reconciler's step budget bounds how long a turn that
		// keeps truncating may run before the goal stalls.
		cp.Messages = append(cp.Messages, llm.Text(llm.RoleUser, "Continue."))
	default:
		// EndTurn or a provider-specific terminal reason: the model claims it is done. The
		// verify count lives on the checkpoint so the budget survives a crash-resume.
		if cp.VerifyUsed < e.verifyPasses {
			cp.VerifyUsed++
			cp.Messages = append(cp.Messages, llm.Text(llm.RoleUser, verifyPrompt))
		} else {
			cp.Done = true
			cp.Result = resp.Message.TextContent()
		}
	}
	return nil
}

// itemBrief renders the run's current ledger item as the immediate objective: which item
// it is, the check it will be held to, its position in the plan, and the plan around it
// with each item's settled state.
//
// The verify clause is stated back deliberately. The item committed to it at planning
// time, it is what will actually be run, and a run told the check up front can work toward
// it rather than toward its own idea of done. The rest of the ledger is shown for context
// and because seeing the proven items above and the unproven ones below is what makes the
// position mean something.
func itemBrief(spec goal.Spec, status goal.Status, current goal.LedgerItem) string {
	proven := make(map[string]bool, len(status.Ledger))
	for _, st := range status.Ledger {
		proven[st.ID] = st.Proven
	}
	var b strings.Builder
	b.WriteString("Current item: ")
	b.WriteString(current.Item)
	b.WriteString("\nIt is proven when: ")
	b.WriteString(current.Verify)
	b.WriteString("\n\nWork this item, not the whole objective. The plan:\n")
	for i, it := range spec.Ledger {
		mark := "[ ]"
		switch {
		case proven[it.ID]:
			mark = "[x]"
		case it.ID == current.ID:
			mark = "[>]"
		}
		fmt.Fprintf(&b, "%s %d. %s\n", mark, i+1, it.Item)
	}
	return strings.TrimRight(b.String(), "\n")
}

// appendNudge folds a stalling warning into the transcript as user-visible text without
// breaking role alternation: it rides on the last message when that is already a user
// turn (the tool results being fed back this step), and otherwise opens its own user
// turn. Either way the model reads it as part of the conversation.
func appendNudge(msgs []llm.Message, nudge string) []llm.Message {
	if n := len(msgs); n > 0 && msgs[n-1].Role == llm.RoleUser {
		msgs[n-1].Blocks = append(msgs[n-1].Blocks, llm.Block{Kind: llm.KindText, Text: nudge})
		return msgs
	}
	return append(msgs, llm.Text(llm.RoleUser, nudge))
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
func (e *Executor) runTools(ctx context.Context, goalID string, scope state.Scope, turn int, calls []llm.ToolUse) []llm.Block {
	out := make([]llm.Block, 0, len(calls))
	for _, c := range calls {
		res := &llm.ToolResult{ToolUseID: c.ID}
		content, err := e.invokeTool(ctx, goalID, scope, c)
		if err != nil {
			res.IsError, res.Content = true, err.Error()
		} else {
			res.Content = content
		}
		e.reporter.Report(ctx, Event{
			Kind: EventToolResult, Goal: goalID, Turn: turn, Tool: c.Name, ToolUseID: c.ID,
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
func (e *Executor) invokeTool(ctx context.Context, goalID string, scope state.Scope, c llm.ToolUse) (string, error) {
	var content string
	err := e.governWithApproval(ctx, dispatch.Action{Name: c.Name, Scope: scope, Trust: toolTrust(e.tools[c.Name]), Goal: goalID},
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

// userTurn builds one user message from prompt text and its attached images.
// A text block leads when the text is non-empty (an image-only turn omits it),
// then one image block per attachment in order, so the model sees the prose
// before the pictures it refers to. The bytes are carried inline in the block,
// matching how the rest of the conversation persists in the checkpoint.
func userTurn(text string, images []llm.Image) llm.Message {
	var blocks []llm.Block
	if text != "" {
		blocks = append(blocks, llm.Block{Kind: llm.KindText, Text: text})
	}
	for i := range images {
		blocks = append(blocks, llm.Block{Kind: llm.KindImage, Image: &images[i]})
	}
	return llm.Message{Role: llm.RoleUser, Blocks: blocks}
}

// Convergence is the goal.StopEvaluator paired with Executor: a mission has
// converged once its conversation reached a final turn. It reads the same
// checkpoint the executor writes, so the model's own decision to stop is the
// convergence signal.
type Convergence struct{}

var _ goal.StopEvaluator = Convergence{}

// Met reports whether the conversation has finished, returning the model's final
// text as the reason. It decodes only the outcome fields, not the whole message
// history: convergence is checked on every reconcile tick, so folding the entire
// (growing) transcript back into memory each time just to read two fields is waste.
func (Convergence) Met(_ context.Context, _ goal.Spec, status goal.Status) (bool, string, error) {
	cp, err := decodeCheckpointOutcome(status.Checkpoint)
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
// Each user line after the first continues one durable goal, so a session stays
// addressable and replayable by a single id. The phase is reset off its settled value so
// the reconciler re-evaluates rather than skipping a converged goal, and the step counter
// is cleared so the new turn gets a fresh step budget.
func ContinueConversation(status goal.Status, text string, images ...llm.Image) (goal.Status, error) {
	cp, err := decodeCheckpoint(status.Checkpoint)
	if err != nil {
		return status, fault.Wrap(fault.Terminal, "mission_checkpoint_decode", err)
	}
	cp.Messages = append(cp.Messages, userTurn(text, images))
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
	// A new user turn is progress by definition. Without the reset, a session of text-only
	// replies (each a real answer, touching no file, tool or ledger item) accumulates an
	// idle streak across turns and false-stalls a healthy chat.
	status.IdleStreak = 0
	status.ProgressMark = ""
	status.LastActivity = ""
	status.ProgressNudge = ""
	// The prior turn has ended (converged, or cancelled mid-step); waiting on its job
	// would wait on a runtime that is gone.
	status.InFlight = nil
	return status, nil
}

// checkpoint is the mission's resumable state: the full conversation, whether the
// model has finished, and its final answer. It is opaque to the reconciler and
// owned by this package; the executor writes it and Convergence reads it. It is not
// marshaled directly: encodeCheckpoint and decodeCheckpoint own the stored form.
type checkpoint struct {
	Messages []llm.Message
	Done     bool
	Result   string
	// VerifyUsed counts the self-check passes taken (see [WithVerifyPasses]); carried so
	// a crash-resume does not reset the budget.
	VerifyUsed int
	// Pending holds the tool-result slots a fan-out turn owes the model while its spawned
	// children run. Non-empty means the next step folds their results in rather than
	// calling the model; carried so a crash mid-fan-out resumes waiting instead of
	// re-spawning.
	Pending []resultSlot
	// Turns counts the model turns taken, one per assistant message, so the turn index
	// survives a crash-resume without rescanning the history.
	Turns int
	// Item is the ledger item id the conversation was last briefed on. A changed id is
	// the moment to brief again; carried so a crash-resumed step does not re-brief.
	Item string

	// encoded holds the marshaled JSON of each message in Messages, in order, and is
	// never itself marshaled onto the wire. decodeCheckpoint fills it from the stored
	// form and encodeCheckpoint reuses it: the conversation is append-only, so a write
	// re-serializes only the turn just appended and copies the historical prefix
	// verbatim instead of marshaling the whole growing history every step.
	encoded []json.RawMessage
}

// checkpointWire is the stored form of a checkpoint. Messages are held as already-
// encoded JSON, so encoding reuses the historical prefix and marshals only the newly
// appended turn, and decoding retains each message's bytes for the next write.
type checkpointWire struct {
	Messages   []json.RawMessage `json:"messages"`
	Done       bool              `json:"done"`
	Result     string            `json:"result,omitempty"`
	VerifyUsed int               `json:"verifyUsed,omitempty"`
	Pending    []resultSlot      `json:"pending,omitempty"`
	Turns      int               `json:"turns,omitempty"`
	Item       string            `json:"item,omitempty"`
}

func decodeCheckpoint(raw json.RawMessage) (checkpoint, error) {
	var cp checkpoint
	if len(raw) == 0 {
		return cp, nil
	}
	var wire checkpointWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return cp, err
	}
	cp.Done, cp.Result, cp.VerifyUsed, cp.Pending, cp.Turns, cp.Item = wire.Done, wire.Result, wire.VerifyUsed, wire.Pending, wire.Turns, wire.Item
	cp.Messages = make([]llm.Message, len(wire.Messages))
	for i := range wire.Messages {
		if err := json.Unmarshal(wire.Messages[i], &cp.Messages[i]); err != nil {
			return cp, err
		}
	}
	cp.encoded = wire.Messages
	return cp, nil
}

// checkpointOutcome is the minimal projection a convergence check needs: whether the
// run finished and its final text. Decoding into it skips materializing the whole
// message history that a full checkpoint decode would build.
type checkpointOutcome struct {
	Done   bool   `json:"done"`
	Result string `json:"result,omitempty"`
}

func decodeCheckpointOutcome(raw json.RawMessage) (checkpointOutcome, error) {
	var cp checkpointOutcome
	if len(raw) == 0 {
		return cp, nil
	}
	return cp, json.Unmarshal(raw, &cp)
}

func encodeCheckpoint(cp checkpoint) (json.RawMessage, error) {
	// Reuse the bytes of every message already encoded on a prior step and marshal
	// only those appended since (an appended message has no cached entry yet). The
	// prefix copy keeps the stored bytes identical to a full re-marshal, so the form
	// round-trips and stays replay-equivalent.
	raws := make([]json.RawMessage, len(cp.Messages))
	copy(raws, cp.encoded)
	for i := range cp.Messages {
		if raws[i] != nil {
			continue
		}
		b, err := json.Marshal(cp.Messages[i])
		if err != nil {
			return nil, err
		}
		raws[i] = b
	}
	return json.Marshal(checkpointWire{
		Messages:   raws,
		Done:       cp.Done,
		Result:     cp.Result,
		VerifyUsed: cp.VerifyUsed,
		Pending:    cp.Pending,
		Turns:      cp.Turns,
		Item:       cp.Item,
	})
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
