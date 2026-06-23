package state

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/spine"
)

// StateStream is the spine stream every state mutation is recorded on. A single
// ordered stream for sessions, skills, and memory means a Replay folds one log
// to reconstruct the whole provider. It is exported so a host can observe or
// audit the state stream directly.
const StateStream = "state"

// State event types. Each is the post-image of the affected record(s): the
// command computes the canonical record (IDs, Seq, HLC, version, timestamps all
// assigned) and writes it as the event payload, so replaying the event in Seq
// order reproduces identical state without re-running any clock or RNG.
const (
	evSessionCreated = "session.created"
	evTurnAppended   = "session.turn_appended"
	evSessionDeleted = "session.deleted"
	evSkillUpserted  = "skill.upserted"
	evSkillDeleted   = "skill.deleted"
	evMemoryWritten  = "memory.written"
	evMemoryDeleted  = "memory.deleted"
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
	instanceID string
	clk        clock.Clock
	hlc        *hlc.Clock
	log        spine.Log
	gen        *ids.Generator

	mu      sync.Mutex
	lastSeq int64 // the highest event Seq this projection has applied

	sessions   map[string]Session
	turns      map[string][]Turn
	skillsByID map[string]Skill
	slugToID   map[string]string // scopeKey+"\x00"+slug -> skill id
	memItems   []MemoryItem
}

func newCore(instanceID string, clk clock.Clock, hc *hlc.Clock, log spine.Log, gen *ids.Generator) *core {
	return &core{
		instanceID: instanceID,
		clk:        clk,
		hlc:        hc,
		log:        log,
		gen:        gen,
		sessions:   map[string]Session{},
		turns:      map[string][]Turn{},
		skillsByID: map[string]Skill{},
		slugToID:   map[string]string{},
	}
}

// record appends a single state event and projects it. The caller holds mu and
// has already computed the canonical record(s) in payload. Append-then-project
// is the in-memory analogue of the one-transaction append+project the durable
// provider performs; here mu is the boundary that makes the pair atomic.
func (c *core) record(ctx context.Context, typ string, payload map[string]any) error {
	e, err := c.log.Append(ctx, spine.AppendInput{
		Stream:           StateStream,
		Type:             typ,
		Actor:            spine.ActorAgent,
		Payload:          payload,
		OriginInstanceID: c.instanceID,
	})
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
	case evSessionCreated, evSessionDeleted:
		var s Session
		if err := decodeRecord(e.Payload, "session", &s); err != nil {
			return err
		}
		c.sessions[s.ID] = s
	case evTurnAppended:
		var t Turn
		var s Session
		if err := decodeRecord(e.Payload, "turn", &t); err != nil {
			return err
		}
		if err := decodeRecord(e.Payload, "session", &s); err != nil {
			return err
		}
		c.turns[t.SessionID] = append(c.turns[t.SessionID], t)
		c.sessions[s.ID] = s
	case evSkillUpserted:
		var sk Skill
		if err := decodeRecord(e.Payload, "skill", &sk); err != nil {
			return err
		}
		c.skillsByID[sk.ID] = sk
		c.slugToID[scopeKey(sk.Scope)+"\x00"+sk.Slug] = sk.ID
	case evSkillDeleted:
		var sk Skill
		if err := decodeRecord(e.Payload, "skill", &sk); err != nil {
			return err
		}
		c.skillsByID[sk.ID] = sk
	case evMemoryWritten:
		var it MemoryItem
		if err := decodeRecord(e.Payload, "item", &it); err != nil {
			return err
		}
		c.memItems = append(c.memItems, it)
	case evMemoryDeleted:
		var it MemoryItem
		if err := decodeRecord(e.Payload, "item", &it); err != nil {
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
