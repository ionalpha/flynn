package state

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/usage"
)

// StateStream is the one spine stream every state mutation is recorded on, so
// [Replay] folds one log to rebuild the whole provider.
const StateStream = "state"

// State event types. Each payload is the post-image the [Stamper] computed,
// with every ID, HLC and timestamp assigned, so replay in Seq order
// reproduces identical state without re-running a clock or RNG.
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
	// EvModelUsageRecorded carries the [usage.Record]s a [ModelUsageStore] newly stored, so
	// replay and audit read the figures that were stored and the store can be
	// rebuilt from the log.
	EvModelUsageRecorded = "model_usage.recorded"
)

// Payload keys under which a state event carries its post-image record(s).
const (
	keySession = "session"
	keyTurn    = "turn"
	keySkill   = "skill"
	keyItem    = "item"
	keyUsage   = "usage"
	keyPromo   = "promotion"

	keyModelUsage = "modelUsage"
)

// core is the in-memory read model. Every mutation appends to the log and then
// projects onto these maps under mu, so the two never diverge. No mutation
// bypasses the log: apart from restoring a snapshot of it, only projectRecords
// writes the maps, reached from record and from Replay.
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

	// modelUsageRecs holds stored usage records so a snapshot of this projection
	// carries them: a durable store resuming from that snapshot would otherwise
	// lose every record before it. Nothing reads them here.
	modelUsageRecs map[modelUsageIdent]usage.Record

	// snapCodec seals and verifies snapshots; snapEvery and snapPending drive
	// the automatic cadence. Set after construction, read under mu.
	snapCodec   spine.SnapshotCodec
	snapEvery   int
	snapPending int
}

func newCore(st *Stamper, log spine.Log) *core {
	return &core{
		st:             st,
		log:            log,
		sessions:       map[string]Session{},
		turns:          map[string][]Turn{},
		skillsByID:     map[string]Skill{},
		slugToID:       map[string]string{},
		memUsage:       map[string]MemoryUsage{},
		memPromo:       map[string]MemoryPromotion{},
		modelUsageRecs: map[modelUsageIdent]usage.Record{},
	}
}

// usageKey keys one usage row per item per instance.
func usageKey(memoryID, instanceID string) string { return memoryID + "\x00" + instanceID }

// record appends a stamped event and projects it; holding mu makes the pair
// atomic, as one transaction does in the durable provider. It projects from
// the raw payload the event carries, so it matches a replayed projection.
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

// snapshot builds the payload under mu, then seals and saves outside it so the
// lock is not held across signing and the log write.
func (c *core) snapshot(ctx context.Context) error {
	c.mu.Lock()
	snap := c.snapshotLocked()
	c.mu.Unlock()
	return c.saveSnapshot(ctx, snap)
}

// saveSnapshot touches no core state, so mu may or may not be held.
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

// maybeSnapshotLocked counts a mutation toward the cadence and checkpoints
// under mu, so projection and checkpoint agree. A snapshot failure never fails
// the mutation.
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

// payloadRecords is a decoded state payload; each key its event type carries
// is non-nil.
type payloadRecords struct {
	Session *Session    `json:"session"`
	Turn    *Turn       `json:"turn"`
	Skill   *Skill      `json:"skill"`
	Item    *MemoryItem `json:"item"`
	// Usage is a list because a push records a whole digest atomically; a use
	// carries one element.
	Usage []MemoryUsage `json:"usage"`
	// Promotion is singular: a bulk approval is one decision repeated.
	Promotion *MemoryPromotion `json:"promotion"`
	// ModelUsageRecords is the batch an [EvModelUsageRecorded] event stored.
	ModelUsageRecords []usage.Record `json:"modelUsage"`
}

// projectRecords is the only projection logic, shared by record and Replay,
// so a provider rebuilt from the log is identical to a live one. Callers hold
// mu. Model usage records are split off because they share no state with the
// session and memory projection below, and keeping them apart holds that
// function under the complexity limit.
func (c *core) projectRecords(evType string, w payloadRecords) error {
	if evType == EvModelUsageRecorded {
		return c.projectModelUsage(evType, w.ModelUsageRecords)
	}
	return c.projectConversationRecords(evType, w)
}

// projectConversationRecords projects the session, turn, skill and memory events.
func (c *core) projectConversationRecords(evType string, w payloadRecords) error {
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

// apply projects one event during Replay. Callers hold mu.
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
	if ur, ok := e.Payload[keyModelUsage]; ok {
		recs, err := decodeAs[[]usage.Record](ur)
		if err != nil {
			return err
		}
		w.ModelUsageRecords = recs
	}
	return c.projectRecords(e.Type, w)
}

func decodeAs[T any](v any) (T, error) {
	var out T
	b, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}

// Replay rebuilds an in-memory Provider by folding the log's state stream.
// The Provider writes to the same log, continuing the stream.
func Replay(ctx context.Context, log spine.Log, opts ...Option) (Provider, error) {
	p := NewMemory(append(opts, WithEventLog(log))...).(*memProvider)
	p.core.mu.Lock()
	defer p.core.mu.Unlock()

	// Resume from the latest verified snapshot. One the codec cannot open, or
	// that does not decode, is skipped and the stream folds from the start.
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

// DecodeSession extracts the Session post-image from a state payload, so a
// durable backend projects what the in-memory core does.
func DecodeSession(payload map[string]any) (Session, error) {
	var s Session
	err := decodeRecord(payload, keySession, &s)
	return s, err
}

// DecodeTurn extracts the Turn post-image.
func DecodeTurn(payload map[string]any) (Turn, error) {
	var t Turn
	err := decodeRecord(payload, keyTurn, &t)
	return t, err
}

// DecodeSkill extracts the Skill post-image.
func DecodeSkill(payload map[string]any) (Skill, error) {
	var sk Skill
	err := decodeRecord(payload, keySkill, &sk)
	return sk, err
}

// DecodeMemoryItem extracts the MemoryItem post-image.
func DecodeMemoryItem(payload map[string]any) (MemoryItem, error) {
	var it MemoryItem
	err := decodeRecord(payload, keyItem, &it)
	return it, err
}

// DecodeMemoryUsage extracts the usage rows: a whole digest for a push, one
// row for a use.
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

// DecodeMemoryPromotion extracts the promotion post-image.
func DecodeMemoryPromotion(payload map[string]any) (MemoryPromotion, error) {
	var p MemoryPromotion
	err := decodeRecord(payload, keyPromo, &p)
	return p, err
}

// encodePayload marshals the records once; the spine stores the bytes
// verbatim as RawPayload.
func encodePayload(records map[string]any) (json.RawMessage, error) {
	return json.Marshal(records)
}

func decodeRecord(payload map[string]any, key string, dst any) error {
	// A missing or null key would decode to a zero record; the in-memory core
	// rejects it, so a durable backend must too or the two diverge.
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

// projectModelUsage keeps the first record under each (Run, Call), as a store that
// refuses a differing one would have, so a replayed log and a live store agree.
func (c *core) projectModelUsage(evType string, records []usage.Record) error {
	if len(records) == 0 {
		return fmt.Errorf("state: event %q payload is missing %q", evType, keyModelUsage)
	}
	for _, r := range records {
		k := modelUsageIdent{r.Run, r.Call}
		if _, ok := c.modelUsageRecs[k]; !ok {
			c.modelUsageRecs[k] = r
		}
	}
	return nil
}
