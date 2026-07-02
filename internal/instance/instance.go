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
	"time"

	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the Instance kind's API group and version.
	GroupVersion = "instance.ionagent.io/v1alpha1"
	// Kind is the resource kind name.
	Kind = "Instance"
)

// DefaultStaleAfter is how long an instance's heartbeat (its last envelope write)
// may age before its live run-state can no longer be trusted and is reported as
// Unknown. A healthy process refreshes its record well within this window;
// exceeding it means the process most likely stopped without recording a terminal
// state, so reporting its last live state as current would be a lie. The value is
// several heartbeat intervals, so a single missed refresh does not flap the state.
const DefaultStaleAfter = 90 * time.Second

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
func DecodeSpec(r resource.Resource) (Spec, error) { return resource.DecodeSpec[Spec](r) }

// DecodeStatus reads the typed status from a resource.
func DecodeStatus(r resource.Resource) (Status, error) { return resource.DecodeStatus[Status](r) }

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

// IsStale reports whether the instance's heartbeat is older than staleAfter as of
// now. The heartbeat is the resource's last envelope write time (UpdatedAt): a live
// process refreshes it on every Register and SetStatus, so a record that has not
// moved is a process that has stopped writing. A non-positive staleAfter disables
// the check (nothing is ever stale), and a record that was never written (zero
// UpdatedAt) is treated as stale, because there is no heartbeat to trust. The
// comparison is strict, so a heartbeat exactly staleAfter old is not yet stale.
func IsStale(r resource.Resource, now time.Time, staleAfter time.Duration) bool {
	if staleAfter <= 0 {
		return false
	}
	if r.UpdatedAt.IsZero() {
		return true
	}
	return now.Sub(r.UpdatedAt) > staleAfter
}

// EffectiveState is the run-state to report for an instance right now, derived from
// its recorded status and the age of its heartbeat. It is the single rule the read
// surface (flynn ps/status, the dashboard, the remote API) shares, so a crashed
// process is never reported as alive: a live recorded state (Idle, Working, Blocked)
// becomes Unknown once the heartbeat goes stale. A cleanly finished instance stays
// Done regardless of heartbeat age, because it recorded its own terminal state
// before shutting down and is expected to stop refreshing. A record whose status is
// missing or unreadable reports Unknown rather than guessing. The function is pure
// (time enters only through now), so it is deterministic under replay and the same
// result on every machine.
func EffectiveState(r resource.Resource, now time.Time, staleAfter time.Duration) State {
	st, err := DecodeStatus(r)
	if err != nil {
		return StateUnknown
	}
	switch st.State {
	case StateDone, StateUnknown:
		return st.State
	case StateIdle, StateWorking, StateBlocked:
		if IsStale(r, now, staleAfter) {
			return StateUnknown
		}
		return st.State
	default:
		// Empty, or any value outside the vocabulary (a malformed or adversarial
		// record): the state cannot be trusted, so report Unknown rather than echo
		// an out-of-band value into the read surface.
		return StateUnknown
	}
}
