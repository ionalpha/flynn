package state

import (
	"context"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/spine"
)

// Option configures the in-memory Provider.
type Option func(*memProvider)

// WithInstanceID sets the origin/last-writer instance stamped onto records this
// provider creates (default "local"). The agent passes its own instance identity
// here so fleet/P2P merge can attribute records.
func WithInstanceID(id string) Option {
	return func(p *memProvider) {
		if id != "" {
			p.instanceID = id
		}
	}
}

// WithClock sets the time source for record timestamps (default clock.System),
// so tests and deterministic replay can supply a clock.Manual. The same clock
// stamps the event log when one is not injected separately.
func WithClock(c clock.Clock) Option {
	return func(p *memProvider) {
		if c != nil {
			p.clk = c
		}
	}
}

// WithEventLog backs the provider with a specific spine.Log instead of a private
// in-memory one. Inject a shared log to observe, audit, or Replay the state
// stream; pass the same log to Replay to reconstruct the provider from it.
func WithEventLog(l spine.Log) Option {
	return func(p *memProvider) {
		if l != nil {
			p.log = l
		}
	}
}

// WithIDGenerator sets the source of record IDs (default: a generator on the
// provider's clock with crypto/rand entropy). Supply a generator seeded with a
// deterministic clock and entropy so a re-run with the same seeds produces the
// exact same IDs — the basis of deterministic replay.
func WithIDGenerator(g *ids.Generator) Option {
	return func(p *memProvider) {
		if g != nil {
			p.gen = g
		}
	}
}

// NewMemory returns an empty in-memory Provider so the agent runs with zero
// setup. It is safe for concurrent use and intended as the standalone default
// and for tests. Every mutation is recorded on a spine.Log and projected, so the
// provider's state is always a fold of its log (see Replay).
func NewMemory(opts ...Option) Provider {
	p := &memProvider{instanceID: "local"}
	for _, o := range opts {
		o(p)
	}
	if p.clk == nil {
		p.clk = clock.System{}
	}
	if p.hlc == nil {
		p.hlc = hlc.NewClock(hlc.WithPhysical(p.clk))
	}
	if p.log == nil {
		p.log = spine.NewMemoryLog(spine.WithClock(p.clk))
	}
	if p.gen == nil {
		p.gen = ids.NewGenerator(ids.WithClock(p.clk))
	}
	p.core = newCore(p.instanceID, p.clk, p.hlc, p.log, p.gen)
	p.sessions = &memSessions{c: p.core}
	p.skills = &memSkills{c: p.core}
	p.memory = &memMemory{c: p.core}
	return p
}

// Compile-time checks that the in-memory types satisfy the state interfaces.
var (
	_ Provider     = (*memProvider)(nil)
	_ SessionStore = (*memSessions)(nil)
	_ SkillStore   = (*memSkills)(nil)
	_ MemoryStore  = (*memMemory)(nil)
)

type memProvider struct {
	instanceID string
	clk        clock.Clock
	hlc        *hlc.Clock
	log        spine.Log
	gen        *ids.Generator
	core       *core
	sessions   *memSessions
	skills     *memSkills
	memory     *memMemory
}

func (m *memProvider) Name() string           { return "memory" }
func (m *memProvider) Sessions() SessionStore { return m.sessions }
func (m *memProvider) Skills() SkillStore     { return m.skills }
func (m *memProvider) Memory() MemoryStore    { return m.memory }
func (m *memProvider) Close() error           { return nil }

// scopeKey is a stable map key for a Scope.
func scopeKey(s Scope) string {
	return s.Instance + "\x00" + s.Project + "\x00" + s.Workspace
}

type memSessions struct {
	c *core
}

func (s *memSessions) Create(ctx context.Context, ses Session) (Session, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if ses.ID == "" {
		ses.ID = c.gen.New()
	}
	now := c.clk.Now()
	if ses.CreatedAt.IsZero() {
		ses.CreatedAt = now
	}
	ses.UpdatedAt = now
	if ses.OriginInstanceID == "" {
		ses.OriginInstanceID = c.instanceID
	}
	ses.LastWriterID = c.instanceID
	ses.UpdatedHLC = c.hlc.Now()
	ses.SyncVersion = 1
	ses.Deleted = false
	payload, err := encodeRecord(ses)
	if err != nil {
		return Session{}, err
	}
	if err := c.record(ctx, evSessionCreated, map[string]any{"session": payload}); err != nil {
		return Session{}, err
	}
	return ses, nil
}

func (s *memSessions) Get(_ context.Context, id string) (Session, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	ses, ok := c.sessions[id]
	if !ok || ses.Deleted {
		return Session{}, ErrNotFound
	}
	return ses, nil
}

