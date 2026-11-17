// Package mission drives a goal as a tool-using conversation with a model over the llm
// port.
//
// [Executor] is a goal.StepExecutor that advances one model turn per step, so a crashed
// step resumes from the persisted history. [Convergence] is the matching
// goal.StopEvaluator: the goal is met when the model ends a turn with a final answer. The
// reconciler's step budget bounds the turns, so a conversation that never settles stalls
// instead of looping.
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
	"github.com/ionalpha/flynn/usage"
)

// Tool is a capability the model may call. An Invoke error goes back to the model as a
// failed result so it can adapt; it does not fail the step.
type Tool interface {
	Def() llm.Tool
	Invoke(ctx context.Context, input json.RawMessage) (string, error)
}

// TrustedWork sets the containment the waist requires before a [Tool] runs. A tool without
// it is trusted. A tool that runs model-authored content declares lower trust so a host
// that cannot contain it refuses it.
type TrustedWork interface {
	WorkTrust() sandbox.Trust
}

// toolTrust treats an unknown tool as trusted, because the unknown-tool error is raised
// when the work runs.
func toolTrust(t Tool) sandbox.Trust {
	if tw, ok := t.(TrustedWork); ok {
		return tw.WorkTrust()
	}
	return sandbox.TrustTrusted
}

// Executor is the [goal.StepExecutor] that runs one model turn per step.
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

	// Unset by default, so a NeedsApproval rejection reaches the model unchanged (see
	// [WithApprovalPrompter]).
	prompter       ApprovalPrompter
	approvalSigner approval.Signer
	approvalHost   string
	approvalGrace  time.Duration
	approvalClock  clock.Clock
}

// Option configures an [Executor].
type Option func(*Executor)

// WithAdmitter replaces the default capability admitter, which is permissive until a grant
// is bound (see [WithGrant]).
func WithAdmitter(a dispatch.Admitter) Option {
	return func(e *Executor) { e.dispatchOpts = append(e.dispatchOpts, dispatch.WithAdmitter(a)) }
}

// WithGrant sets the default-deny grant for goals that carry none of their own.
func WithGrant(g capability.Grant) Option {
	return func(e *Executor) { e.grant, e.hasGrant = g, true }
}

// systemFor prefers the goal's own prompt so a delegated child runs as its bound agent.
func systemFor(spec goal.Spec, fallback string) string {
	if spec.System != "" {
		return spec.System
	}
	return fallback
}

// grantFor prefers the goal's own grant so a delegated child runs narrowed. No grant at
// all means permissive.
func (e *Executor) grantFor(spec goal.Spec) (capability.Grant, bool) {
	if len(spec.Grant) > 0 {
		return capability.NewGrant(spec.Grant...), true
	}
	if e.hasGrant {
		return e.grant, true
	}
	return capability.Grant{}, false
}

// WithSandbox refuses, before it runs, any action whose trust needs stronger isolation
// than sb provides. Without it only the grant governs.
func WithSandbox(sb sandbox.Sandbox) Option {
	return func(e *Executor) {
		e.dispatchOpts = append(e.dispatchOpts, dispatch.WithHook(capability.NewContainmentGate(sb)))
	}
}

// WithEventSink records every tool call's lifecycle for audit and replay. The default
// discards.
func WithEventSink(s dispatch.EventSink) Option {
	return func(e *Executor) { e.dispatchOpts = append(e.dispatchOpts, dispatch.WithEventSink(s)) }
}

// WithBrakes halts the run from outside the model loop when a breaker trips or the
// kill-switch is engaged. The caller keeps h to engage the switch out of band. A nil hook
// is ignored.
func WithBrakes(h *brakes.Hook) Option {
	return func(e *Executor) {
		if h != nil {
			e.dispatchOpts = append(e.dispatchOpts, dispatch.WithHook(h))
			e.brakes = true
		}
	}
}

// WithBudget charges every model and tool call to the run's pool and refuses calls past
// the ceiling. A fan-out shares one pool, so one ceiling bounds the graph. A pool with no
// budget resource is unlimited. A nil hook is ignored.
func WithBudget(h *budget.Hook) Option {
	return func(e *Executor) {
		if h != nil {
			e.dispatchOpts = append(e.dispatchOpts, dispatch.WithHook(h))
			e.budgeted = true
		}
	}
}

