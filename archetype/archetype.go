// Package archetype defines the Agent resource kind: a versioned, addressable
// description of how an agent runs. An Agent bundles the standing system prompt,
// the capabilities it may use, the model and loop it runs on, and the skill and
// memory scope it sees. Promoting archetypes from inline configuration to a stored
// resource makes them listable, diffable, grant-narrowable, and fleet-syncable like
// every other resource, and gives a delegation (a spawned specialist) a concrete
// thing to bind to: a run "as" an Agent is governed by exactly the capabilities the
// Agent declares.
package archetype

import (
	"encoding/json"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the Agent kind's API group and version.
	GroupVersion = "agent.ionagent.io/v1alpha1"
	// Kind is the resource kind name.
	Kind = "Agent"
)

// Spec is an Agent's desired shape: how a run as this agent is configured. Every
// field is optional so a minimal Agent is just a name; the zero Spec runs the
// default loop with no special capabilities.
type Spec struct {
	// System is the standing system prompt framing every turn for this agent. Empty
	// uses the host's default prompt.
	System string `json:"system,omitempty"`
	// Capabilities is the exact set of action names a run as this agent may take, the
	// complete record of its authority. A tool the agent does not list is refused at
	// the dispatch waist even though the host offers it, so the Agent resource is the
	// least-privilege boundary. The model call is always permitted; it need not be
	// listed.
	Capabilities []string `json:"capabilities,omitempty"`
	// Model is the provider:model identifier this agent runs on. Empty defers to the
	// host's configured model.
	Model string `json:"model,omitempty"`
	// Driver names the run loop this agent uses (resolved from the driver registry).
	// Empty uses the default general-purpose loop.
	Driver string `json:"driver,omitempty"`
	// SkillScope and MemoryScope bound which skills and memory the agent sees. Empty
	// means the host default scope.
	SkillScope  string `json:"skillScope,omitempty"`
	MemoryScope string `json:"memoryScope,omitempty"`
}

// Grant builds the capability grant for a run as this agent from its declared
// capabilities. It is the authority half of the Agent: the run is admitted only for
// the actions the Agent lists. A delegate spawned under this Agent narrows from
// here, never widens.
func (s Spec) Grant() capability.Grant {
	return capability.NewGrant(s.Capabilities...)
}

// Status is an Agent's observed state. It is minimal today: an Agent is mostly
// desired-state configuration, so there is little to observe beyond which spec
// generation has been reconciled.
type Status struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

var specSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "system": {"type": "string"},
    "capabilities": {"type": "array", "items": {"type": "string"}},
    "model": {"type": "string"},
    "driver": {"type": "string"},
    "skillScope": {"type": "string"},
    "memoryScope": {"type": "string"}
  },
  "additionalProperties": false
}`)

// RegisterKind registers the Agent kind so agents can be stored and admitted like
// any other resource.
func RegisterKind(reg *resource.Registry) error {
	return reg.Register(resource.Kind{
		APIVersion: GroupVersion,
		Name:       Kind,
		Schema:     specSchema,
		Singular:   "agent",
		Plural:     "agents",
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

// Encode renders the status for storage.
func (s Status) Encode() (json.RawMessage, error) { return json.Marshal(s) }
