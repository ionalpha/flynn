package governed

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ionalpha/flynn/brakes"
	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/spine"
)

// Guard names a class of governance a run carries by default. Each can be waived, and
// only through [Waive].
type Guard string

const (
	// GuardSandbox is the confined sandbox and everything bound to it: the containment
	// gate, the item verifier and the invariant auditor.
	GuardSandbox Guard = "sandbox"
	// GuardApproval is the human-approval gate.
	GuardApproval Guard = "approval"
	// GuardBrakes is the rate breaker and kill-switch that halt a runaway.
	GuardBrakes Guard = "brakes"
	// GuardBudget is the spend charge applied to every action.
	GuardBudget Guard = "budget"
	// GuardAudit is the run-level judging: invariant auditor, steer judge, and the
	// progress and refusal probes.
	GuardAudit Guard = "audit"
)

var allGuards = []Guard{GuardSandbox, GuardApproval, GuardBrakes, GuardBudget, GuardAudit}

func (g Guard) valid() bool {
	for _, k := range allGuards {
		if g == k {
			return true
		}
	}
	return false
}

// Waiver is one recorded opt-out.
type Waiver struct {
	Guard  Guard
	Reason string
}

// WaivedEventType is the spine event type a waiver is recorded under, on the run's own
// stream, with the guard and the reason in its payload. Its absence from a record is
// what lets a reader say the run carried every guard.
const WaivedEventType = "governance.waived"

// recordWaivers writes each waiver onto stream before the run starts. A failed write
// fails assembly: a run that is weaker than the binary's must not be able to hide it.
func recordWaivers(ctx context.Context, log spine.Log, stream string, ws []Waiver) error {
	for _, w := range ws {
		if _, err := log.Append(ctx, spine.AppendInput{
			Stream:  stream,
			Type:    WaivedEventType,
			Actor:   spine.ActorSystem,
			Payload: map[string]any{"guard": string(w.Guard), "reason": w.Reason},
		}); err != nil {
			return err
		}
	}
	return nil
}

// PostureEventType is the spine event type under which [Assemble] states, on the run's own
// stream, which guards the run was wired with. [WaivedEventType] says what was given up and
// why; this says what is in force, so a reader of a sealed record learns the posture from
// the record and not from the host's account of it. It is written before [Assemble]
// returns, so it precedes the run's first action however the goal is started, and so it
// precedes the session's own opening event: a reader looking for a run's first session
// event skips the governance events ahead of it. A resumed run states it again, because
// each assembly is its own claim.
const PostureEventType = "governance.posture"

// recordPosture writes p onto stream. A failed write fails assembly for the same reason a
// failed waiver does: a run weaker than the binary's must not be able to hide it.
func recordPosture(ctx context.Context, log spine.Log, stream string, p Posture) error {
	actions := make([]any, 0, len(p.ApprovalActions))
	for _, a := range p.ApprovalActions {
		actions = append(actions, a)
	}
	outside := make([]any, 0, len(p.AllowanceActions))
	for _, a := range p.AllowanceActions {
		outside = append(outside, a)
	}
	waived := make(map[string]any, len(p.Waived))
	for g, reason := range p.Waived {
		waived[string(g)] = reason
	}
	_, err := log.Append(ctx, spine.AppendInput{
		Stream: stream,
		Type:   PostureEventType,
		Actor:  spine.ActorSystem,
		Payload: map[string]any{
			"mode":             string(p.Mode),
			"sandbox":          p.Sandbox,
			"hostSandbox":      p.HostSandbox,
			"containment":      p.Containment,
			"approval":         p.Approval,
			"approvalActions":  actions,
			"allowance":        p.Allowance,
			"allowanceActions": outside,
			"brakes":           p.Brakes,
			"budget":           p.Budget,
			"budgetTokens":     p.BudgetLimits.Tokens,
			"budgetCost":       p.BudgetLimits.Cost,
			"brakeMaxActions":  p.BrakeLimits.MaxActions,
			"brakeWindow":      p.BrakeLimits.Window.String(),
			"halt":             p.Halt,
			"eventSink":        p.EventSink,
			"verifier":         p.Verifier,
			"evidence":         p.Evidence,
			"ledgerProof":      p.LedgerProof,
			"auditor":          p.Auditor,
			"steerJudge":       p.SteerJudge,
			"progress":         p.Progress,
			"refusals":         p.Refusals,
			"waived":           waived,
		},
	})
	return err
}

// ReadPosture returns the posture the first assembly of stream recorded, and false when
// the stream has none (a run recorded before postures were). A resume reads it to wire
// the guards the run was started with, so continuing a run cannot drop its spend cap,
// its approvals, its outside-action gate or its item verifier. It reads the first and
// not the latest because a resume records its own posture, and one that had lost a
// guard would otherwise pass the loss to the next resume.
func ReadPosture(ctx context.Context, log spine.Log, stream string) (Posture, bool, error) {
	const page = 64
	for after := int64(0); ; {
		events, err := log.Read(ctx, spine.Query{Stream: stream, AfterSeq: after, Limit: page})
		if err != nil {
			return Posture{}, false, err
		}
		for _, e := range events {
			if e.Type == PostureEventType {
				p, err := decodePosture(e.Payload)
				return p, err == nil, err
			}
		}
		if len(events) < page {
			return Posture{}, false, nil
		}
		after = events[len(events)-1].Seq
	}
}

