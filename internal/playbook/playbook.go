// Package playbook turns a multi-step operational procedure into a typed resource that
// Flynn can run: provision the command-line tools it needs, run them, verify the outcome,
// and register what it produced as a supervised Service. A playbook is the outcome-level
// companion to an integration: an integration spec says how to call one API operation, a
// playbook spec says how to accomplish a goal by composing many steps.
//
// A playbook is data. Its body is a declarative flow (the same interpreter an integration
// uses, extended with the dependency, exec, and assert ops), so the stored spec is exactly
// what runs, and a runtime-authored playbook has no path to arbitrary code: every effect
// goes through a port the runner wires (a sandboxed command runner, the dependency manager,
// the resource store), never an ambient capability.
package playbook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/ionalpha/flynn/internal/flow"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the Playbook kind's API group and version.
	GroupVersion = "playbook.ionagent.io/v1alpha1"
	// Kind is the resource kind name playbooks are stored under.
	Kind = "Playbook"
)

// ErrNotFound is returned when a playbook does not exist.
var ErrNotFound = errors.New("playbook: not found")

// ServiceBlock declares how to classify the supervised Service a playbook registers when
// its flow succeeds. The live fields (the service name, its URL, the provider's external
// id, and the addressing the supervisor replays) come from the flow's return value, so all
// templating stays inside the flow; this block supplies only the static classification.
type ServiceBlock struct {
	// Provider is the provider that owns the workload (for example "fly").
	Provider string `json:"provider"`
	// Target classifies the workload (static-site, container, vps). Optional.
	Target service.Target `json:"target,omitempty"`
}

// Spec is the desired shape of a playbook: a description, an optional input schema for the
// config an operator supplies, the flow that carries out the procedure, and an optional
// service block to register on success.
type Spec struct {
	// Description is a short human summary of what the playbook accomplishes.
	Description string `json:"description,omitempty"`
	// Inputs is a JSON Schema describing the config an operator passes to a run. It is
	// documentation and (later) validation; the runner exposes the config to the flow as
	// "config" regardless.
	Inputs json.RawMessage `json:"inputs,omitempty"`
	// Flow is the declarative procedure the playbook runs, in the flow interpreter's shape.
	Flow json.RawMessage `json:"flow"`
	// Service, when set, is the supervised Service to register from the flow's result.
	Service *ServiceBlock `json:"service,omitempty"`
}

var specSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "description": {"type": "string"},
    "inputs": {"type": "object"},
    "flow": {"type": "object"},
    "service": {
      "type": "object",
      "properties": {
        "provider": {"type": "string"},
        "target": {"type": "string", "enum": ["", "static-site", "container", "vps"]}
      },
      "required": ["provider"],
      "additionalProperties": false
    }
  },
  "required": ["flow"],
  "additionalProperties": false
}`)

// KindDef is the Playbook kind definition registered with a resource registry.
var KindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       Kind,
	Schema:     specSchema,
	Singular:   "playbook",
	Plural:     "playbooks",
}

// RegisterKind registers the Playbook kind so a store admits playbooks. It is idempotent.
func RegisterKind(reg *resource.Registry) error { return reg.Register(KindDef) }

// DecodeSpec reads the typed spec from a resource.
func DecodeSpec(r resource.Resource) (Spec, error) {
	s, err := resource.DecodeSpec[Spec](r)
	if err != nil {
		return Spec{}, fmt.Errorf("playbook: decode spec: %w", err)
	}
	return s, nil
}

// DecodeFlow parses and validates the playbook's flow, so a caller can confirm the
// procedure is well-formed before running it (the build-time gate uses this).
func (s Spec) DecodeFlow() (flow.Flow, error) { return flow.Decode(s.Flow) }

// Playbook is the typed view of a playbook resource.
type Playbook struct {
	Name string
	Spec Spec
}

// Store is the typed playbook facade over a resource.Store. Playbooks live in the
// instance-global scope, addressed by name.
type Store struct {
	rs    resource.Store
	scope resource.Scope
}

// NewStore returns a playbook facade over rs. The caller must have registered the Playbook
// kind with the registry rs admits against (see RegisterKind).
func NewStore(rs resource.Store) *Store { return &Store{rs: rs} }

// Put creates or updates the named playbook.
func (s *Store) Put(ctx context.Context, name string, spec Spec) (Playbook, error) {
	if name == "" || len(spec.Flow) == 0 {
		return Playbook{}, errors.New("playbook: a playbook needs a name and a flow")
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return Playbook{}, fmt.Errorf("playbook: encode spec: %w", err)
	}
	r, err := s.rs.Put(ctx, resource.Resource{
		APIVersion: GroupVersion,
		Kind:       Kind,
		Name:       name,
		Scope:      s.scope,
		Spec:       raw,
	})
	if err != nil {
		return Playbook{}, err
	}
	return toPlaybook(r)
}

// Get returns the named playbook, or ErrNotFound.
func (s *Store) Get(ctx context.Context, name string) (Playbook, error) {
	r, err := s.rs.Get(ctx, Kind, s.scope, name)
	if err != nil {
		if errors.Is(err, resource.ErrNotFound) {
			return Playbook{}, ErrNotFound
		}
		return Playbook{}, err
	}
	return toPlaybook(r)
}

// List returns every playbook, ordered by name.
func (s *Store) List(ctx context.Context) ([]Playbook, error) {
	rs, err := s.rs.List(ctx, Kind, s.scope, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Playbook, 0, len(rs))
	for _, r := range rs {
		p, err := toPlaybook(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func toPlaybook(r resource.Resource) (Playbook, error) {
	spec, err := DecodeSpec(r)
	if err != nil {
		return Playbook{}, err
	}
	return Playbook{Name: r.Name, Spec: spec}, nil
}
