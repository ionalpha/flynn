package state

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ionalpha/flynn/spine"
)

// StateStream is the spine stream every state mutation is recorded on. A single
// ordered stream for sessions, skills, and memory means a Replay folds one log
// to reconstruct the whole provider. It is exported so a host can observe or
// audit the state stream directly.
const StateStream = "state"

// State event types: the vocabulary of the state stream. Each event is the
// post-image of the affected record(s): the Stamper computes the canonical record
// (IDs, Seq, HLC, version, timestamps all assigned) and it is written as the event
// payload, so replaying the events in Seq order reproduces identical state without
// re-running any clock or RNG. Exported so durable backends project the same
// vocabulary rather than redeclaring the strings.
const (
	EvSessionCreated = "session.created"
	EvTurnAppended   = "session.turn_appended"
	EvSessionDeleted = "session.deleted"
	EvSkillUpserted  = "skill.upserted"
	EvSkillDeleted   = "skill.deleted"
	EvMemoryWritten  = "memory.written"
	EvMemoryDeleted  = "memory.deleted"
	EvMemoryPushed   = "memory.pushed"
	EvMemoryUsed     = "memory.used"
	EvMemoryPromoted = "memory.promoted"
)

// Payload keys under which a state event carries its post-image record(s).
const (
	keySession = "session"
	keyTurn    = "turn"
	keySkill   = "skill"
	keyItem    = "item"
	keyUsage   = "usage"
	keyPromo   = "promotion"
)

// core is the in-memory read model behind the command path. Every mutation
// appends an event to the log and then projects it onto these maps under mu, so
// the log and the projection never diverge; reads take mu and read the maps.
//
// The invariant that keeps full event-sourcing reachable: no state mutation
// bypasses the log. apply is the only function that mutates the maps, and it is
// reached only from record (the live write path) and Replay (reconstruction) -
// there are no raw map writes in the store methods.
type core struct {
	st  *Stamper
	log spine.Log

	mu      sync.Mutex
	lastSeq int64 // the highest event Seq this projection has applied

	sessions   map[string]Session
	turns      map[string][]Turn
	skillsByID map[string]Skill
	slugToID   map[string]string // scopeKey+"\x00"+slug -> skill id
	memItems   []MemoryItem
	memUsage   map[string]MemoryUsage     // usageKey(memoryID, instanceID) -> usage row
	memPromo   map[string]MemoryPromotion // memoryID -> the current push decision

	// snapCodec seals a snapshot before it is saved and verifies it before restore;
	// snapEvery/snapPending drive the automatic snapshot cadence. They are set from the
	// provider's options after the core is built, and read only under mu.
	snapCodec   spine.SnapshotCodec
	snapEvery   int
	snapPending int
}

func newCore(st *Stamper, log spine.Log) *core {
	return &core{
		st:         st,
		log:        log,
		sessions:   map[string]Session{},
		turns:      map[string][]Turn{},
		skillsByID: map[string]Skill{},
		slugToID:   map[string]string{},
		memUsage:   map[string]MemoryUsage{},
		memPromo:   map[string]MemoryPromotion{},
	}
}

// usageKey is the projection key for a usage row: one row per item per instance.
func usageKey(memoryID, instanceID string) string { return memoryID + "\x00" + instanceID }

// record appends a stamped event and projects it. The caller holds mu and has
// already produced the event via the Stamper. Append-then-project is the in-memory
// analogue of the one-transaction append+project the durable provider performs;
// here mu is the boundary that makes the pair atomic. It projects from the raw
// payload the Stamper already serialized (one Unmarshal of the same bytes the
// event carries), so the live projection is identical to a replayed one without
// the event round-trip apply pays.
func (c *core) record(ctx context.Context, in spine.AppendInput) error {
	e, err := c.log.Append(ctx, in)
	if err != nil {
		return err
	}
	var w payloadRecords
	if err := json.Unmarshal(in.RawPayload, &w); err != nil {
		return err
	}
	if err := c.projectRecords(e.Type, w); err != nil {
		return err
	}
	c.lastSeq = e.Seq
	c.maybeSnapshotLocked(ctx)
	return nil
}

// snapshot checkpoints the current projection onto the log. It builds the payload under
// mu, then seals and saves outside it so the store lock is not held across the signing
// and log write.
func (c *core) snapshot(ctx context.Context) error {
	c.mu.Lock()
	snap := c.snapshotLocked()
	c.mu.Unlock()
	return c.saveSnapshot(ctx, snap)
}

// saveSnapshot seals a snapshot with the configured codec (if any) and writes it to the
// log on the state stream. It touches no core state, so it is safe to call with or
// without mu held.
func (c *core) saveSnapshot(ctx context.Context, snap Snapshot) error {
	payload, err := MarshalSnapshot(snap)
	if err != nil {
		return err
	}
	s := spine.Snapshot{Stream: StateStream, Seq: snap.LastSeq, Payload: payload}
	if c.snapCodec != nil {
		if s, err = c.snapCodec.Seal(ctx, c.log, s); err != nil {
			return err
		}
	}
	return c.log.SaveSnapshot(ctx, s)
}

