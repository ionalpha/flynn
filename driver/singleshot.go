package driver

import (
	"context"
	"encoding/json"

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

// NameSingleShot is a tight responder loop: one model turn, no tools, converge on
// the answer. It is the alternate loop that proves the Driver boundary: a
// genuinely different strategy, selected by name, with no edits to the default
// loop. It suits an archetype that should answer directly rather than work a
// tool-using investigation (a classifier, a router, a summarizer).
const NameSingleShot = "single-shot"

// singleShotDriver builds the single-shot loop. It does not reuse the default
// loop: it is its own executor, which is the point, it independently governs its
// one model call through the dispatch waist (the run's grant and brake still
// apply), demonstrating that a loop composes with governance rather than owning it.
type singleShotDriver struct{}

// Name identifies the single-shot driver.
func (singleShotDriver) Name() string { return NameSingleShot }

// Build assembles the single-shot executor and its convergence test. Tools and the
// scaffolding Plan are intentionally ignored: this loop takes one turn and never
// calls a tool.
func (singleShotDriver) Build(s Spec) (goal.StepExecutor, goal.StopEvaluator, error) {
	opts := []dispatch.Option{dispatch.WithAdmitter(capability.Admitter{})}
	if s.Brakes != nil {
		opts = append(opts, dispatch.WithHook(s.Brakes))
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
	}
	return exec, singleShotStop{}, nil
}

var _ Driver = singleShotDriver{}

// singleShotExec advances a goal with exactly one model turn and converges. It
// implements goal.StepExecutor.
type singleShotExec struct {
	model      llm.Model
	system     string
	reporter   mission.Reporter
	dispatcher *dispatch.Dispatcher
	grant      capability.Grant
	hasGrant   bool
}

// Execute runs the single turn. A resumed step whose checkpoint is already done
// returns it unchanged, so the step is safe to re-run after a crash.
func (e *singleShotExec) Execute(ctx context.Context, r resource.Resource) (json.RawMessage, error) {
	if e.hasGrant {
		ctx = capability.Into(ctx, e.grant)
	}
	spec, err := goal.DecodeSpec(r)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "singleshot_spec_decode", err)
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

// prompt renders the goal into the single user message: the objective, and the
// stop condition as the explicit definition of done.
func prompt(spec goal.Spec) string {
	s := spec.Objective
	if spec.StopCondition != "" {
		s += "\n\nYou are done when: " + spec.StopCondition
	}
	return s
}

// singleShotStop converges as soon as the single turn has run. It implements
// goal.StopEvaluator.
type singleShotStop struct{}

// Met reports whether the turn has completed, returning the model's answer as the
// reason.
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

// shotCheckpoint is the single-shot loop's resumable state: whether the turn has
// run and the answer it produced.
type shotCheckpoint struct {
	Done   bool   `json:"done"`
	Result string `json:"result,omitempty"`
}

func decodeShotCheckpoint(raw json.RawMessage) (shotCheckpoint, error) {
	var cp shotCheckpoint
	if len(raw) == 0 {
		return cp, nil
	}
	return cp, json.Unmarshal(raw, &cp)
}

func encodeShotCheckpoint(cp shotCheckpoint) (json.RawMessage, error) {
	return json.Marshal(cp)
}

// nopReporter drops events, for a single-shot run with no observer.
type nopReporter struct{}

func (nopReporter) Report(context.Context, mission.Event) {}