// WithAllowance refuses an action p marks irreversible unless the goal spec declares it.
// An approval is signed during the run; an allowance is written before it, for a run
// nobody watches. A nil policy is ignored.
func WithAllowance(p allowance.Policy) Option {
	return func(e *Executor) {
		if p != nil {
			e.dispatchOpts = append(e.dispatchOpts, dispatch.WithHook(allowance.NewGate(p)))
		}
	}
}

// WithApproval refuses a listed privileged action unless a quorum of signed approvals is
// on the context. The grant allows the action in principle; the gate authorizes the
// instance. A nil gate is ignored.
func WithApproval(g *approval.Gate) Option {
	return func(e *Executor) {
		if g != nil {
			e.dispatchOpts = append(e.dispatchOpts, dispatch.WithHook(g))
		}
	}
}

// WithTools registers tools. A later tool of the same name wins, so a caller can override
// a default.
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

// WithSystem sets the default system prompt; a goal's own prompt takes precedence.
func WithSystem(system string) Option {
	return func(e *Executor) { e.system = system }
}

// WithObserver streams each [Event] to r. A nil reporter is ignored.
func WithObserver(r Reporter) Option {
	return func(e *Executor) {
		if r != nil {
			e.reporter = r
		}
	}
}

// WithMaxTokens caps output tokens per turn. Non-positive is ignored.
func WithMaxTokens(n int) Option {
	return func(e *Executor) {
		if n > 0 {
			e.maxTokens = n
		}
	}
}

// WithCompactionBudget elides the oldest middle turns from what the model sees once input
// exceeds tokens. The checkpoint keeps everything. Zero disables it. About half the
// context window keeps a long session clear of the limit.
func WithCompactionBudget(tokens int) Option {
	return func(e *Executor) {
		if tokens > 0 {
			e.compactBudget = tokens
		}
	}
}

// WithVerifyPasses answers up to n completion claims with a request to re-check the work.
// Each pass is a normal turn, so the step budget bounds it. Zero trusts the first claim.
func WithVerifyPasses(n int) Option {
	return func(e *Executor) {
		if n > 0 {
			e.verifyPasses = n
		}
	}
}

// WithSimplifiedSchemas shortens tool descriptions and drops schema documentation for a
// weaker model, keeping every property and requirement.
func WithSimplifiedSchemas() Option {
	return func(e *Executor) { e.simplifySchemas = true }
}

// WithSampling pins decoding for reproducibility. Nil uses the server's defaults.
func WithSampling(s *llm.Sampling) Option {
	return func(e *Executor) { e.sampling = s }
}

// WithGenerationRecorder records each call's decoding envelope. The default discards it;
// see [GenerationRecorder].
func WithGenerationRecorder(r GenerationRecorder) Option {
	return func(e *Executor) {
		if r != nil {
			e.recorder = r
		}
	}
}