// maybeSnapshotLocked counts one mutation toward the automatic cadence and checkpoints
// when it is reached. The caller holds mu; the snapshot is built and saved under it,
// which keeps the projection and its checkpoint consistent. It is best effort: a snapshot
// failure never fails the mutation that triggered it.
func (c *core) maybeSnapshotLocked(ctx context.Context) {
	if c.snapEvery <= 0 {
		return
	}
	c.snapPending++
	if c.snapPending < c.snapEvery {
		return
	}
	c.snapPending = 0
	_ = c.saveSnapshot(ctx, c.snapshotLocked())
}

// payloadRecords is the decoded form of a state event payload: each key that a
// given event type carries is non-nil.
type payloadRecords struct {
	Session *Session    `json:"session"`
	Turn    *Turn       `json:"turn"`
	Skill   *Skill      `json:"skill"`
	Item    *MemoryItem `json:"item"`
	// Usage is a list because one push event records the whole digest: the set
	// went in front of the reader together, so it is counted together or not at
	// all. A single use carries a one-element list through the same path rather
	// than a second payload shape.
	Usage []MemoryUsage `json:"usage"`
	// Promotion is singular where Usage is a list: a reviewer decides about one
	// item at a time, and a bulk approval is that decision repeated, not a
	// different kind of event.
	Promotion *MemoryPromotion `json:"promotion"`
}

// projectRecords projects one event's post-image record(s) onto the read model.
// It is the single source of the projection logic, shared by the live write path
// (record) and reconstruction (apply, via Replay), so a rebuilt-from-log provider
// is byte-for-byte identical to a live one. Callers hold mu.
func (c *core) projectRecords(evType string, w payloadRecords) error {
	missing := func(key string) error {
		return fmt.Errorf("state: event %q payload is missing %q", evType, key)
	}
	switch evType {
	case EvSessionCreated, EvSessionDeleted:
		if w.Session == nil {
			return missing(keySession)
		}
		c.sessions[w.Session.ID] = *w.Session
	case EvTurnAppended:
		if w.Turn == nil {
			return missing(keyTurn)
		}
		if w.Session == nil {
			return missing(keySession)
		}
		c.turns[w.Turn.SessionID] = append(c.turns[w.Turn.SessionID], *w.Turn)
		c.sessions[w.Session.ID] = *w.Session
	case EvSkillUpserted:
		if w.Skill == nil {
			return missing(keySkill)
		}
		c.skillsByID[w.Skill.ID] = *w.Skill
		c.slugToID[scopeKey(w.Skill.Scope)+"\x00"+w.Skill.Slug] = w.Skill.ID
	case EvSkillDeleted:
		if w.Skill == nil {
			return missing(keySkill)
		}
		c.skillsByID[w.Skill.ID] = *w.Skill
	case EvMemoryWritten:
		if w.Item == nil {
			return missing(keyItem)
		}
		c.memItems = append(c.memItems, *w.Item)
	case EvMemoryDeleted:
		if w.Item == nil {
			return missing(keyItem)
		}
		for i := range c.memItems {
			if c.memItems[i].ID == w.Item.ID {
				c.memItems[i] = *w.Item
				break
			}
		}
	case EvMemoryPushed, EvMemoryUsed:
		if len(w.Usage) == 0 {
			return missing(keyUsage)
		}
		for _, u := range w.Usage {
			c.memUsage[usageKey(u.MemoryID, u.InstanceID)] = u
		}
	case EvMemoryPromoted:
		if w.Promotion == nil {
			return missing(keyPromo)
		}
		c.memPromo[w.Promotion.MemoryID] = *w.Promotion
	default:
		return fmt.Errorf("state: unknown event type %q", evType)
	}
	return nil
}

// apply projects one event onto the read model during Replay; the live write
// path (record) projects the same post-images from the raw payload instead.
// Callers hold mu.
func (c *core) apply(e spine.Event) error {
	var w payloadRecords
	if s, ok := e.Payload[keySession]; ok {
		ses, err := decodeAs[Session](s)
		if err != nil {
			return err
		}
		w.Session = &ses
	}
	if t, ok := e.Payload[keyTurn]; ok {
		turn, err := decodeAs[Turn](t)
		if err != nil {
			return err
		}
		w.Turn = &turn
	}
	if sk, ok := e.Payload[keySkill]; ok {
		skill, err := decodeAs[Skill](sk)
		if err != nil {
			return err
		}
		w.Skill = &skill
	}
	if it, ok := e.Payload[keyItem]; ok {
		item, err := decodeAs[MemoryItem](it)
		if err != nil {
			return err
		}
		w.Item = &item
	}
	if u, ok := e.Payload[keyUsage]; ok {
		usage, err := decodeAs[[]MemoryUsage](u)
		if err != nil {
			return err
		}
		w.Usage = usage
	}
	if pr, ok := e.Payload[keyPromo]; ok {
		promo, err := decodeAs[MemoryPromotion](pr)
		if err != nil {
			return err
		}
		w.Promotion = &promo
	}
	return c.projectRecords(e.Type, w)
}

