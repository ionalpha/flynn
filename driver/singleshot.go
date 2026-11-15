package driver

import (
	"context"
	"encoding/json"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// NameSingleShot is one model turn with no tools, converging on the answer. It suits an
// archetype that answers directly (a classifier, a router, a summarizer).
const NameSingleShot = "single-shot"

// singleShotDriver builds its own executor, which governs its one model call through
// the dispatch waist so the run's grant and brake still apply.
type singleShotDriver struct{}

// Name returns [NameSingleShot].
func (singleShotDriver) Name() string { return NameSingleShot }

// Build ignores Tools and Plan: the loop takes one turn and calls no tool.
func (singleShotDriver) Build(s Spec) (goal.StepExecutor, goal.StopEvaluator, error) {
	opts := []dispatch.Option{dispatch.WithAdmitter(capability.Admitter{})}
	if s.Brakes != nil {
		opts = append(opts, dispatch.WithHook(s.Brakes))
	}
	if s.Budget != nil {
		opts = append(opts, dispatch.WithHook(s.Budget))
	}
	reporter := s.Reporter
	if reporter == nil {
		reporter = nopReporter{}
	}
	exec := &singleShotExec{
		model:      s.Model,
		system:     s.System,
		reporter:   reporter,
		dispatcher: dispatch.New(opts...),
		grant:      s.Grant,
		hasGrant:   s.HasGrant,
		budgeted:   s.Budget != nil,
	}
	return exec, singleShotStop{}, nil
}

var _ Driver = singleShotDriver{}

// singleShotExec is the goal.StepExecutor for the single turn.
type singleShotExec struct {
	model      llm.Model
	system     string
	reporter   mission.Reporter
	dispatcher *dispatch.Dispatcher
	grant      capability.Grant
	hasGrant   bool
	budgeted   bool
}

// Execute runs the turn. A checkpoint already done is returned unchanged, so a re-run
// after a crash is safe.
func (e *singleShotExec) Execute(ctx context.Context, r resource.Resource) (json.RawMessage, error) {
	if e.hasGrant {
		ctx = capability.Into(ctx, e.grant)
	}
	spec, err := goal.DecodeSpec(r)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "singleshot_spec_decode", err)
	}
	// Bind the pool, which a fan-out shares, so one ceiling covers the graph. A root has
	// no pool of its own and is the pool.
	if e.budgeted {
		pool := spec.BudgetPool
		if pool == "" {
			pool = r.Name
		}
		ctx = budget.Into(ctx, pool)
	}
	status, err := goal.DecodeStatus(r)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "singleshot_status_decode", err)
	}
	cp, err := decodeShotCheckpoint(status.Checkpoint)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "singleshot_checkpoint_decode", err)
	}
	if cp.Done {
		return status.Checkpoint, nil
	}

	e.reporter.Report(ctx, mission.Event{Kind: mission.EventTurnStarted, Turn: 1})

	var resp llm.Response
	err = e.dispatcher.Govern(ctx,
		dispatch.Action{Name: mission.ActionModelGenerate, Scope: state.Scope(r.Scope), Trust: sandbox.TrustTrusted},
		func(ctx context.Context) (dispatch.Metering, error) {
			var gerr error
			resp, gerr = e.model.Generate(ctx, llm.Request{
				System:   e.system,
				Messages: []llm.Message{llm.Text(llm.RoleUser, prompt(spec))},
			})
			return dispatch.Metering{Tokens: resp.Usage.InputTokens + resp.Usage.OutputTokens}, gerr
		})
	if err != nil {
		return nil, err
	}

	text := resp.Message.TextContent()
	if text != "" {
		e.reporter.Report(ctx, mission.Event{Kind: mission.EventAssistantText, Turn: 1, Text: text})
	}
	e.reporter.Report(ctx, mission.Event{Kind: mission.EventTurnCompleted, Turn: 1, StopReason: string(resp.StopReason), Usage: resp.Usage})

	cp.Done, cp.Result = true, text
	return encodeShotCheckpoint(cp)
}

// prompt renders the objective and the stop condition, as the definition of done, into
// one user message.
func prompt(spec goal.Spec) string {
	s := spec.Objective
	if spec.StopCondition != "" {
		s += "\n\nYou are done when: " + spec.StopCondition
	}
	return s
}

// singleShotStop is the goal.StopEvaluator that converges once the turn has run.
type singleShotStop struct{}

// Met returns the model's answer as the reason.
func (singleShotStop) Met(_ context.Context, _ goal.Spec, status goal.Status) (bool, string, error) {
	cp, err := decodeShotCheckpoint(status.Checkpoint)
	if err != nil {
		return false, "", fault.Wrap(fault.Terminal, "singleshot_checkpoint_decode", err)
	}
	if !cp.Done {
		return false, "", nil
	}
	reason := cp.Result
	if reason == "" {
		reason = "single-shot turn completed"
	}
	return true, reason, nil
}

// shotCheckpoint is the resumable state: whether the turn ran, and its answer.
type shotCheckpoint struct {
	Done   bool   `json:"done"`
	Result string `json:"result,omitempty"`
}

func decodeShotCheckpoint(raw json.RawMessage) (shotCheckpoint, error) {
	var cp shotCheckpoint
	if len(raw) == 0 {
		return cp, nil
	}
	err := json.Unmarshal(raw, &cp)
	return cp, err
}

func encodeShotCheckpoint(cp shotCheckpoint) (json.RawMessage, error) {
	return json.Marshal(cp)
}

// nopReporter drops events, for a single-shot run with no observer.
type nopReporter struct{}

func (nopReporter) Report(context.Context, mission.Event) {}
