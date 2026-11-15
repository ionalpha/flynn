// Package playbook stores and runs multi-step operational procedures: provision the
// CLI tools needed, run them, check the outcome, and register the result as a
// supervised service. An integration calls one API operation; a playbook composes
// many steps toward a goal.
//
// A playbook is a declarative flow, so the stored spec is what runs and a
// runtime-authored playbook cannot run arbitrary code: every effect goes through a
// port the [Runner] wires.
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

// ServiceBlock is the static classification of the service a playbook registers.
// Name, URL, external id and address come from the flow's return value, so all
// templating stays inside the flow.
type ServiceBlock struct {
	// Provider owns the workload, for example "fly".
	Provider string `json:"provider"`
	// Target is static-site, container or vps. Optional.
	Target service.Target `json:"target,omitempty"`
}

// Spec is a playbook's stored definition.
type Spec struct {
	Description string `json:"description,omitempty"`
	// Inputs is a JSON Schema for the run's config. It is not enforced: the runner
	// passes the config to the flow as given.
	Inputs json.RawMessage `json:"inputs,omitempty"`
	Flow   json.RawMessage `json:"flow"`
	// Service, when set, is registered from the flow's result on success.
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

// KindDef is the Playbook resource kind.
var KindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       Kind,
	Schema:     specSchema,
	Singular:   "playbook",
	Plural:     "playbooks",
}

// RegisterKind registers [KindDef] with reg. It is idempotent.
func RegisterKind(reg *resource.Registry) error { return reg.Register(KindDef) }

// DecodeSpec reads the typed spec from a resource.
func DecodeSpec(r resource.Resource) (Spec, error) {
	s, err := resource.DecodeSpec[Spec](r)
	if err != nil {
		return Spec{}, fmt.Errorf("playbook: decode spec: %w", err)
	}
	return s, nil
}

// DecodeFlow parses and validates the flow; the build-time gate calls it on every
// bundled playbook.
func (s Spec) DecodeFlow() (flow.Flow, error) { return flow.Decode(s.Flow) }

// Playbook is the typed view of a playbook resource.
type Playbook struct {
	Name string
	Spec Spec
}

// Store keeps playbooks in the instance-global scope of a resource store, by name.
type Store struct {
	rs    resource.Store
	scope resource.Scope
}

// NewStore returns a Store over rs. The Playbook kind must already be registered
// with rs's registry ([RegisterKind]).
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
