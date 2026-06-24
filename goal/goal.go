// Package goal is the agent's first desired-state kind: a Goal declares an
// objective and a stop condition (the desired state), and a reconciler drives it
// toward that condition by dispatching work steps and observing progress (the
// observed state). It is the agent's own executflynn model expressed on the generic
// resource + reconcile substrate: declarative, level-triggered, crash-resumable,
// and budget-bounded, rather than an imperative do-step-do-step loop that loses
// the thread on failure.
package goal

import (
	"encoding/json"
	"time"

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
    "maxSteps": {"type": "integer", "minimum": 0}
  },
  "additionalProperties": false
}`)

// Spec is a goal's desired state: what to achieve, the condition that means it is
// done, and an optional ceiling on how many steps may be spent trying.
type Spec struct {
	Objective     string `json:"objective"`
	StopCondition string `json:"stopCondition"`
	MaxSteps      int    `json:"maxSteps,omitempty"`
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
}

// Condition is one standard status condition.
type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"` // "True" | "False" | "Unknown"
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

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
func DecodeSpec(r resource.Resource) (Spec, error) {
	var s Spec
	if len(r.Spec) == 0 {
		return s, nil
	}
	return s, json.Unmarshal(r.Spec, &s)
}

// DecodeStatus reads the typed status from a resource.
func DecodeStatus(r resource.Resource) (Status, error) {
	var s Status
	if len(r.Status) == 0 {
		return s, nil
	}
	return s, json.Unmarshal(r.Status, &s)
}

// Encode marshals the status for writing back onto a resource.
func (s Status) Encode() (json.RawMessage, error) { return json.Marshal(s) }

// SetCondition upserts c by type, stamping LastTransitionTime only when the
// status value actually changes (so a no-op reconcile does not churn the time).
func (s *Status) SetCondition(c Condition, now time.Time) {
	for i := range s.Conditions {
		if s.Conditions[i].Type != c.Type {
			continue
		}
		if s.Conditions[i].Status == c.Status {
			c.LastTransitionTime = s.Conditions[i].LastTransitionTime
		} else {
			c.LastTransitionTime = now
		}
		s.Conditions[i] = c
		return
	}
	c.LastTransitionTime = now
	s.Conditions = append(s.Conditions, c)
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
