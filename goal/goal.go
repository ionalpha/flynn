// Package goal declares an objective and a stop condition as desired state, and a
// [Reconciler] drives a Goal toward it by dispatching steps. Progress is recorded in
// status, so a crash resumes mid-goal and spending stays bounded.
package goal

import (
	"encoding/json"
	"time"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the API group and version of the Goal kind.
	GroupVersion = "goal.ionagent.io/v1alpha1"
	// Kind is the resource kind name.
	Kind = "Goal"
	// Finalizer holds deletion until child goals, runs and worktrees are cleaned up.
	Finalizer = "goal.ionagent.io/cleanup"
	// StepJobKind is the job kind of a build step.
	StepJobKind = "goal.step"
	// PlanJobKind shares the step queue and worker, so planning inherits its lease,
	// crash-resume and retries.
	PlanJobKind = "goal.plan"
	// VerifyJobKind shares the step queue and worker like [PlanJobKind].
	VerifyJobKind = "goal.verify"
)

// Phase is a summary for people; the conditions are the source of truth.
type Phase string

// The phases a goal moves through. Converged and Stalled are terminal.
const (
	PhasePending   Phase = "Pending"
	PhasePlanning  Phase = "Planning" // writing the ledger, before any building
	PhaseRunning   Phase = "Running"
	PhaseConverged Phase = "Converged"
	PhaseStalled   Phase = "Stalled"
)

// Condition types, kstatus style: an abnormal condition is True only while it holds.
const (
	CondReady       = "Ready"
	CondReconciling = "Reconciling"
	CondStalled     = "Stalled"
)

