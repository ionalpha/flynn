// Package budget is the agent's spend ceiling: a per-run pool of tokens and cost
// that every model and tool call is charged against at the dispatch waist, so a
// run, or a whole fan-out of runs sharing one pool, cannot exceed the limit set
// for it. It is the enforcement half of the README's "shared budget pool"; the
// metering it charges is the dispatch.Metering each governed action already
// reports.
//
// A budget is an ordinary Resource (kind "Budget") keyed by the run id, so it is
// durable, replayable, and shared: concurrent runs charge the same record under
// optimistic concurrency, and a crash-resumed run reads the spend already made
// rather than a fresh pool. A run with no budget bound is unlimited, which keeps
// the standalone agent zero-config; binding one switches the posture to
// default-deny once the ceiling is reached.
package budget

import (
	"encoding/json"

	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the Budget kind's API group and version.
	GroupVersion = "budget.ionagent.io/v1alpha1"
	// Kind is the resource kind name.
	Kind = "Budget"
)

// Limits is a run's spend ceiling. A zero field means no limit on that axis, so
// the zero Limits is unlimited (a budget that only tracks spend without capping
// it). Tokens is a total token count; Cost is in the provider's currency unit.
type Limits struct {
	Tokens int64   `json:"tokens,omitempty"`
	Cost   float64 `json:"cost,omitempty"`
}

// Exceeded reports whether spent has reached or passed any limit that is set. A
// zero limit on an axis never triggers, so a run is capped only on the axes its
// budget actually constrains.
func (l Limits) Exceeded(s Spent) bool {
	if l.Tokens > 0 && s.Tokens >= l.Tokens {
		return true
	}
	if l.Cost > 0 && s.Cost >= l.Cost {
		return true
	}
	return false
}

// Spent is what a run has consumed so far, accumulated from the metering of every
// governed action it ran.
type Spent struct {
	Tokens int64   `json:"tokens,omitempty"`
	Cost   float64 `json:"cost,omitempty"`
}

// plus returns the element-wise sum of two Spent values.
func (s Spent) plus(o Spent) Spent {
	return Spent{Tokens: s.Tokens + o.Tokens, Cost: s.Cost + o.Cost}
}

// minusFloored returns s - o on each axis, never below zero, so releasing a
// reservation can never drive the reserved total negative under a lost race.
func (s Spent) minusFloored(o Spent) Spent {
	out := Spent{Tokens: s.Tokens - o.Tokens, Cost: s.Cost - o.Cost}
	if out.Tokens < 0 {
		out.Tokens = 0
	}
	if out.Cost < 0 {
		out.Cost = 0
	}
	return out
}

// IsZero reports whether the value consumes nothing on any axis.
func (s Spent) IsZero() bool { return s.Tokens == 0 && s.Cost == 0 }

// Spec is a budget's desired state: the ceiling the run may spend up to.
type Spec struct {
	Limits Limits `json:"limits"`
}

// Status is a budget's observed state: what the run has spent, and what it has
// reserved but not yet spent. The ledger accumulates both under optimistic
// concurrency. Reserved is the in-flight commitment a reserve-before-dispatch
// holds against the pool so concurrent actions (a fan-out of children sharing one
// budget) admit against one consistent view and cannot all pass an under-budget
// check and then overshoot together. An action settles its reservation into Spent
// when it finishes.
type Status struct {
	Spent    Spent `json:"spent"`
	Reserved Spent `json:"reserved,omitempty"`
}

// committed is the total a budget has promised: spent plus still-reserved. The
// ceiling is enforced against this, so an admitted-but-unfinished action counts
// against the pool until it settles.
func (s Status) committed() Spent { return s.Spent.plus(s.Reserved) }

var specSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "limits": {
      "type": "object",
      "properties": {
        "tokens": {"type": "integer", "minimum": 0},
        "cost": {"type": "number", "minimum": 0}
      },
      "additionalProperties": false
    }
  },
  "additionalProperties": false
}`)

// RegisterKind registers the Budget kind so budgets can be stored and admitted
// like any other resource.
func RegisterKind(reg *resource.Registry) error {
	return reg.Register(resource.Kind{
		APIVersion: GroupVersion,
		Name:       Kind,
		Schema:     specSchema,
		Singular:   "budget",
		Plural:     "budgets",
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
