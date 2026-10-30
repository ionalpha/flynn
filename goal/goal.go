// Package goal is the agent's first desired-state kind: a Goal declares an
// objective and a stop condition (the desired state), and a reconciler drives it
// toward that condition by dispatching work steps and observing progress (the
// observed state). It is the agent's own execution model expressed on the generic
// resource + reconcile foundation: declarative, level-triggered, crash-resumable,
// and budget-bounded, rather than an imperative do-step-do-step loop that loses
// the thread on failure.
package goal

import (
	"encoding/json"
	"time"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the Goal kind's API group and version.
	GroupVersion = "goal.ionagent.io/v1alpha1"
	// Kind is the resource kind name.
	Kind = "Goal"
	// Finalizer is the key the reconciler adds to a goal so it gets a chance to
	// clean up owned work (child goals, runs, worktrees) before the goal is removed.
	Finalizer = "goal.ionagent.io/cleanup"
	// StepJobKind is the job kind a dispatched goal step is enqueued under.
	StepJobKind = "goal.step"
	// PlanJobKind is the job kind a goal's planning step is enqueued under. It shares the
	// build step's queue and worker, so planning inherits its lease, crash-resume and
	// retry ladder.
	PlanJobKind = "goal.plan"
	// VerifyJobKind is the job kind a goal's item-verification step is enqueued under,
	// sharing the build step's queue and worker like planning.
	VerifyJobKind = "goal.verify"
)

// Phase is a coarse, human-facing lifecycle summary of a goal, derived from its
// conditions (a convenience projection, not the source of truth).
type Phase string

// The phases a goal moves through, from accepted to a terminal converged or
// stalled state.
const (
	PhasePending   Phase = "Pending"   // accepted, no step run yet
	PhasePlanning  Phase = "Planning"  // expanding the objective into a ledger, before any building
	PhaseRunning   Phase = "Running"   // a step is in flight or more are queued
	PhaseConverged Phase = "Converged" // the stop condition is satisfied
	PhaseStalled   Phase = "Stalled"   // out of budget or a step failed terminally
)

// Standard condition types (kstatus-style: abnormal conditions are present and
// True only when something noteworthy holds).
const (
	CondReady       = "Ready"       // True once the goal has converged
	CondReconciling = "Reconciling" // True while the controller is actively working
	CondStalled     = "Stalled"     // True when progress has stopped abnormally
)

var specSchema = json.RawMessage(`{
  "type": "object",
  "required": ["objective", "stopCondition"],
  "properties": {
    "objective": {"type": "string", "minLength": 1},
    "stopCondition": {"type": "string", "minLength": 1},
    "maxSteps": {"type": "integer", "minimum": 0},
    "grant": {"type": "array", "items": {"type": "string"}},
    "depth": {"type": "integer", "minimum": 0},
    "budgetPool": {"type": "string"},
    "budget": {
      "type": "object",
      "properties": {
        "tokens": {"type": "integer", "minimum": 0},
        "cost": {"type": "number", "minimum": 0},
        "windowFraction": {"type": "number", "minimum": 0, "maximum": 1}
      },
      "additionalProperties": false
    },
    "ledger": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["id", "item", "verify"],
        "properties": {
          "id": {"type": "string", "minLength": 1},
          "item": {"type": "string", "minLength": 1},
          "verify": {"type": "string", "minLength": 1}
        },
        "additionalProperties": false
      }
    },
    "units": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["id", "objective", "verify"],
        "properties": {
          "id": {"type": "string", "minLength": 1},
          "objective": {"type": "string", "minLength": 1},
          "verify": {"type": "string", "minLength": 1},
          "dependsOn": {"type": "array", "items": {"type": "string", "minLength": 1}},
          "actions": {"type": "array", "items": {"type": "string"}},
          "agent": {"type": "string"}
        },
        "additionalProperties": false
      }
    },
    "invariants": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["id", "statement"],
        "properties": {
          "id": {"type": "string", "minLength": 1},
          "statement": {"type": "string", "minLength": 1},
          "check": {"type": "string"}
        },
        "additionalProperties": false
      }
    },
    "steers": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["id", "instruction"],
        "properties": {
          "id": {"type": "string", "minLength": 1},
          "instruction": {"type": "string", "minLength": 1}
        },
        "additionalProperties": false
      }
    },
    "kill": {
      "type": "object",
      "properties": {
        "reason": {"type": "string"}
      },
      "additionalProperties": false
    },
    "allowances": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["action"],
        "properties": {
          "action": {"type": "string", "minLength": 1},
          "target": {"type": "string"}
        },
        "additionalProperties": false
      }
    },
    "system": {"type": "string"},
    "driver": {"type": "string"},
    "model": {"type": "string"},
    "attachments": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["mediaType", "data"],
        "properties": {
          "mediaType": {"type": "string", "minLength": 1},
          "data": {"type": "string"}
        },
        "additionalProperties": false
      }
    }
  },
  "additionalProperties": false
}`)

