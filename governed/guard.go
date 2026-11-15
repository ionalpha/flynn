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

// Guard names a default governance class, removable only through [Waive].
//
//flynn:policy
type Guard string

const (
	// GuardSandbox includes the containment gate, item verifier and invariant auditor.
	GuardSandbox Guard = "sandbox"
	// GuardApproval is the human approval gate.
	GuardApproval Guard = "approval"
	// GuardBrakes is the rate breaker and kill-switch.
	GuardBrakes Guard = "brakes"
	// GuardBudget is the spend ceiling.
	GuardBudget Guard = "budget"
	// GuardAudit covers the auditor, steer judge and the progress and refusal probes.
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

// WaivedEventType records a waiver's guard and reason. Its absence from a record shows
// the run carried every guard.
const WaivedEventType = "governance.waived"

// recordWaivers fails assembly on a failed write, so a weaker run cannot hide it.
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

// PostureEventType records which guards are in force, so a sealed record states its
// posture without the host's word. It precedes the first action and the session's opening
// event, so a reader seeking that event skips governance events first. Each assembly,
// including a resume, writes its own.
const PostureEventType = "governance.posture"

// recordPosture fails assembly on a failed write, as [recordWaivers] does.
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

// ReadPosture returns the first posture on stream, or false if none, so a resume rewires
// the original guards. It reads the first, because a resume that lost a guard would pass
// the loss on.
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

// decodePosture goes through JSON, because a durable log returns JSON types (a number
// is a float64).
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

// Mode is the kind of loop a run drives.
type Mode string

// The run modes.
const (
	ModeConversation Mode = "conversation"
	ModeFanout       Mode = "fanout"
	ModeToolset      Mode = "toolset"
	ModeExternal     Mode = "external"
)

// Posture reports what [Assemble] built, so a host can assert it before starting. A
// field is false when the guard was waived (named in Waived) or the mode lacks it: a
// fan-out has no progress or refusal probe, a toolset run no sandbox-bound judges, an
// external run no auditor or steer judge.
type Posture struct {
	Mode Mode

	Sandbox     bool
	HostSandbox bool
	Containment bool

	Approval        bool
	ApprovalActions []string
	Allowance       bool
	// AllowanceActions is sorted.
	AllowanceActions []string

	Brakes bool
	Budget bool
	Halt   bool

	// BudgetLimits is zero when no ceiling was given, leaving the run unlimited.
	BudgetLimits budgetpkg.Limits
	// BrakeLimits is zero when waived.
	BrakeLimits brakes.Limits

	EventSink   bool
	Verifier    bool
	Evidence    bool
	LedgerProof bool
	Auditor     bool
	SteerJudge  bool
	Progress    bool
	Refusals    bool

	Waived map[Guard]string
}
