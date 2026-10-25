package governed

import (
	"context"

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

// Mode is the shape of the run's loop.
type Mode string

// The run modes.
const (
	ModeConversation Mode = "conversation"
	ModeFanout       Mode = "fanout"
	ModeToolset      Mode = "toolset"
	ModeExternal     Mode = "external"
)

// Posture reports what [Assemble] wired, computed from the wiring and not from the
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