// Spec is a goal's desired state: what to achieve, the condition that means it is
// done, an optional ceiling on how many steps may be spent trying, and the
// capabilities the goal is authorized to use.
type Spec struct {
	Objective     string `json:"objective"`
	StopCondition string `json:"stopCondition"`
	MaxSteps      int    `json:"maxSteps,omitempty"`
	// Grant is the exact set of action names this goal may take. It travels with the
	// goal, so one executor can drive goals of differing authority, and a child's grant
	// is a subset of its parent's: a delegation never widens authority. Empty defers to
	// the executor's default grant.
	Grant []string `json:"grant,omitempty"`
	// Depth is how many delegation hops separate this goal from a root goal (0). A
	// fan-out spawner refuses a child past a maximum depth, so agents spawning agents
	// cannot recurse without bound.
	Depth int `json:"depth,omitempty"`
	// BudgetPool is the run id whose budget this goal charges and reserves against. Every
	// descendant inherits the root's pool, so a fan-out is bounded by one ceiling. Empty
	// means the goal is its own pool.
	BudgetPool string `json:"budgetPool,omitempty"`
	// Budget bounds the goal by what it spends, beside MaxSteps' bound on how many steps
	// it takes: a ceiling on tokens, cost and share of the plan window, checked at the
	// same reconcile point as MaxSteps against the spend recorded on BudgetPool. A step
	// is the wrong unit for cost, which varies by an order of magnitude between steps.
	// The zero value bounds nothing.
	Budget SpendBudget `json:"budget,omitempty"`
	// System is the standing system prompt this goal runs under, so a delegated child can
	// run as a different agent than its parent. Empty defers to the executor's default.
	System string `json:"system,omitempty"`
	// Driver names the run loop this goal uses (resolved from the driver registry), and
	// Model the model it runs on, so a delegated child can differ from its parent. Empty
	// defers to the host default.
	Driver string `json:"driver,omitempty"`
	Model  string `json:"model,omitempty"`
	// Attachments are images seeded onto the goal's opening user turn, whose text is the
	// objective. They are the model port's own image type (bytes inline, like the rest
	// of a conversation in the checkpoint), so no translation is needed.
	Attachments []llm.Image `json:"attachments,omitempty"`
	// Ledger is the objective expanded into the units of work it implies, each with its
	// own declared way to verify it. It is desired state, so it lives here and not in
	// status; the per-item observation is Status.Ledger. The planner writes it before any
	// building starts and it is append-and-mark-only from then on (see ledger.go).
	Ledger []LedgerItem `json:"ledger,omitempty"`
	// Units is the goal's fan-out written down in advance: a graph of child units with
	// dependency edges, admitted in dependency order as governed children (see units.go).
	// It is the plan-driven counterpart to the model deciding to spawn mid-conversation.
	// A malformed graph is refused at admission, before a child is created.
	Units []Unit `json:"units,omitempty"`
	// Invariants are the terms of the run: what must stay true while the goal works (see
	// invariant.go). An auditor rules on them before the stop evaluator is asked anything,
	// a breach settles the goal whatever the evaluator was about to say, and an adopted
	// term cannot be dropped or reworded.
	Invariants []Invariant `json:"invariants,omitempty"`
	// Steers are the redirects an operator has issued since the run started, with the
	// objective and stop condition left alone (see steer.go). They are the one desired
	// state written while the work is in flight, and the run is refused its completion
	// until it has said what it did about each.
	Steers []Steer `json:"steers,omitempty"`
	// Kill is an operator's order to stop this run (see kill.go). Unlike the rest of the
	// spec it asks for less: a reconcile that reads it engages the run's halt, so the stop
	// reaches the waist inside the running step, and settles the goal as killed.
	Kill *Kill `json:"kill,omitempty"`
	// Allowances are the irreversible actions outside the workspace this run is
	// authorized to take (see allowance.go). A run that reaches an undeclared one is
	// paused with the ask, not refused into a model that would look for another route.
	// With none declared the run can take no such action.
	Allowances []Allowance `json:"allowances,omitempty"`
}

