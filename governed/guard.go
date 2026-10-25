package governed

import (
	"context"

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
	waived := make(map[string]any, len(p.Waived))
	for g, reason := range p.Waived {
		waived[string(g)] = reason
	}
	_, err := log.Append(ctx, spine.AppendInput{
		Stream: stream,
		Type:   PostureEventType,
		Actor:  spine.ActorSystem,
		Payload: map[string]any{
			"mode":            string(p.Mode),
			"sandbox":         p.Sandbox,
			"hostSandbox":     p.HostSandbox,
			"containment":     p.Containment,
			"approval":        p.Approval,
			"approvalActions": actions,
			"allowance":       p.Allowance,
			"brakes":          p.Brakes,
			"budget":          p.Budget,
			"budgetTokens":    p.BudgetLimits.Tokens,
			"budgetCost":      p.BudgetLimits.Cost,
			"brakeMaxActions": p.BrakeLimits.MaxActions,
			"brakeWindow":     p.BrakeLimits.Window.String(),
			"halt":            p.Halt,
			"eventSink":       p.EventSink,
			"verifier":        p.Verifier,
			"evidence":        p.Evidence,
			"ledgerProof":     p.LedgerProof,
			"auditor":         p.Auditor,
			"steerJudge":      p.SteerJudge,
			"progress":        p.Progress,
			"refusals":        p.Refusals,
			"waived":          waived,
		},
	})
	return err
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