// decodeAs reconstructs one typed record from its decoded payload value.
func decodeAs[T any](v any) (T, error) {
	var out T
	b, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}

// Replay reconstructs an in-memory Provider purely by folding a log's "state"
// stream: the running proof that state is a projection of the spine. The
// returned Provider is backed by the same log, with its projection caught up to
// the last recorded event, so subsequent writes continue the stream.
func Replay(ctx context.Context, log spine.Log, opts ...Option) (Provider, error) {
	p := NewMemory(append(opts, WithEventLog(log))...).(*memProvider)
	p.core.mu.Lock()
	defer p.core.mu.Unlock()

	// Resume from the latest verified snapshot when one exists, folding only the events
	// after it. A codec makes restore fail closed: a snapshot it cannot open (tampered,
	// unsigned, unknown key) or one that does not decode is skipped and the stream folds
	// from the start - a snapshot is a cache, so this is only slower, never wrong.
	snap, found, err := log.LatestSnapshot(ctx, StateStream, 0)
	if err != nil {
		return nil, err
	}
	if found && p.core.snapCodec != nil {
		opened, oerr := p.core.snapCodec.Open(ctx, snap)
		if oerr != nil {
			found = false
		} else {
			snap = opened
		}
	}
	if found {
		if decoded, derr := UnmarshalSnapshot(snap.Payload); derr == nil {
			p.core.restoreLocked(decoded)
		}
	}

	events, err := log.Read(ctx, spine.Query{Stream: StateStream, AfterSeq: p.core.lastSeq})
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		if err := p.core.apply(e); err != nil {
			return nil, err
		}
		p.core.lastSeq = e.Seq
	}
	return p, nil
}

// DecodeSession extracts the Session post-image from a state event payload.
// Durable backends use it to project the same records the in-memory core does.
func DecodeSession(payload map[string]any) (Session, error) {
	var s Session
	return s, decodeRecord(payload, keySession, &s)
}

// DecodeTurn extracts the Turn post-image from a state event payload.
func DecodeTurn(payload map[string]any) (Turn, error) {
	var t Turn
	return t, decodeRecord(payload, keyTurn, &t)
}

// DecodeSkill extracts the Skill post-image from a state event payload.
func DecodeSkill(payload map[string]any) (Skill, error) {
	var sk Skill
	return sk, decodeRecord(payload, keySkill, &sk)
}

// DecodeMemoryItem extracts the MemoryItem post-image from a state event payload.
func DecodeMemoryItem(payload map[string]any) (MemoryItem, error) {
	var it MemoryItem
	return it, decodeRecord(payload, keyItem, &it)
}

// DecodeMemoryUsage extracts the usage post-images from a state event payload.
// It is a list because a push event records a whole digest at once; a use event
// carries exactly one.
func DecodeMemoryUsage(payload map[string]any) ([]MemoryUsage, error) {
	var u []MemoryUsage
	if err := decodeRecord(payload, keyUsage, &u); err != nil {
		return nil, err
	}
	if len(u) == 0 {
		return nil, fmt.Errorf("state: event payload is missing %q", keyUsage)
	}
	return u, nil
}

// DecodeMemoryPromotion extracts the promotion post-image from a state event
// payload.
func DecodeMemoryPromotion(payload map[string]any) (MemoryPromotion, error) {
	var p MemoryPromotion
	return p, decodeRecord(payload, keyPromo, &p)
}

// encodePayload serialises an event's post-image record(s) to the raw JSON
// payload with a single Marshal. The spine stores these bytes verbatim
// (spine.AppendInput.RawPayload), so a write serialises its records exactly once
// instead of Marshal-Unmarshal-Marshal through a generic tree.
func encodePayload(records map[string]any) (json.RawMessage, error) {
	return json.Marshal(records)
}

// decodeRecord reconstructs a typed record from the payload value at key.
func decodeRecord(payload map[string]any, key string, dst any) error {
	// An absent key, or one explicitly set to null, marshals to "null", which
	// json.Unmarshal accepts as a no-op and leaves dst zeroed. Reject it instead:
	// the in-memory core treats a missing record as an error, and a durable
	// backend projecting a zero-valued record from the same event would diverge
	// from it.
	raw, ok := payload[key]
	if !ok || raw == nil {
		return fmt.Errorf("state: event payload is missing %q", key)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}