// SpendBudget is a goal's spend ceiling on three axes. Tokens and Cost cap the total
// the goal may spend on its budget pool; WindowFraction caps the share of the plan
// window the run may consume, in [0,1]. A zero field is no bound on that axis.
//
// Window share suits a subscription's plan window, where the scarce resource is not
// money. Flynn enforces the fraction but does not source the window data; an app
// supplies that through the reconciler's WindowSource, and the bound has no effect
// until one is wired.
//
// Crossing a ceiling stops the goal with a named reason. That is deliberately not the
// code path of a provider's own limit (a pause that resumes on reset) or a transient
// error (retried with backoff): see the reconciler's spendGuard.
type SpendBudget struct {
	Tokens         int64   `json:"tokens,omitempty"`
	Cost           float64 `json:"cost,omitempty"`
	WindowFraction float64 `json:"windowFraction,omitempty"`
}

// IsZero reports whether the budget bounds nothing on any axis, so the reconciler can
// skip the spend guard entirely for a goal that sets no ceiling.
func (b SpendBudget) IsZero() bool {
	return b.Tokens == 0 && b.Cost == 0 && b.WindowFraction == 0
}

// InFlight records a dispatched step not yet observed complete, so a re-reconcile
// observes the running work instead of launching a duplicate.
type InFlight struct {
	JobID     string    `json:"jobID"`
	StartedAt time.Time `json:"startedAt"`
	// Kind is the job kind dispatched (StepJobKind or PlanJobKind), so the observing pass
	// can tell a planning step, which spends no build budget, from a build step when the
	// reservation is cleared. Empty reads as a build step.
	Kind string `json:"kind,omitempty"`
}

