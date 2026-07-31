package state

import (
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/envelope"
	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/spine"
)

// Stamper computes the canonical post-image of a state mutation: it assigns the
// record ID, timestamps, the hybrid-logical clock, the content and sync versions,
// and the sync envelope, enforces optimistic concurrency (CAS), and builds the
// spine event to append. Every backend routes its writes through one Stamper, so
// the envelope/version/tombstone rules are defined exactly once and the in-memory
// and durable backends cannot drift (the duplication that previously lived in both
// state/memory.go and the SQLite adapter as Go and as SQL).
//
// A Stamper does no persistence and reads no store: the caller supplies the
// existing record (looked up under its own lock or transaction) and persists the
// returned record and event. This keeps the rules backend-agnostic and pure, so
// the same stamp produces the same record regardless of which backend stores it.
type Stamper struct {
	instanceID string
	clk        clock.Clock
	hlc        *hlc.Clock
	gen        *ids.Generator
}

// NewStamper builds a Stamper from a backend's instance identity and its injected
// deterministic primitives, so a re-run with the same seeds produces identical
// records and IDs (the basis of deterministic replay).
func NewStamper(instanceID string, clk clock.Clock, hc *hlc.Clock, gen *ids.Generator) *Stamper {
	return &Stamper{instanceID: instanceID, clk: clk, hlc: hc, gen: gen}
}

// InstanceID is the instance this Stamper attributes writes to.
func (s *Stamper) InstanceID() string { return s.instanceID }

func (s *Stamper) sessionEvent(typ string, ses Session) (spine.AppendInput, error) {
	return s.input(typ, map[string]any{keySession: ses})
}

func (s *Stamper) skillEvent(typ string, sk Skill) (spine.AppendInput, error) {
	return s.input(typ, map[string]any{keySkill: sk})
}

func (s *Stamper) memoryEvent(typ string, it MemoryItem) (spine.AppendInput, error) {
	return s.input(typ, map[string]any{keyItem: it})
}

// input builds the AppendInput for a state event: always the state stream, the
// agent actor, this instance as origin, and the post-image record(s) serialized
// once as the raw payload.
func (s *Stamper) input(typ string, records map[string]any) (spine.AppendInput, error) {
	p, err := encodePayload(records)
	if err != nil {
		return spine.AppendInput{}, err
	}
	return envelope.EventInput(StateStream, typ, spine.ActorAgent, s.instanceID, p), nil
}

// CreateSession stamps a new session and returns it with the event to append.
func (s *Stamper) CreateSession(ses Session) (Session, spine.AppendInput, error) {
	if ses.ID == "" {
		ses.ID = s.gen.New()
	}
	now := s.clk.Now()
	if ses.CreatedAt.IsZero() {
		ses.CreatedAt = now
	}
	ses.UpdatedAt = now
	envelope.StampCreate(&ses.Envelope, s.instanceID, s.hlc.Now())
	ev, err := s.sessionEvent(EvSessionCreated, ses)
	return ses, ev, err
}

// AppendTurn stamps a turn at nextSeq and the envelope bump it induces on its
// session, returning both plus the single event that records the pair. The caller
// supplies the live session (so its envelope advances under the same HLC) and the
// next sequence number from its own store.
func (s *Stamper) AppendTurn(ses Session, t Turn, nextSeq int64) (Turn, Session, spine.AppendInput, error) {
	if t.ID == "" {
		t.ID = s.gen.New()
	}
	t.SessionID = ses.ID
	t.Seq = nextSeq
	now := s.clk.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	hnow := s.hlc.Now()
	envelope.StampCreate(&t.Envelope, s.instanceID, hnow)

	// Appending a turn mutates the session: bump its envelope under the same HLC.
	ses.UpdatedAt = t.CreatedAt
	envelope.StampBump(&ses.Envelope, s.instanceID, hnow)

	ev, err := s.input(EvTurnAppended, map[string]any{keyTurn: t, keySession: ses})
	if err != nil {
		return Turn{}, Session{}, spine.AppendInput{}, err
	}
	return t, ses, ev, nil
}

