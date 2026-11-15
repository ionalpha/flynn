// Package instance defines the Instance resource kind, the record of one running
// flynn process, so processes are listable like any other kind and placement and
// remote control have a handle to address. The envelope's last-write time is the
// heartbeat.
//
// # Trust
//
// This package does no authentication or authorisation. Anything that can Put
// the kind can write any instance's spec and status, including making a stopped
// runner look alive or a live one look Done. The host must restrict writes of a
// record to that instance's authenticated identity and treat every field as
// self-reported. The size caps ([MaxFieldLen], [MaxRecordBytes]) and the
// fail-closed [IsStale] do not make a record trustworthy.
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

// DefaultStaleAfter is how old a heartbeat may be before a live run-state is
// reported as Unknown, since the process most likely stopped without recording
// Done. It spans several heartbeat intervals so one missed beat does not flap.
const DefaultStaleAfter = 90 * time.Second

const (
	// MaxFieldLen caps the byte length of any string in a spec or status, so a
	// compromised instance cannot use its record to carry a payload or flood a
	// renderer.
	MaxFieldLen = 256
	// MaxListLen caps the entries in Capabilities and Runs.
	MaxListLen = 64
	// MaxRecordBytes caps the summed length of the strings in one record.
	MaxRecordBytes = 24 << 10
)

// MaxClockSkew is how far ahead of the reader's clock a heartbeat may be dated
// and still count as fresh; see [IsStale].
const MaxClockSkew = 30 * time.Second

// State is the coarse run-state the read views report for an instance.
type State string

const (
	// StateIdle is a registered instance with no active run.
	StateIdle State = "Idle"
	// StateWorking is an instance driving one or more runs.
	StateWorking State = "Working"
	// StateBlocked is an instance whose runs all wait on approval or input.
	StateBlocked State = "Blocked"
	// StateDone is an instance that finished its work and is shutting down.
	StateDone State = "Done"
	// StateUnknown is an instance whose state cannot be trusted, such as one
	// with a stale heartbeat.
	StateUnknown State = "Unknown"
)

// Spec is an instance's declared host, version and capabilities. Every field is
// optional; the record name is the instance id.
type Spec struct {
	Host         string   `json:"host,omitempty"`
	Version      string   `json:"version,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Status is written by the live process. Runs lists the run ids it is driving.
// The heartbeat is the envelope write time and has no field here.
type Status struct {
	State              State    `json:"state,omitempty"`
	Runs               []string `json:"runs,omitempty"`
	ObservedGeneration int64    `json:"observedGeneration,omitempty"`
	// OmittedRuns counts the run ids [SetStatus] dropped because the instance
	// reported more than [MaxListLen], or ids longer than [MaxFieldLen]. A reader
	// sees Runs is incomplete instead of the whole beat failing.
	OmittedRuns int `json:"omittedRuns,omitempty"`
}

var specSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "host": {"type": "string", "maxLength": 256},
    "version": {"type": "string", "maxLength": 256},
    "capabilities": {"type": "array", "maxItems": 64, "items": {"type": "string", "maxLength": 256}}
  },
  "additionalProperties": false
}`)

// RegisterKind registers the Instance kind with reg.
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

// Register upserts the Instance record for id with spec, refreshing the
// heartbeat. An existing status is kept, so re-registering on startup never
// clears a live run-state; a new instance starts Idle.
func Register(ctx context.Context, store resource.Store, scope resource.Scope, id string, spec Spec) (resource.Resource, error) {
	if err := validate(id, spec, Status{}); err != nil {
		return resource.Resource{}, err
	}
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
		// The cap covers the merged record, so a spec cannot be sized to overflow
		// around a stored status. An undecodable status counts as empty, so
		// corruption cannot skip the check.
		old, derr := DecodeStatus(existing)
		if derr != nil {
			old = Status{}
		}
		if err := validate(id, spec, old); err != nil {
			return resource.Resource{}, err
		}
	case errors.Is(err, resource.ErrNotFound):
		if r.Status, err = (Status{State: StateIdle}).encode(); err != nil {
			return resource.Resource{}, err
		}
	default:
		return resource.Resource{}, err
	}
	return store.Put(ctx, r)
}