// Status is a goal's observed state.
type Status struct {
	Phase Phase `json:"phase,omitempty"`
	// ObservedSpecHash is the resource.SpecHash the reconciler last acted on, so a
	// reconcile is a no-op while the spec is unchanged and the goal has settled.
	ObservedSpecHash string      `json:"observedSpecHash,omitempty"`
	Steps            int         `json:"steps,omitempty"`
	InFlight         *InFlight   `json:"inFlight,omitempty"`
	Conditions       []Condition `json:"conditions,omitempty"`
	Message          string      `json:"message,omitempty"`
	// Checkpoint is opaque progress a worker persists mid-step so a step that
	// crashes resumes from here instead of restarting. It is owned by the step
	// executor; the reconciler never interprets it.
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
	// WaitingSince marks the goal as parked: its last step reported ErrWaiting (no
	// progress, waiting on external state such as a fan-out's running children),
	// stamped with when the worker recorded it. While set, the reconciler does not
	// dispatch a step, evaluate the stop condition, or count the wait against the
	// step budget. A settling child clears it (the prompt wake); the reconciler's
	// recheck fallback clears it after a bounded delay if that wake is lost.
	WaitingSince *time.Time `json:"waitingSince,omitempty"`
	// Planned records that the planning phase has run to completion. It is separate
	// from a non-empty Ledger because the two answer different questions: a planner
	// that ran and produced nothing is a planned goal with an empty ledger, which is
	// a stall, not an invitation to plan again on the next pass.
	Planned bool `json:"planned,omitempty"`
	// Ledger is the observed state of Spec.Ledger, one entry per planned item in the same
	// order. Every entry starts unproven, so completion is a transition here.
	Ledger []LedgerState `json:"ledger,omitempty"`
	// Units is the observed state of Spec.Units, one entry per unit in the same order:
	// which have had a child created, which children have finished, and which finished
	// having proven their unit. A run that crashes with children in flight resumes
	// knowing they exist and does not create them a second time.
	Units []UnitState `json:"units,omitempty"`
	// Invariants is the observed state of Spec.Invariants: which terms the run adopted,
	// their wording at adoption, how often each was audited, and any breach. It is durable
	// because the adopted wording is what a later rewording is caught against, and a
	// recorded breach stops the goal converging on a pass that runs no audit of its own.
	Invariants []InvariantState `json:"invariants,omitempty"`
	// Steers is the observed state of Spec.Steers: which redirects the run was given, their
	// wording at the time, and the account that discharged each. The wording is what a
	// later rewording is caught against; the account records whether the operator was
	// answered.
	Steers []SteerState `json:"steers,omitempty"`
	// Killed is when the reconciler stopped this run under Spec.Kill. A spec that later
	// drops the order is caught against it, so a run a person stopped cannot be edited
	// into one that stopped on its own.
	Killed *time.Time `json:"killed,omitempty"`
	// VerifyPending marks that a build step has completed and the current ledger item's
	// declared check has not run since. It alternates the run between building and
	// verifying, so one check runs per build step and not one per reconcile tick. It is
	// durable so a restart does not build twice and verify once.
	VerifyPending bool `json:"verifyPending,omitempty"`
	// ItemFeedback is the detail of the last verification that did not pass, so the next
	// build step is told what the item's declared check reported. The reconciler clears it
	// once the item is proven.
	ItemFeedback string `json:"itemFeedback,omitempty"`
	// IdleStreak, ProgressMark and LastActivity drive no-progress detection (progress.go).
	// ProgressMark is the fingerprint of substantive work after the last step that made
	// any, LastActivity describes the most recent step so a stall can name it, and
	// IdleStreak counts the consecutive steps since ProgressMark last changed.
	IdleStreak   int    `json:"idleStreak,omitempty"`
	ProgressMark string `json:"progressMark,omitempty"`
	LastActivity string `json:"lastActivity,omitempty"`
	// ProgressNudge is the warning the next step is handed when the goal is stalling but
	// not yet stopped (see ProgressWarning). The executor surfaces it to the agent and
	// clears it once delivered.
	ProgressNudge string `json:"progressNudge,omitempty"`
	// LastVerdict, VerdictMark and VerdictRepeat drive non-convergence detection
	// (converge.go). LastVerdict is the refusal the run was last given verbatim, so the
	// stall can quote it; VerdictMark is the normalized form it was compared on, with how
	// much of the ledger stood proven; VerdictRepeat counts the consecutive build-and-check
	// cycles that ended in that same refusal.
	LastVerdict   string `json:"lastVerdict,omitempty"`
	VerdictMark   string `json:"verdictMark,omitempty"`
	VerdictRepeat int    `json:"verdictRepeat,omitempty"`
	// MissingCommand and MissingRepeat drive the environment stall (environment.go):
	// the program the last verifications found the host without, and how many in a row
	// did. The worker writes them with ItemFeedback, from the same verdict.
	MissingCommand string `json:"missingCommand,omitempty"`
	MissingRepeat  int    `json:"missingRepeat,omitempty"`
	// Unwired marks a stall that describes the loop and not the run: the goal needed a
	// producer and the reconciler it met had none (see stall). It is the one stall a later
	// reconcile re-examines. A spent budget or a failed step stays true however often it
	// is looked at; a missing producer stops being true the moment it is wired. Without
	// this the goal would stay settled on the no-op skip until recreated.
	Unwired bool `json:"unwired,omitempty"`
}

