package state

import (
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/envelope"
	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/spine"
)

// Stamper computes the post-image of a state mutation (ID, timestamps, HLC,
// versions, envelope), enforces CAS, and builds the spine event. Every backend
// writes through one, so the in-memory and durable backends cannot drift.
//
// It neither reads nor persists: the caller passes the existing record, read
// under its own lock or transaction, and stores what comes back.
type Stamper struct {
	instanceID string
	clk        clock.Clock
	hlc        *hlc.Clock
	gen        *ids.Generator
}

// NewStamper builds a Stamper. With the same seeds a re-run produces identical
// records and IDs, which deterministic replay relies on.
func NewStamper(instanceID string, clk clock.Clock, hc *hlc.Clock, gen *ids.Generator) *Stamper {
	return &Stamper{instanceID: instanceID, clk: clk, hlc: hc, gen: gen}
}

// InstanceID is the instance writes are attributed to.
func (s *Stamper) InstanceID() string { return s.instanceID }

// Now reads the write clock. Read paths that judge MemoryItem.ExpiresAt use
// it, so a manual clock in a test moves reads and writes together.
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

func (s *Stamper) input(typ string, records map[string]any) (spine.AppendInput, error) {
	p, err := encodePayload(records)
	if err != nil {
		return spine.AppendInput{}, err
	}
	return envelope.EventInput(StateStream, typ, spine.ActorAgent, s.instanceID, p), nil
}

// CreateSession stamps a new session and its event.
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

// AppendTurn stamps a turn at nextSeq and bumps the live session's envelope
// under the same HLC; one event records both.
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

	ses.UpdatedAt = t.CreatedAt
	envelope.StampBump(&ses.Envelope, s.instanceID, hnow)

	ev, err := s.input(EvTurnAppended, map[string]any{keyTurn: t, keySession: ses})
	if err != nil {
		return Turn{}, Session{}, spine.AppendInput{}, err
	}
	return t, ses, ev, nil
}

// DeleteSession tombstones a live session.
func (s *Stamper) DeleteSession(ses Session) (Session, spine.AppendInput, error) {
	envelope.StampTombstone(&ses.Envelope, s.instanceID, s.hlc.Now())
	ses.UpdatedAt = s.clk.Now()
	ev, err := s.sessionEvent(EvSessionDeleted, ses)
	return ses, ev, err
}

// UpsertSkill stamps a create or update keyed by (Scope, Slug). existing is
// the stored record, tombstones included (an upsert resurrects one), or nil.
// CAS is opt-in: a non-zero SyncVersion must match the stored one, and with no
// existing record it is ErrConflict.
func (s *Stamper) UpsertSkill(existing *Skill, sk Skill) (Skill, spine.AppendInput, error) {
	now := s.clk.Now()
	if existing != nil {
		if !envelope.CAS(sk.SyncVersion, &existing.Envelope) {
			return Skill{}, spine.AppendInput{}, ErrConflict
		}
		sk.ID = existing.ID
		sk.CreatedAt = existing.CreatedAt
		sk.Version = existing.Version + 1
		// Deleted comes from sk, so an upsert over a tombstone resurrects it.
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

// DeleteSkill tombstones a live skill and bumps Version, so a delete is a
// revision.
func (s *Stamper) DeleteSkill(sk Skill) (Skill, spine.AppendInput, error) {
	sk.Version++
	envelope.StampTombstone(&sk.Envelope, s.instanceID, s.hlc.Now())
	sk.UpdatedAt = s.clk.Now()
	ev, err := s.skillEvent(EvSkillDeleted, sk)
	return sk, ev, err
}

// WriteMemory stamps a new memory item. It canonicalizes anchors, subject and
// supersession here and nowhere else, so every backend stores the same bytes.
// A value no read could match is ErrInvalid: a half-empty anchor, an
// all-punctuation subject, or an item superseding itself.
func (s *Stamper) WriteMemory(it MemoryItem) (MemoryItem, spine.AppendInput, error) {
	anchors, err := NormalizeAnchors(it.Anchors)
	if err != nil {
		return MemoryItem{}, spine.AppendInput{}, err
	}
	it.Anchors = anchors
	subject, err := NormalizeSubject(it.Subject)
	if err != nil {
		return MemoryItem{}, spine.AppendInput{}, err
	}
	it.Subject = subject
	if it.ID == "" {
		it.ID = s.gen.New()
	}
	// Checked after the id is assigned, so a caller-supplied id is held to the
	// no-self-loop rule too.
	supersedes, err := NormalizeSupersedes(it.Supersedes, it.ID)
	if err != nil {
		return MemoryItem{}, spine.AppendInput{}, err
	}
	it.Supersedes = supersedes
	if it.CreatedAt.IsZero() {
		it.CreatedAt = s.clk.Now()
	}
	// Score belongs to one query, so a recalled item written back must not
	// persist it.
	it.Score = 0
	envelope.StampCreate(&it.Envelope, s.instanceID, s.hlc.Now())
	ev, err := s.memoryEvent(EvMemoryWritten, it)
	return it, ev, err
}

// DeleteMemory tombstones a live memory item.
func (s *Stamper) DeleteMemory(it MemoryItem) (MemoryItem, spine.AppendInput, error) {
	envelope.StampTombstone(&it.Envelope, s.instanceID, s.hlc.Now())
	ev, err := s.memoryEvent(EvMemoryDeleted, it)
	return it, ev, err
}

// RecordMemoryPush stamps one push of each id, under one event. prev holds
// this instance's existing rows. A repeated id counts once, since one digest
// cannot push an item twice.
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

// RecordMemoryUse stamps one use at origin. prev holds this instance's
// existing rows. An invalid origin is ErrInvalid; there is no default.
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

// stampUsage returns this instance's row for an item with its envelope
// advanced. There is no CAS, because only this instance writes the row.
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

// RecordMemoryPromotion stamps a reviewer's decision. prev is the stored one,
// or nil. A decision missing an item or reviewer is ErrInvalid. A revision
// advances the same row, so the row is the policy in force and the stream
// keeps every decision.
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
