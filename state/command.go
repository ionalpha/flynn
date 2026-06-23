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
)

// Payload keys under which a state event carries its post-image record(s).
const (
	keySession = "session"
	keyTurn    = "turn"
	keySkill   = "skill"
	keyItem    = "item"
)

// core is the in-memory read model behind the command path. Every mutation
// appends an event to the log and then projects it onto these maps under mu, so
// the log and the projection never diverge; reads take mu and read the maps.
//
// The invariant that keeps full event-sourcing reachable: no state mutation
// bypasses the log. apply is the only function that mutates the maps, and it is
// reached only from record (the live write path) and Replay (reconstruction) —
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
}

func newCore(st *Stamper, log spine.Log) *core {
	return &core{
		st:         st,
		log:        log,
		sessions:   map[string]Session{},
		turns:      map[string][]Turn{},
		skillsByID: map[string]Skill{},
		slugToID:   map[string]string{},
	}
}

// record appends a stamped event and projects it. The caller holds mu and has
// already produced the event via the Stamper. Append-then-project is the in-memory
// analogue of the one-transaction append+project the durable provider performs;
// here mu is the boundary that makes the pair atomic.
func (c *core) record(ctx context.Context, in spine.AppendInput) error {
	e, err := c.log.Append(ctx, in)
	if err != nil {
		return err
	}
	if err := c.apply(e); err != nil {
		return err
	}
	c.lastSeq = e.Seq
	return nil
}

// apply projects one event onto the read model. It is the single source of the
// projection logic, shared by the live write path (record) and reconstruction
// (Replay), so a rebuilt-from-log provider is byte-for-byte identical to a live
// one. Callers hold mu.
func (c *core) apply(e spine.Event) error {
	switch e.Type {
	case EvSessionCreated, EvSessionDeleted:
		s, err := DecodeSession(e.Payload)
		if err != nil {
			return err
		}
		c.sessions[s.ID] = s
	case EvTurnAppended:
		t, err := DecodeTurn(e.Payload)
		if err != nil {
			return err
		}
		s, err := DecodeSession(e.Payload)
		if err != nil {
			return err
		}
		c.turns[t.SessionID] = append(c.turns[t.SessionID], t)
		c.sessions[s.ID] = s
	case EvSkillUpserted:
		sk, err := DecodeSkill(e.Payload)
		if err != nil {
			return err
		}
		c.skillsByID[sk.ID] = sk
		c.slugToID[scopeKey(sk.Scope)+"\x00"+sk.Slug] = sk.ID
	case EvSkillDeleted:
		sk, err := DecodeSkill(e.Payload)
		if err != nil {
			return err
		}
		c.skillsByID[sk.ID] = sk
	case EvMemoryWritten:
		it, err := DecodeMemoryItem(e.Payload)
		if err != nil {
			return err
		}
		c.memItems = append(c.memItems, it)
	case EvMemoryDeleted:
		it, err := DecodeMemoryItem(e.Payload)
		if err != nil {
			return err
		}
		for i := range c.memItems {
			if c.memItems[i].ID == it.ID {
				c.memItems[i] = it
				break
			}
		}
	default:
		return fmt.Errorf("state: unknown event type %q", e.Type)
	}
	return nil
}

// Replay reconstructs an in-memory Provider purely by folding a log's "state"
// stream: the running proof that state is a projection of the spine. The
// returned Provider is backed by the same log, with its projection caught up to
// the last recorded event, so subsequent writes continue the stream.
func Replay(ctx context.Context, log spine.Log, opts ...Option) (Provider, error) {
	p := NewMemory(append(opts, WithEventLog(log))...).(*memProvider)
	events, err := log.Read(ctx, spine.Query{Stream: StateStream})
	if err != nil {
		return nil, err
	}
	p.core.mu.Lock()
	defer p.core.mu.Unlock()
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

// encodeRecord serialises a record to a JSON-compatible value for an event
// payload. The spine is a serialisation boundary (its payload is JSON in the
// durable log), so records cross it as canonical JSON.
func encodeRecord(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// decodeRecord reconstructs a typed record from the payload value at key.
func decodeRecord(payload map[string]any, key string, dst any) error {
	b, err := json.Marshal(payload[key])
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}