func (s *memSessions) List(_ context.Context) ([]Session, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Session, 0, len(c.sessions))
	for _, ses := range c.sessions {
		if ses.Deleted {
			continue
		}
		out = append(out, ses)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID // total order: deterministic reads regardless of map iteration
	})
	return out, nil
}

func (s *memSessions) AppendTurn(ctx context.Context, t Turn) (Turn, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	ses, ok := c.sessions[t.SessionID]
	if !ok || ses.Deleted {
		return Turn{}, ErrNotFound
	}
	if t.ID == "" {
		t.ID = c.gen.New()
	}
	t.Seq = int64(len(c.turns[t.SessionID]) + 1)
	now := c.clk.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.OriginInstanceID == "" {
		t.OriginInstanceID = c.instanceID
	}
	hnow := c.hlc.Now()
	t.LastWriterID = c.instanceID
	t.UpdatedHLC = hnow
	t.SyncVersion = 1
	t.Deleted = false
	// Appending a turn mutates the session: bump its envelope under the same HLC.
	ses.UpdatedAt = t.CreatedAt
	ses.LastWriterID = c.instanceID
	ses.UpdatedHLC = hnow
	ses.SyncVersion++
	turnPayload, err := encodeRecord(t)
	if err != nil {
		return Turn{}, err
	}
	sessionPayload, err := encodeRecord(ses)
	if err != nil {
		return Turn{}, err
	}
	if err := c.record(ctx, evTurnAppended, map[string]any{"turn": turnPayload, "session": sessionPayload}); err != nil {
		return Turn{}, err
	}
	return t, nil
}

func (s *memSessions) Turns(_ context.Context, sessionID string) ([]Turn, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	src := c.turns[sessionID]
	out := make([]Turn, len(src))
	copy(out, src)
	return out, nil
}

func (s *memSessions) Delete(ctx context.Context, id string) error {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	ses, ok := c.sessions[id]
	if !ok || ses.Deleted {
		return ErrNotFound
	}
	ses.Deleted = true
	ses.LastWriterID = c.instanceID
	ses.UpdatedHLC = c.hlc.Now()
	ses.UpdatedAt = c.clk.Now()
	ses.SyncVersion++
	payload, err := encodeRecord(ses)
	if err != nil {
		return err
	}
	return c.record(ctx, evSessionDeleted, map[string]any{"session": payload})
}

type memSkills struct {
	c *core
}

func (s *memSkills) Upsert(ctx context.Context, sk Skill) (Skill, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clk.Now()
	key := scopeKey(sk.Scope) + "\x00" + sk.Slug

	if id, ok := c.slugToID[key]; ok {
		existing := c.skillsByID[id]
		// Opt-in optimistic concurrency: a non-zero SyncVersion must match.
		if sk.SyncVersion != 0 && sk.SyncVersion != existing.SyncVersion {
			return Skill{}, ErrConflict
		}
		sk.ID = id
		sk.CreatedAt = existing.CreatedAt
		sk.OriginInstanceID = existing.OriginInstanceID // origin is preserved
		sk.Version = existing.Version + 1
		sk.SyncVersion = existing.SyncVersion + 1
		sk.LastWriterID = c.instanceID
		sk.UpdatedHLC = c.hlc.Now()
		sk.UpdatedAt = now
		// An upsert over a tombstone resurrects it (Deleted comes from sk).
		payload, err := encodeRecord(sk)
		if err != nil {
			return Skill{}, err
		}
		if err := c.record(ctx, evSkillUpserted, map[string]any{"skill": payload}); err != nil {
			return Skill{}, err
		}
		return sk, nil
	}

	// Creating: a non-zero SyncVersion expected an existing record that is gone.
	if sk.SyncVersion != 0 {
		return Skill{}, ErrConflict
	}
	if sk.ID == "" {
		sk.ID = c.gen.New()
	}
	if sk.Version == 0 {
		sk.Version = 1
	}
	sk.SyncVersion = 1
	if sk.OriginInstanceID == "" {
		sk.OriginInstanceID = c.instanceID
	}
	sk.LastWriterID = c.instanceID
	sk.UpdatedHLC = c.hlc.Now()
	sk.CreatedAt = now
	sk.UpdatedAt = now
	payload, err := encodeRecord(sk)
	if err != nil {
		return Skill{}, err
	}
	if err := c.record(ctx, evSkillUpserted, map[string]any{"skill": payload}); err != nil {
		return Skill{}, err
	}
	return sk, nil
}

