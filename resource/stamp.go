package resource

import (
	"fmt"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/spine"
)

// Stamper computes the canonical post-image of a resource mutation: it runs
// admission (the kind must be registered and the Spec must satisfy its schema),
// assigns identity, the sync envelope, content and sync versions, the content
// hash, and timestamps, enforces optimistic concurrency, and builds the spine
// event to append. Every backend routes writes through one Stamper, so the rules
// live in exactly one place and backends cannot drift. It does no persistence: the
// caller supplies the existing record (looked up under its own lock/transaction)
// and persists the returned resource and event.
type Stamper struct {
	instanceID string
	clk        clock.Clock
	hlc        *hlc.Clock
	gen        *ids.Generator
	reg        *Registry
}

// NewStamper builds a Stamper from a backend's instance identity, deterministic
// primitives, and the kind registry used for admission.
func NewStamper(instanceID string, clk clock.Clock, hc *hlc.Clock, gen *ids.Generator, reg *Registry) *Stamper {
	return &Stamper{instanceID: instanceID, clk: clk, hlc: hc, gen: gen, reg: reg}
}

// Registry returns the registry this stamper admits against.
func (s *Stamper) Registry() *Registry { return s.reg }

// Put stamps a create or update of r keyed by (Kind, Scope, Name). existing is the
// stored record for that key (tombstones included, so a put over a tombstone
// resurrects it) or nil. It admits the spec, enforces opt-in CAS, and recomputes
// the content hash, returning the canonical resource and the event to append.
func (s *Stamper) Put(existing *Resource, r Resource) (Resource, spine.AppendInput, error) {
	if r.APIVersion == "" || r.Kind == "" || (r.Name == "" && r.GenerateName == "") {
		return Resource{}, spine.AppendInput{}, fmt.Errorf("%w: resource requires APIVersion, Kind and a Name or GenerateName", ErrInvalid)
	}
	if err := s.reg.Validate(r.APIVersion, r.Kind, r.Spec); err != nil {
		return Resource{}, spine.AppendInput{}, err
	}

	now := s.clk.Now()
	if existing != nil {
		if r.SyncVersion != 0 && r.SyncVersion != existing.SyncVersion {
			return Resource{}, spine.AppendInput{}, ErrConflict
		}
		r.ID = existing.ID
		r.CreatedAt = existing.CreatedAt
		r.OriginInstanceID = existing.OriginInstanceID // origin preserved
		r.Version = existing.Version + 1
		r.SyncVersion = existing.SyncVersion + 1
	} else {
		if r.SyncVersion != 0 {
			return Resource{}, spine.AppendInput{}, ErrConflict
		}
		if r.ID == "" {
			r.ID = s.gen.New()
		}
		// A nameless kind gets a server-assigned name from the one ID source, so
		// the record is addressable like any other without a facade inventing its
		// own ID generator.
		if r.Name == "" {
			r.Name = r.GenerateName + r.ID
		}
		r.Version = 1
		r.SyncVersion = 1
		if r.OriginInstanceID == "" {
			r.OriginInstanceID = s.instanceID
		}
		r.CreatedAt = now
	}
	r.GenerateName = "" // consumed: the persisted record is named, never re-generates
	r.LastWriterID = s.instanceID
	r.WriterActor = writerActor(r.WriterActor) // caller may mark a human write; defaults to the agent
	r.UpdatedHLC = s.hlc.Now()
	r.UpdatedAt = now

	// DeletionTimestamp is system-owned: a Put never sets or clears it. Preserve it
	// from a live record so a finalizer update keeps the resource terminating;
	// clear it on a create or on a resurrection over a tombstone (a fresh resource
	// is not being deleted).
	if existing != nil && !existing.Deleted {
		r.DeletionTimestamp = existing.DeletionTimestamp
	} else {
		r.DeletionTimestamp = nil
	}

	// Removing the last finalizer from a terminating resource completes its
	// deletion (the record tombstones); otherwise the put leaves it live.
	evType := EvPut
	if r.DeletionTimestamp != nil && len(r.Finalizers) == 0 {
		r.Deleted = true
		evType = EvDeleted
	} else {
		r.Deleted = false
	}

	hash, err := Hash(r)
	if err != nil {
		return Resource{}, spine.AppendInput{}, err
	}
	r.ContentHash = hash

	ev, err := s.event(evType, r)
	return r, ev, err
}

// Delete requests deletion of the given live resource. If it has no finalizers it
// tombstones immediately (EvDeleted). If it has finalizers it is marked
// terminating instead: DeletionTimestamp is set and the record stays live (EvPut)
// so its owners can run cleanup and remove their finalizer keys; the deletion
// completes later, in Put, when the last finalizer is removed.
func (s *Stamper) Delete(r Resource) (Resource, spine.AppendInput, error) {
	r.Version++
	r.SyncVersion++
	r.LastWriterID = s.instanceID
	r.WriterActor = writerActor(r.WriterActor)
	r.UpdatedHLC = s.hlc.Now()
	r.UpdatedAt = s.clk.Now()

	evType := EvDeleted
	if len(r.Finalizers) > 0 {
		if r.DeletionTimestamp == nil {
			ts := s.clk.Now()
			r.DeletionTimestamp = &ts
		}
		r.Deleted = false
		evType = EvPut
	} else {
		r.Deleted = true
	}

	hash, err := Hash(r)
	if err != nil {
		return Resource{}, spine.AppendInput{}, err
	}
	r.ContentHash = hash
	ev, err := s.event(evType, r)
	return r, ev, err
}

func (s *Stamper) event(typ string, r Resource) (spine.AppendInput, error) {
	p, err := encodeResource(r)
	if err != nil {
		return spine.AppendInput{}, err
	}
	return spine.AppendInput{
		Stream:           ResourceStream,
		Type:             typ,
		Actor:            writerActor(r.WriterActor),
		Payload:          map[string]any{payloadKey: p},
		OriginInstanceID: s.instanceID,
	}, nil
}

// writerActor normalizes a provenance actor, defaulting the zero value to the
// agent so every record and event carries a concrete authorship signal.
func writerActor(a spine.ActorType) spine.ActorType {
	if a == "" {
		return spine.ActorAgent
	}
	return a
}
