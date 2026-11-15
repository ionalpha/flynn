// Package archetype defines the Agent resource kind: a versioned description of how
// an agent runs (system prompt, capabilities, model and loop, skill and memory
// scope). As a stored resource an Agent is listable, diffable, grant-narrowable and
// fleet-syncable, and a delegated run "as" an Agent is governed by exactly the
// capabilities it declares.
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

// Spec is an Agent's desired configuration. Every field is optional; the zero Spec
// runs the default loop with no special capabilities.
type Spec struct {
	// System is the standing system prompt for every turn. Empty uses the host
	// default.
	System string `json:"system,omitempty"`
	// Capabilities is the exact set of action names a run as this agent may take.
	// An unlisted tool is refused at dispatch even when the host offers it, so the
	// Agent is the least-privilege boundary. The model call is always permitted.
	Capabilities []string `json:"capabilities,omitempty"`
	// Model is the provider:model this agent runs on. Empty uses the host's model.
	Model string `json:"model,omitempty"`
	// Driver names the run loop from the driver registry. Empty uses the default
	// loop.
	Driver string `json:"driver,omitempty"`
	// SkillScope and MemoryScope bound the skills and memory the agent sees. Empty
	// means the host default.
	SkillScope  string `json:"skillScope,omitempty"`
	MemoryScope string `json:"memoryScope,omitempty"`
	// Tools and Knowledge name the implementations and knowledge sources the agent
	// is built with; the capability grant is the authority to call them. The host
	// resolves them to implementations.
	Tools     []string `json:"tools,omitempty"`
	Knowledge []string `json:"knowledge,omitempty"`
	// Extends names base Agents to compose from, so a specialist is a base plus
	// overrides rather than a duplicated spec. Bases merge in order and this Agent
	// goes on top: scalar fields take the most-derived non-empty value, and
	// capabilities, tools and knowledge are unioned. See Resolve.
	Extends []string `json:"extends,omitempty"`
}

// Grant builds the capability grant for a run as this agent, admitting only the
// listed actions. A delegate spawned under it narrows from here and never widens.
func (s Spec) Grant() capability.Grant {
	return capability.NewGrant(s.Capabilities...)
}

// Status is an Agent's observed state: the reconciled spec generation, since an
// Agent is mostly desired-state configuration.
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
    "memoryScope": {"type": "string"},
    "tools": {"type": "array", "items": {"type": "string"}},
    "knowledge": {"type": "array", "items": {"type": "string"}},
    "extends": {"type": "array", "items": {"type": "string"}}
  },
  "additionalProperties": false
}`)

// RegisterKind registers the Agent kind with reg.
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
func DecodeSpec(r resource.Resource) (Spec, error) { return resource.DecodeSpec[Spec](r) }

// DecodeStatus reads the typed status from a resource.
func DecodeStatus(r resource.Resource) (Status, error) { return resource.DecodeStatus[Status](r) }

// Encode renders the status for storage.
func (s Status) Encode() (json.RawMessage, error) { return resource.EncodeStatus(s) }
