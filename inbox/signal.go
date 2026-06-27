// Package inbox is the agent's unified inbound boundary. Every message or event
// the agent receives, from any source, is recorded as a Signal resource and then
// triaged into an action: a reply, a goal, a memory write, a notification, or
// nothing. Separating where input arrives (a Source) from what the agent does
// about it (the triage reconciler) means a new platform is a small adapter rather
// than new agent logic, and every inbound item is durable, replayable, audited,
// and governed at one waist.
//
// Signal is the inbound envelope expressed on the same resource + reconcile
// substrate as Goal: declarative and level-triggered, so triage is crash-resumable
// and a lost change hint is recovered by resync rather than dropping the input.
package inbox

import (
	"encoding/json"
	"time"

	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the Signal kind's API group and version.
	GroupVersion = "inbox.ionagent.io/v1alpha1"
	// Kind is the resource kind name.
	Kind = "Signal"
)

// Phase is a coarse lifecycle summary of a signal: received, triaged into a
// disposition, then a terminal acted or dropped.
type Phase string

const (
	PhaseReceived Phase = "Received" // recorded, not yet triaged
	PhaseTriaged  Phase = "Triaged"  // a disposition was chosen, action in progress
	PhaseActed    Phase = "Acted"    // the disposition completed
	PhaseDropped  Phase = "Dropped"  // triage chose to take no action
)

// Disposition is the action triage chose for a signal. Empty means not yet
// triaged.
type Disposition string

const (
	DispositionReply  Disposition = "Reply"  // answer in the originating conversation
	DispositionGoal   Disposition = "Goal"   // run it as a goal
	DispositionStore  Disposition = "Store"  // record it (e.g. to memory) without replying
	DispositionNotify Disposition = "Notify" // push a message out, not a reply to input
	DispositionDrop   Disposition = "Drop"   // intentionally ignore
)

// Standard condition types (kstatus-style: present and True only when noteworthy).
const (
	CondTriaged = "Triaged" // True once a disposition has been chosen
	CondActed   = "Acted"   // True once the disposition has completed
	CondFailed  = "Failed"  // True when acting on the signal failed terminally
)

var specSchema = json.RawMessage(`{
  "type": "object",
  "required": ["source"],
  "properties": {
    "source": {"type": "string", "minLength": 1},
    "conversation": {"type": "string"},
    "sender": {"type": "string"},
    "type": {"type": "string"},
    "content": {"type": "string"},
    "metadata": {"type": "object", "additionalProperties": {"type": "string"}},
    "receivedAt": {"type": "string"}
  },
  "additionalProperties": false
}`)

// Spec is an inbound signal's content: where it came from and what it carries. It
// is immutable input, set once when the signal is received.
type Spec struct {
	// Source names the adapter the signal arrived on, e.g. "telegram". Replies are
	// routed back to the Sink registered under this name.
	Source string `json:"source"`
	// Conversation identifies the thread on the source platform and is the reply
	// address scope. Empty for sources that are not conversational (a monitor).
	Conversation string `json:"conversation,omitempty"`
	// Sender is the platform user id or handle, for routing and audit. Optional.
	Sender string `json:"sender,omitempty"`
	// Type is the nature of the signal; defaults to "message" when unset.
	Type string `json:"type,omitempty"`
	// Content is the message body or event payload.
	Content string `json:"content,omitempty"`
	// Metadata carries source-specific extras without widening the schema.
	Metadata map[string]string `json:"metadata,omitempty"`
	// ReceivedAt is when the source observed the signal.
	ReceivedAt time.Time `json:"receivedAt,omitempty"`
}

// Status is a signal's observed triage state.
type Status struct {
	Phase Phase `json:"phase,omitempty"`
	// Disposition is the action triage chose; empty until triaged.
	Disposition Disposition `json:"disposition,omitempty"`
	// ObservedSpecHash is the resource.SpecHash triage last acted on, so a
	// re-reconcile is a no-op once the signal has settled.
	ObservedSpecHash string `json:"observedSpecHash,omitempty"`
	// GoalName is the goal a signal was routed to, set when Disposition is Goal.
	GoalName   string      `json:"goalName,omitempty"`
	Conditions []Condition `json:"conditions,omitempty"`
	Message    string      `json:"message,omitempty"`
}

// Condition is one standard status condition.
type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"` // "True" | "False" | "Unknown"
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// RegisterKind registers the Signal kind so signals can be stored and admitted
// like any other resource.
func RegisterKind(reg *resource.Registry) error {
	return reg.Register(resource.Kind{
		APIVersion: GroupVersion,
		Name:       Kind,
		Schema:     specSchema,
		Singular:   "signal",
		Plural:     "signals",
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

// SetCondition upserts c by type, stamping LastTransitionTime only when the status
// value actually changes, so a no-op reconcile does not churn the time.
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