// NewExecutor routes every model and tool call through one dispatcher so governance,
// recording and tracing apply in one place.
func NewExecutor(model llm.Model, opts ...Option) *Executor {
	e := &Executor{model: model, tools: map[string]Tool{}, reporter: nopReporter{}, recorder: nopGenerationRecorder{}}
	// Seeded first so a bound grant is enforced with no extra wiring and a caller's
	// WithAdmitter overrides it.
	e.dispatchOpts = append(e.dispatchOpts, dispatch.WithAdmitter(capability.Admitter{}))
	for _, o := range opts {
		o(e)
	}
	// Governed by the run's grant like any other action.
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

// Dispatcher is exported for plan-driven fan-out, so a spawn the reconciler decides is
// admitted, metered and recorded like one the model decides.
func (e *Executor) Dispatcher() *dispatch.Dispatcher { return e.dispatcher }

// ActionModelGenerate is the action a model call runs under. A least-privilege grant must
// list it, so the grant stays the complete record of what a run may do.
const ActionModelGenerate = "model.generate"

const verifyPrompt = "Before finishing, re-check that the objective is fully accomplished. " +
	"Inspect your work with the tools rather than assuming. If anything is incomplete or " +
	"incorrect, fix it now. If everything is correct and complete, briefly confirm and stop."

const openItemNudge = "The plan is not finished: the current item is still open. Keep working on it, " +
	"and if you believe its check is wrong, say so in your final answer instead of changing the work to fit it."

// Execute advances one model turn and returns the conversation as the checkpoint. A turn
// that ends without tool calls marks it done for [Convergence].
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
	reopened := false
	if cp.Done {
		// The reconciler builds after a claim only when it held the claim back for an open
		// ledger item, so an open item reopens the conversation.
		if _, open := status.CurrentItem(spec.Ledger); !open {
			return status.Checkpoint, nil // already complete; nothing to advance
		}
		cp.Done, cp.Result = false, ""
		reopened = true
	}
	// A goal waiting on a fan-out folds its children in and makes no model call.
	if len(cp.Pending) > 0 {
		return e.advanceFanout(ctx, r, cp)
	}
	if len(cp.Messages) == 0 {
		cp.Messages = []llm.Message{userTurn(e.prompt(spec), spec.Attachments)}
	}
	briefTurn(&cp, spec, status)
	// A reopened conversation ends on the assistant; with nothing briefed it needs a user
	// turn.
	if reopened && cp.Messages[len(cp.Messages)-1].Role != llm.RoleUser {
		cp.Messages = appendNudge(cp.Messages, openItemNudge)
	}

	turn := cp.Turns + 1
	e.reporter.Report(ctx, Event{Kind: EventTurnStarted, Goal: r.Name, Turn: turn})

	resp, err := e.generate(ctx, r, spec, cp.Messages)
	if err != nil {
		return nil, err // the model classifies its own errors
	}
	cp.Messages = append(cp.Messages, resp.Message)
	cp.Turns++
	e.reportResponse(ctx, r.Name, turn, resp.Message)

	if err := e.settleTurn(ctx, r, turn, &cp, resp); err != nil {
		return nil, err
	}
	e.reporter.Report(ctx, Event{Kind: EventTurnCompleted, Goal: r.Name, Turn: turn, StopReason: string(resp.StopReason), Usage: resp.Usage})
	return encodeCheckpoint(cp)
}

// bindRunContext binds governance per goal, so one executor drives goals of differing
// authority.
func (e *Executor) bindRunContext(ctx context.Context, r resource.Resource, spec goal.Spec) context.Context {
	if g, ok := e.grantFor(spec); ok {
		ctx = capability.Into(ctx, g)
	}
	// Bound whether or not anyone is watching, because an undeclared irreversible action is
	// refused and paused instead of asked about.
	if decls := goal.Declarations(spec.Allowances); len(decls) > 0 {
		ctx = allowance.Into(ctx, decls...)
	}
	// Keyed on the resource name so breakers track each run and the kill-switch halts the
	// right one.
	if e.brakes {
		ctx = brakes.Into(ctx, r.Name)
	}
	if e.budgeted {
		// Descendants inherit the root's pool; a root carries none and is the pool.
		pool := spec.BudgetPool
		if pool == "" {
			pool = r.Name
		}
		ctx = budget.Into(ctx, pool)
		// A goal with no model reads as untiered.
		if spec.Model != "" {
			ctx = budget.TierInto(ctx, spec.Model)
		}
	}
	return ctx
}

// briefTurn renders each brief from the durable record, because a message folded in when
// it arrived may be dropped by the pruner, the compactor or a reseed.
func briefTurn(cp *checkpoint, spec goal.Spec, status goal.Status) {
	// Only at an item boundary, so the transcript gets one copy per item.
	item, ok := status.CurrentItem(spec.Ledger)
	switch {
	case ok && item.ID != cp.Item:
		cp.Messages = appendNudge(cp.Messages, itemBrief(spec, status, item))
		cp.Item = item.ID
	case !ok && cp.Item != "":
		// The one turn the run hears its plan is over and what was left unproven.
		if brief := ledgerClosed(spec, status); brief != "" {
			cp.Messages = appendNudge(cp.Messages, brief)
		}
		cp.Item = ""
	}
	// Every turn the failure survives. The check is the model's own guess, so a wrong one
	// is reported: bending the work to pass it hides what the ledger exists to show.
	if status.ItemFeedback != "" {
		cp.Messages = appendNudge(cp.Messages, "The last run of this item's check did not pass: "+status.ItemFeedback+
			"\nIf the work is right and the check itself is wrong, do not change the work to fit it: say so in your final answer.")
	}
	if brief := goal.SteerBrief(spec, status); brief != "" {
		cp.Messages = appendNudge(cp.Messages, brief)
	}
	if status.ProgressNudge != "" {
		cp.Messages = appendNudge(cp.Messages, status.ProgressNudge)
	}
}

