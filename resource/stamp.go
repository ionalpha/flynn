package resource

import (
	"fmt"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/envelope"
	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/spine"
)

// Stamper computes the canonical post-image of a resource mutation: admission,
// identity, the sync envelope, versions, content hash, timestamps, optimistic
// concurrency and the spine event to append. Every backend writes through one
// Stamper, so backends cannot drift on these rules. It does no persistence: the
// caller supplies the existing record, read under its own lock or transaction, and
// persists the result.
type Stamper struct {
	instanceID string
	clk        clock.Clock
	hlc        *hlc.Clock
	gen        *ids.Generator
	reg        *Registry
}

// NewStamper builds a Stamper from a backend's instance identity, clocks, ID
// generator and admission registry.
func NewStamper(instanceID string, clk clock.Clock, hc *hlc.Clock, gen *ids.Generator, reg *Registry) *Stamper {
	return &Stamper{instanceID: instanceID, clk: clk, hlc: hc, gen: gen, reg: reg}
}

// Registry returns the registry this stamper admits against.
func (s *Stamper) Registry() *Registry { return s.reg }

// Put stamps a create or update of r keyed by (Kind, Scope, Name). existing is the
// stored record for that key or nil; it includes tombstones, so a put over one
// resurrects it. Put admits the spec, enforces opt-in CAS and recomputes the hash.
func (s *Stamper) Put(existing *Resource, r Resource) (Resource, spine.AppendInput, error) {
	if r.APIVersion == "" || r.Kind == "" || (r.Name == "" && r.GenerateName == "") {
		return Resource{}, spine.AppendInput{}, fmt.Errorf("%w: resource requires APIVersion, Kind and a Name or GenerateName", ErrInvalid)
	}
	if err := s.reg.Validate(r.APIVersion, r.Kind, r.Spec); err != nil {
		return Resource{}, spine.AppendInput{}, err
	}

	now := s.clk.Now()
	if existing != nil {
		if !envelope.CAS(r.SyncVersion, &existing.Envelope.Envelope) {
			return Resource{}, spine.AppendInput{}, ErrConflict
		}
		r.ID = existing.ID
		r.CreatedAt = existing.CreatedAt
		r.Version = existing.Version + 1
		envelope.StampUpdate(&r.Envelope.Envelope, existing.Envelope.Envelope, s.instanceID, s.hlc.Now())
	} else {
		if !envelope.CAS(r.SyncVersion, nil) {
			return Resource{}, spine.AppendInput{}, ErrConflict
		}
		if r.ID == "" {
			r.ID = s.gen.New()
		}
		// A nameless kind takes its name from the one ID source, so no facade
		// needs its own ID generator.
		if r.Name == "" {
			r.Name = r.GenerateName + r.ID
		}
		r.Version = 1
		envelope.StampCreate(&r.Envelope.Envelope, s.instanceID, s.hlc.Now())
		r.CreatedAt = now
	}
	r.GenerateName = ""                        // consumed: the persisted record is named, never re-generates
	r.WriterActor = writerActor(r.WriterActor) // caller may mark a human write; defaults to the agent
	r.UpdatedAt = now

	// DeletionTimestamp is system-owned and a Put never sets or clears it: keep it
	// from a live record so a finalizer update stays terminating, and clear it on a
	// create or resurrection.
	if existing != nil && !existing.Deleted {
		r.DeletionTimestamp = existing.DeletionTimestamp
	} else {
		r.DeletionTimestamp = nil
	}

	// Removing the last finalizer from a terminating resource tombstones it.
	evType := EvPut
	if r.DeletionTimestamp != nil && len(r.Finalizers) == 0 {
		r.Deleted = true
		evType = EvDeleted
	} else {
		r.Deleted = false
	}

	content, spec, err := Hashes(r)
	if err != nil {
		return Resource{}, spine.AppendInput{}, err
	}
	r.ContentHash = content
	r.SpecHash = spec

	ev, err := s.event(evType, r)
	return r, ev, err
}

// Delete requests deletion of a live resource. Without finalizers it tombstones now
// (EvDeleted). With finalizers it sets DeletionTimestamp and stays live (EvPut) so
// owners can clean up; Put completes the deletion when the last finalizer goes.
func (s *Stamper) Delete(r Resource) (Resource, spine.AppendInput, error) {
	r.Version++
	envelope.StampBump(&r.Envelope.Envelope, s.instanceID, s.hlc.Now())
	r.WriterActor = writerActor(r.WriterActor)
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

	content, spec, err := Hashes(r)
	if err != nil {
		return Resource{}, spine.AppendInput{}, err
	}
	r.ContentHash = content
	r.SpecHash = spec
	ev, err := s.event(evType, r)
	return r, ev, err
}

func (s *Stamper) event(typ string, r Resource) (spine.AppendInput, error) {
	p, err := encodePayload(r)
	if err != nil {
		return spine.AppendInput{}, err
	}
	return envelope.EventInput(ResourceStream, typ, writerActor(r.WriterActor), s.instanceID, p), nil
}

// writerActor defaults the zero actor to the agent, so every record and event names
// an author.
func writerActor(a spine.ActorType) spine.ActorType {
	if a == "" {
		return spine.ActorAgent
	}
	return a
}
