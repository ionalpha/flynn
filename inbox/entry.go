// Package inbox records every inbound message or event as an Entry resource and
// triages it into a reply, a goal or nothing. A [Source] only records entries and
// [Triage] alone decides what to do, so a new platform is a small adapter and every
// disposition goes through one policy.
//
// Entries are reconciled level-triggered like goals, so triage resumes after a
// crash and a lost change hint is recovered by resync.
package inbox

import (
	"encoding/json"
	"time"

	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the Entry kind's API group and version.
	GroupVersion = "inbox.ionagent.io/v1alpha1"
	// Kind is the resource kind name.
	Kind = "Entry"
)

// Phase is an entry's lifecycle stage.
type Phase string

// Acted and Dropped are terminal.
const (
	PhaseReceived Phase = "Received" // recorded, not yet triaged
	PhaseTriaged  Phase = "Triaged"  // a disposition was chosen, action in progress
	PhaseActed    Phase = "Acted"    // the disposition completed
	PhaseDropped  Phase = "Dropped"  // triage chose to take no action
)

// Disposition is the action triage chose for an entry. Empty means not yet
// triaged.
type Disposition string

// Store and Notify are reserved; [Triage] treats them as Drop.
const (
	DispositionReply  Disposition = "Reply"  // answer in the originating conversation
	DispositionGoal   Disposition = "Goal"   // run it as a goal
	DispositionStore  Disposition = "Store"  // record it (e.g. to memory) without replying
	DispositionNotify Disposition = "Notify" // push a message out, not a reply to input
	DispositionDrop   Disposition = "Drop"   // intentionally ignore
)

// Condition types, kstatus-style: present and True only when noteworthy.
const (
	CondTriaged = "Triaged" // True once a disposition has been chosen
	CondActed   = "Acted"   // True once the disposition has completed
	CondFailed  = "Failed"  // True when acting on the entry failed terminally
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

// Spec is an inbound entry's content, set once on receipt.
type Spec struct {
	// Source names the adapter, e.g. "telegram"; replies go to the [Sink] of the
	// same name.
	Source string `json:"source"`
	// Conversation is the reply address on the platform. Empty for sources that are
	// not conversational, such as a monitor.
	Conversation string `json:"conversation,omitempty"`
	// Sender is the platform user id or handle. Optional.
	Sender string `json:"sender,omitempty"`
	// Type defaults to "message".
	Type    string `json:"type,omitempty"`
	Content string `json:"content,omitempty"`
	// Metadata carries source-specific extras without widening the schema.
	Metadata map[string]string `json:"metadata,omitempty"`
	// ReceivedAt is when the source observed the entry; the ingester fills it
	// when unset.
	ReceivedAt time.Time `json:"receivedAt,omitempty"`
}

// Status is an entry's observed triage state.
type Status struct {
	Phase       Phase       `json:"phase,omitempty"`
	Disposition Disposition `json:"disposition,omitempty"`
	// ObservedSpecHash is the spec hash triage last acted on, so a re-reconcile of
	// a settled entry is a no-op.
	ObservedSpecHash string `json:"observedSpecHash,omitempty"`
	// GoalName is the [Worker] handle for a Reply or Goal disposition.
	GoalName   string      `json:"goalName,omitempty"`
	Conditions []Condition `json:"conditions,omitempty"`
	Message    string      `json:"message,omitempty"`
}

// Condition is a status condition.
type Condition = resource.Condition

// RegisterKind registers the Entry kind with reg.
func RegisterKind(reg *resource.Registry) error {
	return reg.Register(resource.Kind{
		APIVersion: GroupVersion,
		Name:       Kind,
		Schema:     specSchema,
		Singular:   "entry",
		Plural:     "entries",
	})
}

// DecodeSpec reads the typed spec from a resource.
func DecodeSpec(r resource.Resource) (Spec, error) { return resource.DecodeSpec[Spec](r) }

// DecodeStatus reads the typed status from a resource.
func DecodeStatus(r resource.Resource) (Status, error) { return resource.DecodeStatus[Status](r) }

// Encode marshals the status for writing back onto a resource.
func (s Status) Encode() (json.RawMessage, error) { return resource.EncodeStatus(s) }

// SetCondition upserts c by type. LastTransitionTime changes only with the status
// value, so a no-op reconcile does not churn it.
func (s *Status) SetCondition(c Condition, now time.Time) {
	s.Conditions = resource.SetCondition(s.Conditions, c, now)
}