// SetStatus writes the run-state and active runs, keeping the stored spec, and
// refreshes the heartbeat. Run ids past the caps are dropped and counted in
// [Status.OmittedRuns]. The record must already exist.
func SetStatus(ctx context.Context, store resource.Store, scope resource.Scope, id string, state State, runs []string) (resource.Resource, error) {
	st := Status{State: state}
	st.Runs, st.OmittedRuns = capRuns(runs)
	if err := validate(id, Spec{}, st); err != nil {
		return resource.Resource{}, err
	}
	existing, err := store.Get(ctx, Kind, scope, id)
	if err != nil {
		return resource.Resource{}, err
	}
	old, derr := DecodeSpec(existing)
	if derr != nil {
		old = Spec{}
	}
	if err := validate(id, old, st); err != nil {
		return resource.Resource{}, err
	}
	status, err := st.encode()
	if err != nil {
		return resource.Resource{}, err
	}
	existing.Status = status
	return store.Put(ctx, existing)
}

// ErrTooLarge is returned by [Register] and [SetStatus] for a write that exceeds
// [MaxFieldLen], [MaxListLen] or [MaxRecordBytes]. The stored record is left
// unchanged.
var ErrTooLarge = errors.New("instance: field or record exceeds its size cap")

// capRuns keeps the first [MaxListLen] run ids that fit [MaxFieldLen] and counts
// the rest, because a refused beat would read as an unreachable runner.
func capRuns(runs []string) (kept []string, omitted int) {
	for _, id := range runs {
		if len(kept) == MaxListLen || len(id) > MaxFieldLen {
			omitted++
			continue
		}
		kept = append(kept, id)
	}
	return kept, omitted
}

func validate(id string, spec Spec, st Status) error {
	if len(id) > MaxFieldLen || len(spec.Host) > MaxFieldLen || len(spec.Version) > MaxFieldLen ||
		len(spec.Capabilities) > MaxListLen || len(st.Runs) > MaxListLen || len(st.State) > MaxFieldLen {
		return ErrTooLarge
	}
	size := len(id) + len(spec.Host) + len(spec.Version) + len(st.State)
	for _, v := range [][]string{spec.Capabilities, st.Runs} {
		for _, e := range v {
			if len(e) > MaxFieldLen {
				return ErrTooLarge
			}
			size += len(e)
		}
	}
	if size > MaxRecordBytes {
		return ErrTooLarge
	}
	return nil
}

func (s Status) encode() (json.RawMessage, error) { return json.Marshal(s) }

// IsStale reports whether the heartbeat (UpdatedAt) is strictly older than
// staleAfter as of now. A never-written record is stale. A non-positive
// staleAfter means [DefaultStaleAfter], so an unset option cannot make a dead
// runner read as alive.
//
// The store's clock stamps UpdatedAt, so it can run ahead of now. A heartbeat
// more than [MaxClockSkew] in the future is stale, or a stopped or forged
// runner would look fresh until the reader's clock caught up.
func IsStale(r resource.Resource, now time.Time, staleAfter time.Duration) bool {
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}
	if r.UpdatedAt.IsZero() || r.UpdatedAt.Sub(now) > MaxClockSkew {
		return true
	}
	return now.Sub(r.UpdatedAt) > staleAfter
}

// EffectiveState is the run-state every read view (flynn ps and status, the
// dashboard, the remote API) reports, so a crashed process never reads as
// alive: Idle, Working and Blocked become Unknown once the heartbeat is stale.
// Done stays Done at any age because a clean stop writes it last; it is
// self-reported, so a host that needs liveness uses [Unreachable]. A missing,
// unreadable or unrecognised status is Unknown. Time enters only through now,
// so the result is deterministic under replay.
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
		// Never echo an out-of-vocabulary value into the read views.
		return StateUnknown
	}
}
