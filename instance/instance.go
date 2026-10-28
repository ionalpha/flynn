// Package instance defines the Instance resource kind: a record of one running
// flynn process. Promoting a process to a stored resource makes it listable and
// describable through the same read model as every other kind, and gives placement
// and remote control a concrete handle to address. An Instance carries declarative
// spec (its host, version, and the capabilities it offers) and reconciled status
// (its current run-state and the runs it is driving). The resource's own last-write
// time on the envelope is the heartbeat, so a stale record is a missed heartbeat.
//
// # Trust
//
// This package does no authentication and no authorisation. The record name is the
// instance id, and anything that can Put the kind on a store can write any
// instance's spec and status, including making a stopped runner look alive or a
// live one look Done. The host must restrict writes of an Instance record to that
// instance's own authenticated identity (an admission rule or per-instance
// credential on the store), and must treat every field as self-reported by the
// instance. The package does bound what a write can carry (see [MaxFieldLen],
// [MaxRecordBytes]) and keeps liveness derivation fail-closed (see [IsStale]), but
// those limits do not make a record trustworthy.
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

// MaxClockSkew is how far ahead of the reader's clock a heartbeat may be dated and
// still count as fresh. Beyond it the heartbeat is untrustworthy and the record is
// stale; see [IsStale].
const MaxClockSkew = 30 * time.Second

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
		// The record cap covers the merged record, so a spec cannot be sized to
		// overflow around a status already stored.
		// An undecodable stored status counts as empty, so corruption cannot be
		// used to skip the check.
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

// SetStatus writes the instance's reconciled status (its run-state and active runs)
// without changing its spec, refreshing the heartbeat. It preserves the stored spec
// by reading the current record first, so status and spec are written by their
// respective owners and never clobber each other.
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
// the rest. A Reporter that over-reports must not make every beat fail: the beat
// is the liveness signal, and a refused one reads as an unreachable runner.
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

// IsStale reports whether the instance's heartbeat (UpdatedAt, refreshed by every
// Register and SetStatus) is older than staleAfter as of now. A never-written
// record is stale, and the comparison is strict. A non-positive staleAfter is
// replaced by [DefaultStaleAfter] rather than disabling the check: a zero value
// from an unset option must not make a dead runner read as alive.
//
// now is the reader's clock while UpdatedAt is stamped by the store's, so the two
// can differ. A heartbeat dated more than [MaxClockSkew] after now is stale: taken
// at face value its age is negative and a stopped or forged runner would look
// fresh until the reader's clock caught up. Skew inside the tolerance is fresh, so
// ordinary drift does not flap.
func IsStale(r resource.Resource, now time.Time, staleAfter time.Duration) bool {
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}
	if r.UpdatedAt.IsZero() || r.UpdatedAt.Sub(now) > MaxClockSkew {
		return true
	}
	return now.Sub(r.UpdatedAt) > staleAfter
}

// EffectiveState is the run-state to report for an instance right now, derived from
// its recorded status and the age of its heartbeat. It is the single rule the read
// surface (flynn ps/status, the dashboard, the remote API) shares, so a crashed
// process is never reported as alive: a live recorded state (Idle, Working, Blocked)
// becomes Unknown once the heartbeat goes stale. A recorded Done stays Done
// regardless of heartbeat age, because a clean stop writes it before the process
// stops refreshing; it is self-reported, so a host that needs liveness uses
// [Unreachable], which does not exempt Done. A record whose status is
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