// DeleteSession tombstones the given live session and returns the event.
func (s *Stamper) DeleteSession(ses Session) (Session, spine.AppendInput, error) {
	envelope.StampTombstone(&ses.Envelope, s.instanceID, s.hlc.Now())
	ses.UpdatedAt = s.clk.Now()
	ev, err := s.sessionEvent(EvSessionDeleted, ses)
	return ses, ev, err
}

// UpsertSkill stamps a create or an update keyed by (Scope, Slug). existing is the
// stored record for that key (tombstones included, so an upsert over a tombstone
// resurrects it) or nil if none. Optimistic concurrency is opt-in: a non-zero
// SyncVersion on the input must match the stored one, else ErrConflict; a non-zero
// SyncVersion with no existing record is also a conflict (it expected one).
func (s *Stamper) UpsertSkill(existing *Skill, sk Skill) (Skill, spine.AppendInput, error) {
	now := s.clk.Now()
	if existing != nil {
		if !envelope.CAS(sk.SyncVersion, &existing.Envelope) {
			return Skill{}, spine.AppendInput{}, ErrConflict
		}
		sk.ID = existing.ID
		sk.CreatedAt = existing.CreatedAt
		sk.Version = existing.Version + 1
		// Deleted comes from sk: a normal upsert (Deleted false) over a tombstone
		// resurrects it; the projection reindexes accordingly.
		envelope.StampUpdate(&sk.Envelope, existing.Envelope, s.instanceID, s.hlc.Now())
		sk.UpdatedAt = now
		ev, err := s.skillEvent(EvSkillUpserted, sk)
		return sk, ev, err
	}

	if !envelope.CAS(sk.SyncVersion, nil) {
		return Skill{}, spine.AppendInput{}, ErrConflict
	}
	if sk.ID == "" {
		sk.ID = s.gen.New()
	}
	if sk.Version == 0 {
		sk.Version = 1
	}
	envelope.StampCreate(&sk.Envelope, s.instanceID, s.hlc.Now())
	sk.CreatedAt = now
	sk.UpdatedAt = now
	ev, err := s.skillEvent(EvSkillUpserted, sk)
	return sk, ev, err
}

// DeleteSkill tombstones the given live skill (bumping the content version too,
// so a delete is itself a revision) and returns the event.
func (s *Stamper) DeleteSkill(sk Skill) (Skill, spine.AppendInput, error) {
	sk.Version++
	envelope.StampTombstone(&sk.Envelope, s.instanceID, s.hlc.Now())
	sk.UpdatedAt = s.clk.Now()
	ev, err := s.skillEvent(EvSkillDeleted, sk)
	return sk, ev, err
}

// WriteMemory stamps a new memory item and returns the event.
func (s *Stamper) WriteMemory(it MemoryItem) (MemoryItem, spine.AppendInput, error) {
	if it.ID == "" {
		it.ID = s.gen.New()
	}
	if it.CreatedAt.IsZero() {
		it.CreatedAt = s.clk.Now()
	}
	// Score grades a match against one query; it is not a property of the record.
	// Clearing it here covers every backend that stamps through this path, so a
	// recalled item handed straight back to Write cannot persist a stale score.
	it.Score = 0
	envelope.StampCreate(&it.Envelope, s.instanceID, s.hlc.Now())
	ev, err := s.memoryEvent(EvMemoryWritten, it)
	return it, ev, err
}

// DeleteMemory tombstones the given live memory item and returns the event.
func (s *Stamper) DeleteMemory(it MemoryItem) (MemoryItem, spine.AppendInput, error) {
	envelope.StampTombstone(&it.Envelope, s.instanceID, s.hlc.Now())
	ev, err := s.memoryEvent(EvMemoryDeleted, it)
	return it, ev, err
}