// generate runs the model call through the dispatcher so it is admitted, metered and
// recorded like a tool call; dispatch sees only the name, scope and token cost.
//
// Pruning keeps the message count so the cached prefix is undisturbed, and compaction runs
// only if the pruned view still overflows. Both are views over the checkpoint.
func (e *Executor) generate(ctx context.Context, r resource.Resource, spec goal.Spec, msgs []llm.Message) (llm.Response, error) {
	reqMessages := compactView(pruneTranscript(msgs, e.summarizerFor), e.compactBudget)
	var resp llm.Response
	err := e.governWithApproval(ctx, dispatch.Action{Name: ActionModelGenerate, Scope: state.Scope(r.Scope), Trust: sandbox.TrustTrusted, Goal: r.Name},
		func(ctx context.Context) (dispatch.Metering, error) {
			// Inside the span so the envelope sits beside the lifecycle without typed input
			// reaching dispatch.
			e.recorder.RecordGeneration(ctx, envelopeOf(e.sampling))
			var gerr error
			resp, gerr = e.model.Generate(ctx, llm.Request{
				System:    systemFor(spec, e.system),
				Messages:  reqMessages,
				Tools:     e.defs,
				MaxTokens: e.maxTokens,
				// The conversation only grows, so the whole prefix is cacheable.
				Cache:    llm.CacheHint{Prefix: true, StableMessages: len(reqMessages), Key: r.Name},
				Sampling: e.sampling,
			})
			return dispatch.Metering{Tokens: int(usage.FromLLM(resp.Usage).Total())}, gerr
		})
	return resp, err
}

func (e *Executor) reportResponse(ctx context.Context, goalID string, turn int, msg llm.Message) {
	if text := msg.TextContent(); text != "" {
		e.reporter.Report(ctx, Event{Kind: EventAssistantText, Goal: goalID, Turn: turn, Text: text})
	}
	for _, tu := range msg.ToolUses() {
		e.reporter.Report(ctx, Event{Kind: EventToolCall, Goal: goalID, Turn: turn, Tool: tu.Name, ToolUseID: tu.ID, Input: tu.Input})
	}
}

