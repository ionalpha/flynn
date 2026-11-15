// Package budget enforces a per-run pool of tokens and cost at the dispatch waist, so a
// run, or a fan-out sharing one pool, cannot spend past its ceiling. It charges the
// dispatch.Metering each action reports.
//
// A budget is a Resource of kind "Budget" keyed by the run id, so concurrent runs
// charge one record under optimistic concurrency and a resumed run reads the spend
// already made. A run with no budget bound is unlimited.
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

// Limits is a run's spend ceiling. A zero axis is unlimited, so the zero Limits only
// tracks spend. Cost is in the provider's currency unit.
type Limits struct {
	Tokens int64   `json:"tokens,omitempty"`
	Cost   float64 `json:"cost,omitempty"`
}

// Exceeded reports whether s has reached any limit that is set.
func (l Limits) Exceeded(s Spent) bool {
	if l.Tokens > 0 && s.Tokens >= l.Tokens {
		return true
	}
	if l.Cost > 0 && s.Cost >= l.Cost {
		return true
	}
	return false
}

// Spent is what a run has consumed, summed from the metering of its actions.
type Spent struct {
	Tokens int64   `json:"tokens,omitempty"`
	Cost   float64 `json:"cost,omitempty"`
}

func (s Spent) plus(o Spent) Spent {
	return Spent{Tokens: s.Tokens + o.Tokens, Cost: s.Cost + o.Cost}
}

// minusFloored floors each axis at zero, so a release under a lost race cannot drive
// the reserved total negative.
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

// IsZero reports whether both axes are zero.
func (s Spent) IsZero() bool { return s.Tokens == 0 && s.Cost == 0 }

// Spec is a budget's desired state.
type Spec struct {
	Limits Limits `json:"limits"`
}

// Status is a budget's observed state.
type Status struct {
	Spent Spent `json:"spent"`
	// Reserved is held for in-flight actions, so a fan-out sharing the pool admits
	// against one view and cannot all pass the check and overshoot together.
	Reserved Spent `json:"reserved,omitempty"`
	// ByTier splits Spent by the tier bound with [TierInto]. The limit applies to
	// Spent only, and untiered spend has no entry, so the tiers sum to at most Spent.
	ByTier map[string]Spent `json:"byTier,omitempty"`
	// Extended is the total [Ledger.Extend] has raised the ceiling by, checked against
	// the cap. It is a pointer so a never-extended status marshals unchanged.
	Extended *Limits `json:"extended,omitempty"`
}

// committed is spent plus reserved, so an unfinished action counts against the
// ceiling until it settles.
func (s Status) committed() Spent { return s.Spent.plus(s.Reserved) }

// attribute skips an empty tier and a zero metering, so the zero Status marshals
// unchanged.
func (s *Status) attribute(tier string, tokens int64, cost float64) {
	if tier == "" || (tokens == 0 && cost == 0) {
		return
	}
	if s.ByTier == nil {
		s.ByTier = make(map[string]Spent)
	}
	cur := s.ByTier[tier]
	cur.Tokens += tokens
	cur.Cost += cost
	s.ByTier[tier] = cur
}

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

// RegisterKind registers the Budget kind with reg.
func RegisterKind(reg *resource.Registry) error {
	return reg.Register(resource.Kind{
		APIVersion: GroupVersion,
		Name:       Kind,
		Schema:     specSchema,
		Singular:   "budget",
		Plural:     "budgets",
	})
}

// DecodeSpec decodes r's spec.
func DecodeSpec(r resource.Resource) (Spec, error) { return resource.DecodeSpec[Spec](r) }

// DecodeStatus decodes r's status.
func DecodeStatus(r resource.Resource) (Status, error) { return resource.DecodeStatus[Status](r) }

// Encode marshals s for a resource's status.
func (s Status) Encode() (json.RawMessage, error) { return resource.EncodeStatus(s) }
