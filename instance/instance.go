// Package instance defines the Instance resource kind: a record of one running
// flynn process. Promoting a process to a stored resource makes it listable and
// describable through the same read model as every other kind, and gives placement
// and remote control a concrete handle to address. An Instance carries declarative
// spec (its host, version, and the capabilities it offers) and reconciled status
// (its current run-state and the runs it is driving). The status is written by the
// live process, never by a user, and the resource's own last-write time on the
// envelope is the heartbeat, so a stale record is a missed heartbeat.
package instance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the Instance kind's API group and version.
	GroupVersion = "instance.ionagent.io/v1alpha1"
	// Kind is the resource kind name.
	Kind = "Instance"
)

// State is the coarse run-state of an instance: the vocabulary the read surface
// reports for what a process is doing right now.
type State string

const (
	// StateIdle is a registered instance with no active run.
	StateIdle State = "Idle"
	// StateWorking is an instance driving one or more runs.
	StateWorking State = "Working"
	// StateBlocked is an instance whose runs are all waiting (on approval or input).
	StateBlocked State = "Blocked"
	// StateDone is an instance that has finished its work and is shutting down.
	StateDone State = "Done"
	// StateUnknown is an instance whose state cannot be determined (for example a
	// heartbeat too old to trust).
	StateUnknown State = "Unknown"
)

// Spec is an instance's declared shape: where it runs, what version it is, and the
// capabilities it can offer work. Every field is optional, so a minimal Instance is
// just its name (the instance id).
type Spec struct {
	Host         string   `json:"host,omitempty"`
	Version      string   `json:"version,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Status is an instance's observed state, written by the live process. Runs lists
// the run ids the instance is currently driving; the heartbeat is the resource's
// envelope write time, not a field here.
type Status struct {
	State              State    `json:"state,omitempty"`
	Runs               []string `json:"runs,omitempty"`
	ObservedGeneration int64    `json:"observedGeneration,omitempty"`
}

var specSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "host": {"type": "string"},
    "version": {"type": "string"},
    "capabilities": {"type": "array", "items": {"type": "string"}}
  },
  "additionalProperties": false
}`)

// RegisterKind registers the Instance kind so instances can be stored and admitted
// like any other resource.
func RegisterKind(reg *resource.Registry) error {
	return reg.Register(resource.Kind{
		APIVersion: GroupVersion,
		Name:       Kind,
		Schema:     specSchema,
		Singular:   "instance",
		Plural:     "instances",
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

// Register upserts this process's Instance resource by id, recording its declared
// spec and refreshing its heartbeat (the envelope write time). An existing record's
// status is preserved, so re-registering on startup never clears a live run-state;
// a brand new instance starts Idle. It returns the stored resource.
func Register(ctx context.Context, store resource.Store, scope resource.Scope, id string, spec Spec) (resource.Resource, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return resource.Resource{}, err
	}
	r := resource.Resource{
		APIVersion: GroupVersion,
		Kind:       Kind,
		Name:       id,
		Scope:      scope,
		Spec:       specJSON,
	}
	switch existing, err := store.Get(ctx, Kind, scope, id); {
	case err == nil:
		r.Status = existing.Status
	case errors.Is(err, resource.ErrNotFound):
		if r.Status, err = (Status{State: StateIdle}).encode(); err != nil {
			return resource.Resource{}, err
		}
	default:
		return resource.Resource{}, err
	}
	return store.Put(ctx, r)
}

// SetStatus writes the instance's reconciled status (its run-state and active runs)
// without changing its spec, refreshing the heartbeat. It preserves the stored spec
// by reading the current record first, so status and spec are written by their
// respective owners and never clobber each other.
func SetStatus(ctx context.Context, store resource.Store, scope resource.Scope, id string, state State, runs []string) (resource.Resource, error) {
	existing, err := store.Get(ctx, Kind, scope, id)
	if err != nil {
		return resource.Resource{}, err
	}
	status, err := (Status{State: state, Runs: runs}).encode()
	if err != nil {
		return resource.Resource{}, err
	}
	existing.Status = status
	return store.Put(ctx, existing)
}

func (s Status) encode() (json.RawMessage, error) { return json.Marshal(s) }
