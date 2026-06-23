package state

import (
	"github.com/ionalpha/flynn/clock"
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
	p, err := encodeRecord(ses)
	if err != nil {
		return spine.AppendInput{}, err
	}
	return s.input(typ, map[string]any{keySession: p}), nil
}

func (s *Stamper) skillEvent(typ string, sk Skill) (spine.AppendInput, error) {
	p, err := encodeRecord(sk)
	if err != nil {
		return spine.AppendInput{}, err
	}
	return s.input(typ, map[string]any{keySkill: p}), nil
}

func (s *Stamper) memoryEvent(typ string, it MemoryItem) (spine.AppendInput, error) {
	p, err := encodeRecord(it)
	if err != nil {
		return spine.AppendInput{}, err
	}
	return s.input(typ, map[string]any{keyItem: p}), nil
}

// input builds the AppendInput for a state event: always the state stream, the
// agent actor, and this instance as origin.
func (s *Stamper) input(typ string, payload map[string]any) spine.AppendInput {
	return spine.AppendInput{
		Stream:           StateStream,
		Type:             typ,
		Actor:            spine.ActorAgent,
		Payload:          payload,
		OriginInstanceID: s.instanceID,
	}
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
	if ses.OriginInstanceID == "" {
		ses.OriginInstanceID = s.instanceID
	}
	ses.LastWriterID = s.instanceID
	ses.UpdatedHLC = s.hlc.Now()
	ses.SyncVersion = 1
	ses.Deleted = false
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
	if t.OriginInstanceID == "" {
		t.OriginInstanceID = s.instanceID
	}
	hnow := s.hlc.Now()
	t.LastWriterID = s.instanceID
	t.UpdatedHLC = hnow
	t.SyncVersion = 1
	t.Deleted = false

	// Appending a turn mutates the session: bump its envelope under the same HLC.
	ses.UpdatedAt = t.CreatedAt
	ses.LastWriterID = s.instanceID
	ses.UpdatedHLC = hnow
	ses.SyncVersion++

	turnPayload, err := encodeRecord(t)
	if err != nil {
		return Turn{}, Session{}, spine.AppendInput{}, err
	}
	sessionPayload, err := encodeRecord(ses)
	if err != nil {
		return Turn{}, Session{}, spine.AppendInput{}, err
	}
	ev := s.input(EvTurnAppended, map[string]any{keyTurn: turnPayload, keySession: sessionPayload})
	return t, ses, ev, nil
}

// DeleteSession tombstones the given live session and returns the event.
func (s *Stamper) DeleteSession(ses Session) (Session, spine.AppendInput, error) {
	ses.Deleted = true
	ses.LastWriterID = s.instanceID
	ses.UpdatedHLC = s.hlc.Now()
	ses.UpdatedAt = s.clk.Now()
	ses.SyncVersion++
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
		if sk.SyncVersion != 0 && sk.SyncVersion != existing.SyncVersion {
			return Skill{}, spine.AppendInput{}, ErrConflict
		}
		sk.ID = existing.ID
		sk.CreatedAt = existing.CreatedAt
		sk.OriginInstanceID = existing.OriginInstanceID // origin is preserved
		sk.Version = existing.Version + 1
		sk.SyncVersion = existing.SyncVersion + 1
		sk.LastWriterID = s.instanceID
		sk.UpdatedHLC = s.hlc.Now()
		sk.UpdatedAt = now
		// Deleted comes from sk: a normal upsert (Deleted false) over a tombstone
		// resurrects it; the projection reindexes accordingly.
		ev, err := s.skillEvent(EvSkillUpserted, sk)
		return sk, ev, err
	}

	if sk.SyncVersion != 0 {
		return Skill{}, spine.AppendInput{}, ErrConflict
	}
	if sk.ID == "" {
		sk.ID = s.gen.New()
	}
	if sk.Version == 0 {
		sk.Version = 1
	}
	sk.SyncVersion = 1
	if sk.OriginInstanceID == "" {
		sk.OriginInstanceID = s.instanceID
	}
	sk.LastWriterID = s.instanceID
	sk.UpdatedHLC = s.hlc.Now()
	sk.CreatedAt = now
	sk.UpdatedAt = now
	ev, err := s.skillEvent(EvSkillUpserted, sk)
	return sk, ev, err
}

// DeleteSkill tombstones the given live skill (bumping the content version too,
// so a delete is itself a revision) and returns the event.
func (s *Stamper) DeleteSkill(sk Skill) (Skill, spine.AppendInput, error) {
	sk.Deleted = true
	sk.Version++
	sk.SyncVersion++
	sk.LastWriterID = s.instanceID
	sk.UpdatedHLC = s.hlc.Now()
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
	if it.OriginInstanceID == "" {
		it.OriginInstanceID = s.instanceID
	}
	it.LastWriterID = s.instanceID
	it.UpdatedHLC = s.hlc.Now()
	it.SyncVersion = 1
	it.Deleted = false
	ev, err := s.memoryEvent(EvMemoryWritten, it)
	return it, ev, err
}

// DeleteMemory tombstones the given live memory item and returns the event.
func (s *Stamper) DeleteMemory(it MemoryItem) (MemoryItem, spine.AppendInput, error) {
	it.Deleted = true
	it.LastWriterID = s.instanceID
	it.UpdatedHLC = s.hlc.Now()
	it.SyncVersion++
	ev, err := s.memoryEvent(EvMemoryDeleted, it)
	return it, ev, err
}