var specSchema = json.RawMessage(`{
  "type": "object",
  "required": ["objective", "stopCondition"],
  "properties": {
    "confinement": {
      "type": "object",
      "properties": {
        "workdir": {"type": "string"},
        "grant": {"type": "array", "items": {"type": "string"}},
        "unrestricted": {"type": "boolean"}
      },
      "additionalProperties": false
    },
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

// Spec is a goal's desired state. MaxSteps zero means [DefaultMaxSteps].
type Spec struct {
	Objective     string `json:"objective"`
	StopCondition string `json:"stopCondition"`
	MaxSteps      int    `json:"maxSteps,omitempty"`
	// Grant travels with the goal, so one executor drives goals of differing authority. A
	// child's is a subset of its parent's. Empty takes the executor's default.
	Grant []string `json:"grant,omitempty"`
	// Depth counts delegation hops from the root (0), so a spawner can refuse unbounded
	// recursion.
	Depth int `json:"depth,omitempty"`
	// BudgetPool is inherited from the root, so a fan-out has one ceiling. Empty means the
	// goal is its own pool.
	BudgetPool string `json:"budgetPool,omitempty"`
	// Budget exists beside MaxSteps because step cost varies by an order of magnitude. It
	// is checked against spend on BudgetPool. The zero value bounds nothing.
	Budget SpendBudget `json:"budget,omitempty"`
	// System, Driver and Model let a child differ from its parent. Empty takes the default.
	System string `json:"system,omitempty"`
	Driver string `json:"driver,omitempty"`
	Model  string `json:"model,omitempty"`
	// Attachments go on the opening user turn, whose text is the objective.
	Attachments []llm.Image `json:"attachments,omitempty"`
	// Ledger is written by the planner before building and is append-only after.
	Ledger []LedgerItem `json:"ledger,omitempty"`
	// Units is fan-out planned in advance; see [Unit].
	Units []Unit `json:"units,omitempty"`
	// Invariants are audited before the stop evaluator, and an adopted term cannot be
	// dropped or reworded.
	Invariants []Invariant `json:"invariants,omitempty"`
	// Steers refuse completion until the run says what it did about each.
	Steers []Steer `json:"steers,omitempty"`
	// Kill engages the run's halt, so the stop reaches inside the running step.
	Kill *Kill `json:"kill,omitempty"`
	// Allowances authorize irreversible actions outside the workspace. An undeclared one
	// pauses the run with the ask, because a refusal sends the model looking for another
	// route.
	Allowances []Allowance `json:"allowances,omitempty"`
	// Confinement is set by the submitting runtime and inherited by children; see
	// [Confinement].
	Confinement *Confinement `json:"confinement,omitempty"`
}

// SpendBudget caps pool spend; WindowFraction, in [0,1], caps plan-window share and needs
// a [WindowSource]. A zero field is unbounded. Crossing a ceiling stops the goal, which a
// provider's limit (pause until reset) and a transient error (retry) never do.
type SpendBudget struct {
	Tokens         int64   `json:"tokens,omitempty"`
	Cost           float64 `json:"cost,omitempty"`
	WindowFraction float64 `json:"windowFraction,omitempty"`
}

// IsZero lets the reconciler skip the spend guard.
func (b SpendBudget) IsZero() bool {
	return b.Tokens == 0 && b.Cost == 0 && b.WindowFraction == 0
}

// InFlight makes a re-reconcile observe the running step instead of duplicating it.
type InFlight struct {
	JobID     string    `json:"jobID"`
	StartedAt time.Time `json:"startedAt"`
	// Kind separates a planning step, which spends no build budget, when the reservation
	// clears. Empty reads as a build step.
	Kind string `json:"kind,omitempty"`
}

// Status is durable, so every field here survives a crash.
type Status struct {
	Phase Phase `json:"phase,omitempty"`
	// ObservedSpecHash makes a settled goal's reconcile a no-op until the spec changes.
	ObservedSpecHash string      `json:"observedSpecHash,omitempty"`
	Steps            int         `json:"steps,omitempty"`
	InFlight         *InFlight   `json:"inFlight,omitempty"`
	Conditions       []Condition `json:"conditions,omitempty"`
	Message          string      `json:"message,omitempty"`
	// Checkpoint is owned by the step executor; the reconciler never interprets it.
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
	// WaitingSince parks the goal after [ErrWaiting]: no step, stop check or step budget is
	// spent. A settling child clears it, and the recheck fallback covers a lost wake.
	WaitingSince *time.Time `json:"waitingSince,omitempty"`
	// Planned is separate from a non-empty Ledger, because a planner that produced nothing
	// is a stall and must not plan again.
	Planned bool `json:"planned,omitempty"`
	// Ledger entries start unproven, so completion is a recorded transition.
	Ledger []LedgerState `json:"ledger,omitempty"`
	Units  []UnitState   `json:"units,omitempty"`
	// Invariants keeps adopted wording, so a rewording is caught, and breaches, so a pass
	// with no audit cannot converge.
	Invariants []InvariantState `json:"invariants,omitempty"`
	// Steers keeps the wording given, so a rewording is caught, and each discharging account.
	Steers []SteerState `json:"steers,omitempty"`
	// Killed is checked against the spec, so a stopped run cannot be edited into one that
	// stopped on its own.
	Killed *time.Time `json:"killed,omitempty"`
	// VerifyPending alternates build and check, one check per build step. It is durable so
	// a restart does not build twice and verify once.
	VerifyPending bool `json:"verifyPending,omitempty"`
	// ItemFeedback tells the next build step what the failed check reported. It clears when
	// the item is proven.
	ItemFeedback string `json:"itemFeedback,omitempty"`
	// IdleStreak counts steps since ProgressMark changed; LastActivity lets a stall name
	// the latest step.
	IdleStreak   int    `json:"idleStreak,omitempty"`
	ProgressMark string `json:"progressMark,omitempty"`
	LastActivity string `json:"lastActivity,omitempty"`
	// ProgressNudge is cleared by the executor once delivered.
	ProgressNudge string `json:"progressNudge,omitempty"`
	// LastVerdict is verbatim so a stall can quote it. VerdictMark is the normalized form
	// plus proven count, and VerdictRepeat counts cycles ending in it.
	LastVerdict   string `json:"lastVerdict,omitempty"`
	VerdictMark   string `json:"verdictMark,omitempty"`
	VerdictRepeat int    `json:"verdictRepeat,omitempty"`
	// MissingCommand is written with ItemFeedback from the same verdict.
	MissingCommand string `json:"missingCommand,omitempty"`
	MissingRepeat  int    `json:"missingRepeat,omitempty"`
	// Unwired marks the one stall a reconcile re-examines, because a missing producer stops
	// being true once wired. Without it the no-op skip would hold the goal until recreated.
	Unwired bool `json:"unwired,omitempty"`
}

// unwiredStalls must gain every new "...Missing" stall reason, or that stall never
// recovers when the producer is wired.
var unwiredStalls = map[string]bool{
	"InvariantAuditorMissing": true,
	"SteerJudgeMissing":       true,
	"UnitSpawnerMissing":      true,
	"WindowSourceMissing":     true,
}

// stall is the only stall path, so both conditions are set as a pair and Unwired never
// carries over from an earlier stall.
func (s *Status) stall(reason, message string, now time.Time) {
	s.Phase = PhaseStalled
	s.Message = message
	s.Unwired = unwiredStalls[reason]
	s.SetCondition(Condition{Type: CondStalled, Status: "True", Reason: reason, Message: message}, now)
	s.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "Stalled"}, now)
}

// Condition is [resource.Condition].
type Condition = resource.Condition

// RegisterKind must run before a goal is stored, so its spec is schema-checked.
func RegisterKind(reg *resource.Registry) error {
	return reg.Register(resource.Kind{
		APIVersion: GroupVersion,
		Name:       Kind,
		Schema:     specSchema,
		Singular:   "goal",
		Plural:     "goals",
	})
}

// DecodeSpec returns the zero Spec for an empty spec.
func DecodeSpec(r resource.Resource) (Spec, error) { return resource.DecodeSpec[Spec](r) }

// DecodeStatus copies the whole transcript in Checkpoint; the no-op skip avoids it.
func DecodeStatus(r resource.Resource) (Status, error) { return resource.DecodeStatus[Status](r) }

// statusHead skips Checkpoint, so resyncing a settled goal never copies its transcript.
type statusHead struct {
	Phase            Phase  `json:"phase,omitempty"`
	ObservedSpecHash string `json:"observedSpecHash,omitempty"`
	Unwired          bool   `json:"unwired,omitempty"`
}

// settled excludes an unwired stall, because the next loop may have the producer.
func (h statusHead) settled() bool {
	return h.Phase == PhaseConverged || (h.Phase == PhaseStalled && !h.Unwired)
}

func decodeStatusHead(r resource.Resource) (statusHead, error) {
	var h statusHead
	if len(r.Status) == 0 {
		return h, nil
	}
	err := json.Unmarshal(r.Status, &h)
	return h, err
}

// Encode is the inverse of [DecodeStatus].
func (s Status) Encode() (json.RawMessage, error) { return resource.EncodeStatus(s) }

// SetCondition stamps LastTransitionTime only on a value change, so a no-op reconcile
// does not churn it.
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