func (s *memSkills) Get(_ context.Context, idOrSlug string) (Skill, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if sk, ok := c.skillsByID[idOrSlug]; ok && !sk.Deleted {
		return sk, nil
	}
	for _, sk := range c.skillsByID {
		if sk.Slug == idOrSlug && !sk.Deleted {
			return sk, nil
		}
	}
	return Skill{}, ErrNotFound
}

func (s *memSkills) List(_ context.Context, scope Scope) ([]Skill, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Skill, 0)
	for _, sk := range c.skillsByID {
		if sk.Scope == scope && !sk.Deleted {
			out = append(out, sk)
		}
	}
	sort.Slice(out, sliceSkillsBySlug(out))
	return out, nil
}

func (s *memSkills) Search(_ context.Context, query string, limit int) ([]Skill, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]Skill, 0)
	for _, sk := range c.skillsByID {
		if sk.Deleted {
			continue
		}
		if q == "" || skillMatches(sk, q) {
			out = append(out, sk)
		}
	}
	sort.Slice(out, sliceSkillsBySlug(out))
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *memSkills) Delete(ctx context.Context, idOrSlug string) error {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.resolveSkill(idOrSlug)
	if !ok {
		return ErrNotFound
	}
	sk := c.skillsByID[id]
	sk.Deleted = true
	sk.Version++
	sk.SyncVersion++
	sk.LastWriterID = c.instanceID
	sk.UpdatedHLC = c.hlc.Now()
	sk.UpdatedAt = c.clk.Now()
	payload, err := encodeRecord(sk)
	if err != nil {
		return err
	}
	return c.record(ctx, evSkillDeleted, map[string]any{"skill": payload})
}

// resolveSkill finds a live skill's id by id or slug. Callers hold mu.
func (c *core) resolveSkill(idOrSlug string) (string, bool) {
	if sk, ok := c.skillsByID[idOrSlug]; ok && !sk.Deleted {
		return idOrSlug, true
	}
	for id, sk := range c.skillsByID {
		if sk.Slug == idOrSlug && !sk.Deleted {
			return id, true
		}
	}
	return "", false
}

// sliceSkillsBySlug orders skills by Slug with an ID tiebreak, so reads are a
// total, deterministic order even when slugs collide across scopes.
func sliceSkillsBySlug(s []Skill) func(i, j int) bool {
	return func(i, j int) bool {
		if s[i].Slug != s[j].Slug {
			return s[i].Slug < s[j].Slug
		}
		return s[i].ID < s[j].ID
	}
}

func skillMatches(sk Skill, lowerQuery string) bool {
	return strings.Contains(strings.ToLower(sk.Name), lowerQuery) ||
		strings.Contains(strings.ToLower(sk.Body), lowerQuery) ||
		strings.Contains(strings.ToLower(strings.Join(sk.Tags, " ")), lowerQuery)
}

type memMemory struct {
	c *core
}

func (m *memMemory) Write(ctx context.Context, it MemoryItem) (MemoryItem, error) {
	c := m.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if it.ID == "" {
		it.ID = c.gen.New()
	}
	if it.CreatedAt.IsZero() {
		it.CreatedAt = c.clk.Now()
	}
	if it.OriginInstanceID == "" {
		it.OriginInstanceID = c.instanceID
	}
	it.LastWriterID = c.instanceID
	it.UpdatedHLC = c.hlc.Now()
	it.SyncVersion = 1
	it.Deleted = false
	payload, err := encodeRecord(it)
	if err != nil {
		return MemoryItem{}, err
	}
	if err := c.record(ctx, evMemoryWritten, map[string]any{"item": payload}); err != nil {
		return MemoryItem{}, err
	}
	return it, nil
}

func (m *memMemory) Recall(_ context.Context, q RecallQuery) ([]MemoryItem, error) {
	c := m.c
	c.mu.Lock()
	defer c.mu.Unlock()
	query := strings.ToLower(strings.TrimSpace(q.Query))
	out := make([]MemoryItem, 0)
	for _, it := range c.memItems {
		if it.Deleted {
			continue
		}
		if q.Scope != (Scope{}) && it.Scope != q.Scope {
			continue
		}
		if query == "" || strings.Contains(strings.ToLower(it.Content), query) {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID // total order: deterministic reads regardless of map iteration
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (m *memMemory) Delete(ctx context.Context, id string) error {
	c := m.c
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.memItems {
		if c.memItems[i].ID == id && !c.memItems[i].Deleted {
			it := c.memItems[i]
			it.Deleted = true
			it.LastWriterID = c.instanceID
			it.UpdatedHLC = c.hlc.Now()
			it.SyncVersion++
			payload, err := encodeRecord(it)
			if err != nil {
				return err
			}
			return c.record(ctx, evMemoryDeleted, map[string]any{"item": payload})
		}
	}
	return ErrNotFound
}
