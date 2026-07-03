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
)

// Phase is a coarse, human-facing lifecycle summary of a goal, derived from its
// conditions (a convenience projection, not the source of truth).
type Phase string

// The phases a goal moves through, from accepted to a terminal converged or
// stalled state.
const (
	PhasePending   Phase = "Pending"   // accepted, no step run yet
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
	// Grant is the exact set of action names this goal may take, carried on the goal
	// itself so authority travels with the work rather than being fixed at the
	// executor. One executor can then drive goals of differing authority (a parent at
	// full grant, a delegated child narrowed to a subset), and a child's grant is set
	// to a subset of its parent's so a delegation can never widen authority. Empty
	// defers to the executor's default grant, so an ungoverned standalone run is
	// unchanged.
	Grant []string `json:"grant,omitempty"`
	// Depth is how many delegation hops separate this goal from a root goal: a root
	// is 0 and a spawned child is its parent's depth plus one. A fan-out spawner
	// refuses to create a child past a maximum depth, so a chain of agents spawning
	// agents cannot recurse without bound.
	Depth int `json:"depth,omitempty"`
	// BudgetPool is the run id whose budget this goal charges and reserves against.
	// A fan-out shares one pool: every descendant inherits the root's pool, so the
	// whole graph is bounded by a single ceiling rather than a budget per goal. Empty
	// means the goal is its own pool (a standalone root).
	BudgetPool string `json:"budgetPool,omitempty"`
	// System is the standing system prompt this goal runs under, carried on the goal
	// so a delegated child can run as a different agent than its parent (its prompt
	// baked in by the spawner from the bound Agent). Empty defers to the executor's
	// default prompt, so a standalone run is unchanged.
	System string `json:"system,omitempty"`
	// Driver names the run loop this goal uses (resolved from the driver registry), and
	// Model names the model it runs on. They are carried on the goal so a delegated
	// child can run a different loop and model than its parent (set by the spawner from
	// the bound Agent). Empty defers to the host default loop and model, so a standalone
	// run is unchanged.
	Driver string `json:"driver,omitempty"`
	Model  string `json:"model,omitempty"`
	// Attachments are images seeded onto the goal's opening user turn. The
	// objective is the opening turn's text; the attachments are its images, so
	// a goal can open on a picture the way a composer prompt can. They are the
	// model port's own image type (bytes inline, like the rest of a
	// conversation in the checkpoint), so no translation is needed to open the
	// turn. Empty on every text-only goal, so the text path serializes
	// identically and is unchanged.
	Attachments []llm.Image `json:"attachments,omitempty"`
}

// InFlight records a dispatched step not yet observed complete, so a re-reconcile
// observes the running work instead of launching a duplicate.
type InFlight struct {
	JobID     string    `json:"jobID"`
	StartedAt time.Time `json:"startedAt"`
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