func (e *Executor) settleTurn(ctx context.Context, r resource.Resource, turn int, cp *checkpoint, resp llm.Response) error {
	switch resp.StopReason {
	case llm.StopToolUse:
		blocks, pending, err := e.dispatchToolUses(ctx, r, turn, resp.Message.ToolUses())
		if err != nil {
			return err
		}
		// A spawning turn waits for its children to fold in.
		if len(pending) > 0 {
			cp.Pending = pending
			return nil
		}
		cp.Messages = append(cp.Messages, llm.Message{Role: llm.RoleUser, Blocks: blocks})
	case llm.StopMaxTokens:
		// The step budget bounds a turn that keeps truncating.
		cp.Messages = append(cp.Messages, llm.Text(llm.RoleUser, "Continue."))
	default:
		// Any other reason is a completion claim.
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

// itemBrief states the item's check because that check is what will run, so the run works
// toward it instead of its own idea of done. The plan is shown so its position is clear.
func itemBrief(spec goal.Spec, status goal.Status, current goal.LedgerItem) string {
	var b strings.Builder
	b.WriteString("Current item: ")
	b.WriteString(current.Item)
	b.WriteString("\nIt is proven when: ")
	b.WriteString(current.Verify)
	b.WriteString("\n\nWork this item, not the whole objective. The plan:\n")
	writePlan(&b, spec, status, current.ID)
	if len(status.Abandoned()) > 0 {
		b.WriteString(abandonedNote)
	}
	return strings.TrimRight(b.String(), "\n")
}

// abandonedNote asks for unproven items in the final answer, where an operator reads them.
const abandonedNote = "Items marked [-] were left unproven because their checks kept failing the same way. " +
	"Say in your final answer which they are and what their checks reported.\n"

// ledgerClosed is "" when every item was proven, because there is nothing to report.
func ledgerClosed(spec goal.Spec, status goal.Status) string {
	abandoned := status.Abandoned()
	if len(abandoned) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Every planned item is settled. The plan:\n")
	writePlan(&b, spec, status, "")
	b.WriteString(abandonedNote)
	for _, st := range abandoned {
		fmt.Fprintf(&b, "- %s: %s\n", itemText(spec, st.ID), st.AbandonedReason)
	}
	b.WriteString("Finish the objective and give your final answer.")
	return b.String()
}

// writePlan marks [x] proven, [-] unproven, [>] current, [ ] not reached.
func writePlan(b *strings.Builder, spec goal.Spec, status goal.Status, current string) {
	marks := make(map[string]string, len(status.Ledger))
	for _, st := range status.Ledger {
		switch {
		case st.Proven:
			marks[st.ID] = "[x]"
		case st.Abandoned:
			marks[st.ID] = "[-]"
		}
	}
	for i, it := range spec.Ledger {
		mark, ok := marks[it.ID]
		switch {
		case ok:
		case it.ID == current:
			mark = "[>]"
		default:
			mark = "[ ]"
		}
		fmt.Fprintf(b, "%s %d. %s\n", mark, i+1, it.Item)
	}
}

// itemText falls back to the id when the ledger lacks the item.
func itemText(spec goal.Spec, id string) string {
	for _, it := range spec.Ledger {
		if it.ID == id {
			return it.Item
		}
	}
	return id
}

// appendNudge joins a trailing user turn when there is one, so roles keep alternating.
func appendNudge(msgs []llm.Message, nudge string) []llm.Message {
	if n := len(msgs); n > 0 && msgs[n-1].Role == llm.RoleUser {
		msgs[n-1].Blocks = append(msgs[n-1].Blocks, llm.Block{Kind: llm.KindText, Text: nudge})
		return msgs
	}
	return append(msgs, llm.Text(llm.RoleUser, nudge))
}

func (e *Executor) summarizerFor(tool string) ResultSummarizer {
	if t, ok := e.tools[tool]; ok {
		if s, ok := t.(ResultSummarizer); ok {
			return s
		}
	}
	return nil
}

// runTools turns a rejected, unknown or failing call into an error result so the model can
// recover next turn.
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

// invokeTool is the only place a tool runs, so sandbox isolation attaches here. Arguments
// and results never reach dispatch.
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

func (e *Executor) prompt(spec goal.Spec) string {
	s := spec.Objective
	if spec.StopCondition != "" {
		s += "\n\nYou are done when: " + spec.StopCondition
	}
	return s
}

// userTurn puts text before images so the model reads the prose before the pictures it
// refers to.
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

// Convergence is the [goal.StopEvaluator] for [Executor]: the model's own decision to stop
// is the signal.
type Convergence struct{}

var _ goal.StopEvaluator = Convergence{}

// Met returns the final text as the reason. It decodes only the outcome fields because it
// runs on every reconcile tick.
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

// ContinueConversation appends a user turn to a converged goal so a session stays one
// replayable goal. The caller persists the status and re-enqueues the goal. Phase and
// step count reset so the turn runs with a fresh budget.
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
	// A new user turn is progress, so a chat of text-only replies does not stall as idle.
	status.IdleStreak = 0
	status.ProgressMark = ""
	status.LastActivity = ""
	status.ProgressNudge = ""
	// The prior turn's runtime is gone.
	status.InFlight = nil
	return status, nil
}

// checkpoint is opaque to the reconciler. Only encodeCheckpoint and decodeCheckpoint touch
// its stored form.
type checkpoint struct {
	Messages []llm.Message
	Done     bool
	Result   string
	// VerifyUsed, Pending, Turns and Item persist so a crash-resume keeps its verify
	// budget, waits instead of re-spawning, and does not re-brief.
	VerifyUsed int
	Pending    []resultSlot
	Turns      int
	Item       string

	// encoded caches each message's JSON so a write marshals only the new turn.
	encoded []json.RawMessage
}

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

type checkpointOutcome struct {
	Done   bool   `json:"done"`
	Result string `json:"result,omitempty"`
}

func decodeCheckpointOutcome(raw json.RawMessage) (checkpointOutcome, error) {
	var cp checkpointOutcome
	if len(raw) == 0 {
		return cp, nil
	}
	err := json.Unmarshal(raw, &cp)
	return cp, err
}

func encodeCheckpoint(cp checkpoint) (json.RawMessage, error) {
	// The reused prefix is byte-identical to a full re-marshal, so replay is unaffected.
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

// Func adapts a function to a [Tool] without declaring a type.
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