// decodePosture is the inverse of the payload [recordPosture] writes. It goes through
// JSON because a payload read back from a durable log holds JSON's types (a number is a
// float64), not the ones it was appended with.
func decodePosture(payload map[string]any) (Posture, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Posture{}, fmt.Errorf("governed: posture payload: %w", err)
	}
	var w struct {
		Mode             string            `json:"mode"`
		Sandbox          bool              `json:"sandbox"`
		HostSandbox      bool              `json:"hostSandbox"`
		Containment      bool              `json:"containment"`
		Approval         bool              `json:"approval"`
		ApprovalActions  []string          `json:"approvalActions"`
		Allowance        bool              `json:"allowance"`
		AllowanceActions []string          `json:"allowanceActions"`
		Brakes           bool              `json:"brakes"`
		Budget           bool              `json:"budget"`
		BudgetTokens     int64             `json:"budgetTokens"`
		BudgetCost       float64           `json:"budgetCost"`
		BrakeMaxActions  int               `json:"brakeMaxActions"`
		BrakeWindow      string            `json:"brakeWindow"`
		Halt             bool              `json:"halt"`
		EventSink        bool              `json:"eventSink"`
		Verifier         bool              `json:"verifier"`
		Evidence         bool              `json:"evidence"`
		LedgerProof      bool              `json:"ledgerProof"`
		Auditor          bool              `json:"auditor"`
		SteerJudge       bool              `json:"steerJudge"`
		Progress         bool              `json:"progress"`
		Refusals         bool              `json:"refusals"`
		Waived           map[string]string `json:"waived"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return Posture{}, fmt.Errorf("governed: posture payload: %w", err)
	}
	window, err := time.ParseDuration(w.BrakeWindow)
	if err != nil && w.BrakeWindow != "" {
		return Posture{}, fmt.Errorf("governed: posture brake window: %w", err)
	}
	p := Posture{
		Mode: Mode(w.Mode), Sandbox: w.Sandbox, HostSandbox: w.HostSandbox, Containment: w.Containment,
		Approval: w.Approval, ApprovalActions: w.ApprovalActions,
		Allowance: w.Allowance, AllowanceActions: w.AllowanceActions,
		Brakes: w.Brakes, Budget: w.Budget, Halt: w.Halt,
		BudgetLimits: budgetpkg.Limits{Tokens: w.BudgetTokens, Cost: w.BudgetCost},
		BrakeLimits:  brakes.Limits{MaxActions: w.BrakeMaxActions, Window: window},
		EventSink:    w.EventSink, Verifier: w.Verifier, Evidence: w.Evidence, LedgerProof: w.LedgerProof,
		Auditor: w.Auditor, SteerJudge: w.SteerJudge, Progress: w.Progress, Refusals: w.Refusals,
		Waived: make(map[Guard]string, len(w.Waived)),
	}
	for g, reason := range w.Waived {
		p.Waived[Guard(g)] = reason
	}
	// recordPosture writes an empty list for none; the run reports none as nil.
	if len(p.ApprovalActions) == 0 {
		p.ApprovalActions = nil
	}
	if len(p.AllowanceActions) == 0 {
		p.AllowanceActions = nil
	}
	return p, nil
}

// Mode is the shape of the run's loop.
type Mode string

// The run modes.
const (
	ModeConversation Mode = "conversation"
	ModeFanout       Mode = "fanout"
	ModeToolset      Mode = "toolset"
	ModeExternal     Mode = "external"
)

// Posture reports what [Assemble] wired, computed from what was built and not from the
// options asked for, so a host can assert it before it starts the run. A field is false
// when that guard is not on this run's path, whether because it was waived or because
// the mode does not have it; Waived names the first cause.
//
// The per-mode differences are the binary's, reported as they are: a fan-out has no
// progress or refusal probe, a toolset run has no sandbox-bound judges, an external run
// has no auditor or steer judge.
type Posture struct {
	Mode Mode

	Sandbox     bool
	HostSandbox bool
	Containment bool

	Approval        bool
	ApprovalActions []string
	Allowance       bool
	// AllowanceActions is the actions marked as reaching outside the workspace, sorted:
	// the ones [Posture.Allowance] gates.
	AllowanceActions []string

	Brakes bool
	Budget bool
	Halt   bool

	// BudgetLimits is the spend ceiling opened for the run; zero when the budget hook is
	// wired and no ceiling was given, which leaves the run unmetered against any pool.
	BudgetLimits budgetpkg.Limits
	// BrakeLimits is the limits the run's brakes were built with, zero when waived.
	BrakeLimits brakes.Limits

	EventSink   bool
	Verifier    bool
	Evidence    bool
	LedgerProof bool
	Auditor     bool
	SteerJudge  bool
	Progress    bool
	Refusals    bool

	// Waived maps each waived guard to its reason.
	Waived map[Guard]string
}
