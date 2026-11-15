package resource

import (
	"encoding/json"
	"time"
)

// DecodeSpec reads a resource's typed spec. An empty spec decodes to the zero
// value, so a kind whose spec is optional reads cleanly without a nil check.
func DecodeSpec[T any](r Resource) (T, error) {
	var s T
	if len(r.Spec) == 0 {
		return s, nil
	}
	err := json.Unmarshal(r.Spec, &s)
	return s, err
}

// DecodeStatus reads a resource's typed status. An empty status decodes to the
// zero value: a freshly created resource has no status yet.
func DecodeStatus[T any](r Resource) (T, error) {
	var s T
	if len(r.Status) == 0 {
		return s, nil
	}
	err := json.Unmarshal(r.Status, &s)
	return s, err
}

// EncodeStatus marshals a typed status for writing back onto a resource.
func EncodeStatus[T any](s T) (json.RawMessage, error) { return json.Marshal(s) }

// Condition is one standard status condition (Ready, Stalled) a controller writes.
// Every kind shares it, so a generic reader such as a CLI listing or a
// wait-for-ready helper can interpret any kind's status.
type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"` // "True" | "False" | "Unknown"
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// SetCondition upserts c by type and returns the slice, stamping
// LastTransitionTime only when the status value changes, so a no-op reconcile
// keeps the time.
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
