// Package profilestore persists a model's measured capability profile as a first-class resource
// and reads it back as a harness.ProfileSource, so a reliability measurement made once drives how
// hard the harness scaffolds that model on every later run. The profile is the integration point
// between the reliability evaluation that writes it and the dispatch waist that reads it: writing
// is an ordinary resource Put, so a profile is versioned, audited, and synced like any other
// record, and reading loads a snapshot the assembly consults with no per-call store access.
package profilestore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the API group/version of a model profile resource.
	GroupVersion = "harness.ionagent.io/v1"
	// Kind is the resource kind name.
	Kind = "ModelProfile"
)

// specSchema constrains a profile: a model id is required, and every reliability score is a
// fraction in [0,1]. The context figure is a non-negative token count.
var specSchema = json.RawMessage(`{
  "type": "object",
  "required": ["modelID"],
  "properties": {
    "modelID": {"type": "string", "minLength": 1},
    "quant": {"type": "string"},
    "runtime": {"type": "string"},
    "batteryVersion": {"type": "string"},
    "toolCallReliability": {"type": "number", "minimum": 0, "maximum": 1},
    "structuredOutput": {"type": "number", "minimum": 0, "maximum": 1},
    "instructionFollowing": {"type": "number", "minimum": 0, "maximum": 1},
    "effectiveContext": {"type": "integer", "minimum": 0}
  },
  "additionalProperties": false
}`)

// Spec is a measured profile for one model and, where they apply, the quantization and runtime it
// was measured on: a different quant is a different function, so it is recorded. The reliability
// fields mirror harness.ModelProfile; BatteryVersion records which probe set produced them, so a
// score is only compared to another taken the same way.
type Spec struct {
	ModelID              string  `json:"modelID"`
	Quant                string  `json:"quant,omitempty"`
	Runtime              string  `json:"runtime,omitempty"`
	BatteryVersion       string  `json:"batteryVersion,omitempty"`
	ToolCallReliability  float64 `json:"toolCallReliability,omitempty"`
	StructuredOutput     float64 `json:"structuredOutput,omitempty"`
	InstructionFollowing float64 `json:"instructionFollowing,omitempty"`
	EffectiveContext     int     `json:"effectiveContext,omitempty"`
}

// profile is the capability fingerprint the harness reads, mapped from the persisted spec.
func (s Spec) profile() harness.ModelProfile {
	return harness.ModelProfile{
		ToolCallReliability:  s.ToolCallReliability,
		StructuredOutput:     s.StructuredOutput,
		InstructionFollowing: s.InstructionFollowing,
		EffectiveContext:     s.EffectiveContext,
	}
}

// RegisterKind registers the ModelProfile kind so the resource store admits and schema-validates
// profiles like any other resource.
func RegisterKind(reg *resource.Registry) error {
	return reg.Register(resource.Kind{
		APIVersion: GroupVersion,
		Name:       Kind,
		Schema:     specSchema,
		Singular:   "modelprofile",
		Plural:     "modelprofiles",
	})
}

// Write persists a measured profile. The resource name is the (model, quant, runtime) key, so a
// re-measurement of the same target overwrites the prior record rather than accumulating
// duplicates. The store validates the spec against the schema on the way in.
func Write(ctx context.Context, rs resource.Store, spec Spec) error {
	body, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("profilestore: encode spec: %w", err)
	}
	_, err = rs.Put(ctx, resource.Resource{
		APIVersion: GroupVersion,
		Kind:       Kind,
		Name:       resourceName(spec.ModelID, spec.Quant, spec.Runtime),
		Labels:     map[string]string{"model.id": spec.ModelID},
		Spec:       body,
	})
	if err != nil {
		return fmt.Errorf("profilestore: write profile for %q: %w", spec.ModelID, err)
	}
	return nil
}

// resourceName builds a stable, unique resource name from the measurement target. The model id is
// used verbatim where it is name-safe; the optional quant and runtime refine it, so distinct
// measurements of the same model do not collide.
func resourceName(modelID, quant, runtime string) string {
	name := modelID
	if quant != "" {
		name += "--" + quant
	}
	if runtime != "" {
		name += "--" + runtime
	}
	return name
}
