package resource

import (
	"encoding/json"
	"time"
)

// This file is the typed-kind surface: the generic spec/status codec and the
// shared status Condition every kind package used to hand-copy. A new kind
// declares its Spec and Status structs and delegates here; the scaffold tax
// (three decode functions and a condition upsert per package) is gone, and a
// decode-rule fix lands once instead of once per kind.

// DecodeSpec reads a resource's typed spec. An empty spec decodes to the zero
// value, so a kind whose spec is optional reads cleanly without a nil check.
func DecodeSpec[T any](r Resource) (T, error) {
	var s T
	if len(r.Spec) == 0 {
		return s, nil
	}
	return s, json.Unmarshal(r.Spec, &s)
}

// DecodeStatus reads a resource's typed status. An empty status decodes to the
// zero value: a freshly created resource has no status yet.
func DecodeStatus[T any](r Resource) (T, error) {
	var s T
	if len(r.Status) == 0 {
		return s, nil
	}
	return s, json.Unmarshal(r.Status, &s)
}

// EncodeStatus marshals a typed status for writing back onto a resource.
func EncodeStatus[T any](s T) (json.RawMessage, error) { return json.Marshal(s) }

// Condition is one standard status condition, the Kubernetes-shaped signal a
// controller writes to say where a resource stands (Ready, Stalled, ...). It is
// shared so every kind reports conditions in one shape and a generic reader
// (a CLI listing, a wait-for-ready helper) can interpret any kind's status.
type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"` // "True" | "False" | "Unknown"
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// SetCondition upserts c by type and returns the updated slice, stamping
// LastTransitionTime only when the status value actually changes, so a no-op
// reconcile does not churn the time.
func SetCondition(conds []Condition, c Condition, now time.Time) []Condition {
	for i := range conds {
		if conds[i].Type != c.Type {
			continue
		}
		if conds[i].Status == c.Status {
			c.LastTransitionTime = conds[i].LastTransitionTime
		} else {
			c.LastTransitionTime = now
		}
		conds[i] = c
		return conds
	}
	c.LastTransitionTime = now
	return append(conds, c)
}