// unwiredStalls names the stall reasons that mean a producer was missing and not that
// the run failed. A reason listed here is recoverable by wiring the producer, so a new
// "...Missing" stall belongs in this map in the same commit that introduces it.
var unwiredStalls = map[string]bool{
	"InvariantAuditorMissing": true,
	"SteerJudgeMissing":       true,
	"UnitSpawnerMissing":      true,
	"WindowSourceMissing":     true,
}

// stall settles the status as stopped for reason, carrying message. Every stall goes
// through here, so the two conditions are always set as a pair and Unwired reflects the
// reason and not whatever the last stall left behind.
//
// CondStalled carries the specific reason; CondReconciling carries "Stalled" because
// all it reports is that the loop stopped driving this goal.
func (s *Status) stall(reason, message string, now time.Time) {
	s.Phase = PhaseStalled
	s.Message = message
	s.Unwired = unwiredStalls[reason]
	s.SetCondition(Condition{Type: CondStalled, Status: "True", Reason: reason, Message: message}, now)
	s.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "Stalled"}, now)
}

// Condition is one standard status condition (the shared resource.Condition).
type Condition = resource.Condition

// RegisterKind registers the Goal kind so goals can be stored and admitted like
// any other resource.
func RegisterKind(reg *resource.Registry) error {
	return reg.Register(resource.Kind{
		APIVersion: GroupVersion,
		Name:       Kind,
		Schema:     specSchema,
		Singular:   "goal",
		Plural:     "goals",
	})
}

// DecodeSpec reads the typed spec from a resource.
func DecodeSpec(r resource.Resource) (Spec, error) { return resource.DecodeSpec[Spec](r) }

// DecodeStatus reads the typed status from a resource.
func DecodeStatus(r resource.Resource) (Status, error) { return resource.DecodeStatus[Status](r) }

// statusHead is the subset of Status the reconciler reads before it commits to doing
// work: the phase and observed spec hash that decide the no-op skip. Decoding into it
// skips over the opaque Checkpoint (which carries the whole transcript) instead of
// copying it, so the periodic resync of already-settled goals no longer materializes
// the transcript on every pass just to find it has nothing to do.
type statusHead struct {
	Phase            Phase  `json:"phase,omitempty"`
	ObservedSpecHash string `json:"observedSpecHash,omitempty"`
	Unwired          bool   `json:"unwired,omitempty"`
}

// settled reports whether this goal is done being reconciled while its spec is
// unchanged. A converged goal is; a stalled one is, unless it stalled because a
// producer was missing, in which case the next reconcile takes another look: the loop
// it meets this time may have the thing it needed.
func (h statusHead) settled() bool {
	return h.Phase == PhaseConverged || (h.Phase == PhaseStalled && !h.Unwired)
}

// decodeStatusHead reads only the scalar status fields the no-op skip needs, without
// copying the Checkpoint. The full DecodeStatus runs only once the reconcile is going
// to act on the goal.
func decodeStatusHead(r resource.Resource) (statusHead, error) {
	var h statusHead
	if len(r.Status) == 0 {
		return h, nil
	}
	return h, json.Unmarshal(r.Status, &h)
}

// Encode marshals the status for writing back onto a resource.
func (s Status) Encode() (json.RawMessage, error) { return resource.EncodeStatus(s) }

// SetCondition upserts c by type, stamping LastTransitionTime only when the
// status value actually changes (so a no-op reconcile does not churn the time).
func (s *Status) SetCondition(c Condition, now time.Time) {
	s.Conditions = resource.SetCondition(s.Conditions, c, now)
}

func hasFinalizer(fz []string, key string) bool {
	for _, f := range fz {
		if f == key {
			return true
		}
	}
	return false
}

func removeFinalizer(fz []string, key string) []string {
	out := fz[:0:0]
	for _, f := range fz {
		if f != key {
			out = append(out, f)
		}
	}
	return out
}
