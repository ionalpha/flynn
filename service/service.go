// Package service models a deployed workload as a typed resource on the event-sourced
// foundation. When a hosting extension's deploy operation succeeds, the operator
// surface materializes a Service here: which provider deployed it, what it deploys
// (a static site, a container, a VPS), the external id and URL the provider returned,
// and the state the workload should be driven toward. A deployed app is therefore not
// fire-and-forget. The record is admitted, versioned, and provenance-stamped like
// every other kind, so a reconcile loop can later track its health, restart it, or
// tear it down, and the control plane can read it back.
//
// The secret a deploy used never enters a Service: only the credential's name is
// recorded; the value stays in the vault. The Service is safe to list and sync.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the Service kind's API group and version.
	GroupVersion = "service.ionagent.io/v1alpha1"
	// Kind is the resource kind name services are stored under.
	Kind = "Service"
)

// ErrNotFound is returned when a service does not exist.
var ErrNotFound = errors.New("service: not found")

// Target classifies what a hosting provider deploys, so the operator (and, later, the
// agent) can pick a provider that can satisfy a goal. The set is small on purpose: a
// provider declares the targets it supports, and a deploy records the one it used.
type Target string

const (
	// TargetStaticSite is a static website (e.g. Cloudflare Pages, Vercel static).
	TargetStaticSite Target = "static-site"
	// TargetContainer is a container/PaaS workload (e.g. Render, Cloud Run).
	TargetContainer Target = "container"
	// TargetVPS is a raw virtual machine (e.g. Hetzner, DigitalOcean droplet).
	TargetVPS Target = "vps"
)

// Valid reports whether t is one of the known targets. The empty target is allowed
// (a provider that does not classify its output).
func (t Target) Valid() bool {
	switch t {
	case "", TargetStaticSite, TargetContainer, TargetVPS:
		return true
	default:
		return false
	}
}

// DesiredState is the state a reconcile loop should drive a service toward.
type DesiredState string

const (
	// StateRunning means the workload should be up.
	StateRunning DesiredState = "running"
	// StateStopped means the workload should be retired (a teardown target).
	StateStopped DesiredState = "stopped"
)

// Spec is the desired shape of a deployed workload: pure metadata, no secret.
type Spec struct {
	// Provider is the extension/provider that deployed this workload (e.g. "cloudflare").
	Provider string `json:"provider"`
	// Target is what was deployed (static site, container, VPS). Optional.
	Target Target `json:"target,omitempty"`
	// ExternalID is the provider's own id for the workload (a deployment id, a server
	// id), used to address it on status and teardown. Empty until the provider returns one.
	ExternalID string `json:"externalID,omitempty"`
	// URL is the live address the workload serves at, when the provider returns one.
	URL string `json:"url,omitempty"`
	// DesiredState is the state the reconcile loop should hold the workload in.
	DesiredState DesiredState `json:"desiredState,omitempty"`
	// Credential names the credential used to deploy and supervise this workload, so a
	// later status/teardown resolves the same one. The value stays in the vault.
	Credential string `json:"credential,omitempty"`
}

// Status is a service's observed state, set by the deploy/teardown path and (later) a
// reconcile loop rather than admitted against the spec schema.
type Status struct {
	// Phase is a short lifecycle word: "deployed", "stopped", "failed".
	Phase string `json:"phase,omitempty"`
	// ObservedURL is the last URL a status check saw the workload at.
	ObservedURL string `json:"observedURL,omitempty"`
	// LastDeploy is the RFC3339 time of the last deploy, supplied by the caller's clock
	// (never read from the wall clock here, so the record stays replay-equivalent).
	LastDeploy string `json:"lastDeploy,omitempty"`
}

// Service is the typed view of a service resource.
type Service struct {
	Name    string
	Spec    Spec
	Status  Status
	Version int
}

var specSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "provider": {"type": "string"},
    "target": {"type": "string", "enum": ["", "static-site", "container", "vps"]},
    "externalID": {"type": "string"},
    "url": {"type": "string"},
    "desiredState": {"type": "string", "enum": ["", "running", "stopped"]},
    "credential": {"type": "string"}
  },
  "required": ["provider"],
  "additionalProperties": false
}`)

// KindDef is the Service kind definition registered with a resource registry.
var KindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       Kind,
	Schema:     specSchema,
	Singular:   "service",
	Plural:     "services",
}

// RegisterKind registers the Service kind so a store admits services. It is idempotent.
func RegisterKind(reg *resource.Registry) error { return reg.Register(KindDef) }

// DecodeSpec reads the typed spec from a resource.
func DecodeSpec(r resource.Resource) (Spec, error) {
	var s Spec
	if len(r.Spec) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(r.Spec, &s); err != nil {
		return Spec{}, fmt.Errorf("service: decode spec: %w", err)
	}
	return s, nil
}

// DecodeStatus reads the typed status from a resource.
func DecodeStatus(r resource.Resource) (Status, error) {
	var s Status
	if len(r.Status) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(r.Status, &s); err != nil {
		return Status{}, fmt.Errorf("service: decode status: %w", err)
	}
	return s, nil
}

// Store is the typed service facade over a resource.Store. Services live in the
// instance-global scope, addressed by their name.
type Store struct {
	rs    resource.Store
	scope resource.Scope
}

// NewStore returns a service facade over rs. The caller must have registered the
// Service kind with the registry rs admits against (see RegisterKind).
func NewStore(rs resource.Store) *Store { return &Store{rs: rs} }

// Put creates or updates the named service, writing both its spec and status in one
// record. The deploy path uses it to register a freshly deployed workload.
func (s *Store) Put(ctx context.Context, name string, spec Spec, status Status) (Service, error) {
	if name == "" || spec.Provider == "" {
		return Service{}, fmt.Errorf("service: a service needs a name and a provider")
	}
	rawSpec, err := json.Marshal(spec)
	if err != nil {
		return Service{}, fmt.Errorf("service: encode spec: %w", err)
	}
	rawStatus, err := json.Marshal(status)
	if err != nil {
		return Service{}, fmt.Errorf("service: encode status: %w", err)
	}
	r, err := s.rs.Put(ctx, resource.Resource{
		APIVersion: GroupVersion,
		Kind:       Kind,
		Name:       name,
		Scope:      s.scope,
		Spec:       rawSpec,
		Status:     rawStatus,
	})
	if err != nil {
		return Service{}, err
	}
	return toService(r)
}

// Get returns the named service, or ErrNotFound.
func (s *Store) Get(ctx context.Context, name string) (Service, error) {
	r, err := s.rs.Get(ctx, Kind, s.scope, name)
	if err != nil {
		if errors.Is(err, resource.ErrNotFound) {
			return Service{}, ErrNotFound
		}
		return Service{}, err
	}
	return toService(r)
}

// List returns every service, ordered by name.
func (s *Store) List(ctx context.Context) ([]Service, error) {
	rs, err := s.rs.List(ctx, Kind, s.scope, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Service, 0, len(rs))
	for _, r := range rs {
		svc, err := toService(r)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Delete removes the named service record. It is the bookkeeping half of a teardown:
// the remote workload is removed by the provider's teardown operation, this retires
// the record. A missing service is not an error.
func (s *Store) Delete(ctx context.Context, name string) error {
	err := s.rs.Delete(ctx, Kind, s.scope, name)
	if errors.Is(err, resource.ErrNotFound) {
		return nil
	}
	return err
}

// toService builds the typed view from a stored resource.
func toService(r resource.Resource) (Service, error) {
	spec, err := DecodeSpec(r)
	if err != nil {
		return Service{}, err
	}
	status, err := DecodeStatus(r)
	if err != nil {
		return Service{}, err
	}
	return Service{Name: r.Name, Spec: spec, Status: status, Version: int(r.Version)}, nil
}
