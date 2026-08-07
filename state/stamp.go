package state

import (
	"time"

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

// Now reads the clock this Stamper timestamps writes with. Read paths that need
// the current time - judging MemoryItem.ExpiresAt on a recall - take it from here
// so they measure against the same clock the writes were stamped by, which is what
// makes a manual clock in a test move both sides together.
func (s *Stamper) Now() time.Time { return s.clk.Now() }

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

// WriteMemory stamps a new memory item and returns the event. It rejects a
// malformed anchor with ErrInvalid, the one shape check on this path: a ref
// missing half of itself can never match a recall, so storing it would be storing
// something that is silently dead.
func (s *Stamper) WriteMemory(it MemoryItem) (MemoryItem, spine.AppendInput, error) {
	anchors, err := NormalizeAnchors(it.Anchors)
	if err != nil {
		return MemoryItem{}, spine.AppendInput{}, err
	}
	it.Anchors = anchors
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

// RecordMemoryPush stamps one push of each of memoryIDs and returns the post-image
// rows with the single event that records them. prev holds this instance's stored
// usage row for each item, absent for an item this instance has never touched; the
// caller looks them up under its own lock or transaction, as with every other
// stamp.
//
// A repeated id in one call counts once. The same item cannot be pushed twice by
// one digest, so counting it twice would be recording a caller's slip as an
// observation about the memory.
func (s *Stamper) RecordMemoryPush(prev map[string]MemoryUsage, memoryIDs []string) ([]MemoryUsage, spine.AppendInput, error) {
	now := s.clk.Now()
	hnow := s.hlc.Now()
	seen := make(map[string]bool, len(memoryIDs))
	out := make([]MemoryUsage, 0, len(memoryIDs))
	for _, id := range memoryIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		u := s.stampUsage(prev, id, hnow)
		u.PushCount++
		u.LastPushedAt = now
		out = append(out, u)
	}
	ev, err := s.usageEvent(EvMemoryPushed, out)
	return out, ev, err
}

// RecordMemoryUse stamps one use of an item at the given origin and returns the
// post-image row with its event. prev is this instance's stored row for the item,
// or the zero value if it has none. An origin that is neither organic nor primed
// is ErrInvalid: the split is the measurement, so there is nothing sensible to
// assume on the caller's behalf.
func (s *Stamper) RecordMemoryUse(prev map[string]MemoryUsage, memoryID string, origin UsageOrigin) (MemoryUsage, spine.AppendInput, error) {
	if !origin.Valid() {
		return MemoryUsage{}, spine.AppendInput{}, ErrInvalid
	}
	u := s.stampUsage(prev, memoryID, s.hlc.Now())
	if origin == UsagePrimed {
		u.PrimedUses++
	} else {
		u.OrganicUses++
	}
	u.LastUsedAt = s.clk.Now()
	ev, err := s.usageEvent(EvMemoryUsed, []MemoryUsage{u})
	return u, ev, err
}

// stampUsage returns this instance's usage row for an item with its envelope
// already advanced for the mutation the caller is about to make. A row this
// instance has not written before is created rather than looked up: an instance
// only ever writes its own row (see MemoryUsage), so there is no other writer to
// conflict with and no CAS to enforce.
func (s *Stamper) stampUsage(prev map[string]MemoryUsage, memoryID string, hnow hlc.Time) MemoryUsage {
	u, existed := prev[memoryID]
	if !existed {
		u = MemoryUsage{MemoryID: memoryID, InstanceID: s.instanceID}
		envelope.StampCreate(&u.Envelope, s.instanceID, hnow)
		return u
	}
	u.MemoryID, u.InstanceID = memoryID, s.instanceID
	before := u.Envelope
	envelope.StampUpdate(&u.Envelope, before, s.instanceID, hnow)
	return u
}

func (s *Stamper) usageEvent(typ string, rows []MemoryUsage) (spine.AppendInput, error) {
	return s.input(typ, map[string]any{keyUsage: rows})
}

// RecordMemoryPromotion stamps a reviewer's decision about an item's push
// eligibility and returns the post-image row with its event. prev is the stored
// decision for the item, or nil when nobody has decided yet; the caller looks it up
// under its own lock or transaction, as with every other stamp. A decision missing
// an item or a reviewer is ErrInvalid.
//
// A revision keeps the row and advances its envelope, so the current answer is one
// row per item however many times it changes, while every decision stays on the
// event stream. That split is what makes the audit trail readable: the row is the
// policy in force, the stream is how it got there.
func (s *Stamper) RecordMemoryPromotion(prev *MemoryPromotion, d PromotionDecision) (MemoryPromotion, spine.AppendInput, error) {
	if !d.Valid() {
		return MemoryPromotion{}, spine.AppendInput{}, ErrInvalid
	}
	hnow := s.hlc.Now()
	p := MemoryPromotion{MemoryID: d.MemoryID}
	if prev == nil {
		envelope.StampCreate(&p.Envelope, s.instanceID, hnow)
	} else {
		p.Envelope = prev.Envelope
		envelope.StampUpdate(&p.Envelope, prev.Envelope, s.instanceID, hnow)
	}
	p.Promoted, p.By, p.Reason = d.Promoted, d.By, d.Reason
	p.DecidedAt = s.clk.Now()
	ev, err := s.input(EvMemoryPromoted, map[string]any{keyPromo: p})
	return p, ev, err
}
