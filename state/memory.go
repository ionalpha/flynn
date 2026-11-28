package state

import (
	"context"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/recallplan"
	"github.com/ionalpha/flynn/spine"
)

// Option configures the in-memory Provider.
type Option func(*memProvider)

// WithInstanceID sets the instance stamped as origin and last writer (default
// "local"), so a fleet merge can attribute records.
func WithInstanceID(id string) Option {
	return func(p *memProvider) {
		if id != "" {
			p.instanceID = id
		}
	}
}

// WithClock sets the record clock (default clock.System). It also stamps the
// event log unless one is injected.
func WithClock(c clock.Clock) Option {
	return func(p *memProvider) {
		if c != nil {
			p.clk = c
		}
	}
}

// WithEventLog records on log instead of a private one; pass the same log to
// [Replay] to rebuild the provider.
func WithEventLog(l spine.Log) Option {
	return func(p *memProvider) {
		if l != nil {
			p.log = l
		}
	}
}

// WithIDGenerator sets the ID source (default: the provider's clock with
// crypto/rand entropy). A seeded generator gives a re-run identical IDs.
func WithIDGenerator(g *ids.Generator) Option {
	return func(p *memProvider) {
		if g != nil {
			p.gen = g
		}
	}
}

// WithSnapshotCodec seals snapshots on save and verifies them in [Replay]. One
// that fails to open is skipped, so an unsigned or tampered snapshot is never
// restored. Without a codec payloads are stored as-is.
func WithSnapshotCodec(c spine.SnapshotCodec) Option {
	return func(p *memProvider) { p.snapCodec = c }
}

// WithSnapshotEvery snapshots after every k mutations, so [Replay] folds at
// most k events past the last one. A snapshot failure never fails the write.
// k <= 0 (the default) disables it.
func WithSnapshotEvery(k int) Option {
	return func(p *memProvider) { p.snapEvery = k }
}

// NewMemory returns an empty in-memory Provider, safe for concurrent use. Its
// state is always a fold of its log (see [Replay]).
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
	st := NewStamper(p.instanceID, p.clk, p.hlc, p.gen)
	p.core = newCore(st, p.log)
	p.core.snapCodec = p.snapCodec
	p.core.snapEvery = p.snapEvery
	p.sessions = &memSessions{c: p.core}
	p.skills = &memSkills{c: p.core}
	p.memory = &memMemory{c: p.core}
	return p
}

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
	snapCodec  spine.SnapshotCodec
	snapEvery  int
}

// Snapshot checkpoints the projection at the last applied Seq, so [Replay]
// cost stays flat as the stream grows. It is sealed by the codec, if any.
func (m *memProvider) Snapshot(ctx context.Context) error { return m.core.snapshot(ctx) }

func (m *memProvider) Name() string           { return "memory" }
func (m *memProvider) Sessions() SessionStore { return m.sessions }
func (m *memProvider) Skills() SkillStore     { return m.skills }
func (m *memProvider) Memory() MemoryStore    { return m.memory }
func (m *memProvider) Close() error           { return nil }

// Log returns the provider's spine. The conformance suite holds a backend that
// exposes its log to "no write bypasses the log".
func (m *memProvider) Log() spine.Log { return m.log }

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
	rec, ev, err := c.st.CreateSession(ses)
	if err != nil {
		return Session{}, err
	}
	if err := c.record(ctx, ev); err != nil {
		return Session{}, err
	}
	return rec, nil
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
	//ordered:ok sorted below by creation time and ID
	for _, ses := range c.sessions {
		if ses.Deleted {
			continue
		}
		out = append(out, ses)
	}
	sort.SliceStable(out, func(i, j int) bool {
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
	nextSeq := int64(len(c.turns[t.SessionID]) + 1)
	rec, _, ev, err := c.st.AppendTurn(ses, t, nextSeq)
	if err != nil {
		return Turn{}, err
	}
	if err := c.record(ctx, ev); err != nil {
		return Turn{}, err
	}
	return rec, nil
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
	_, ev, err := c.st.DeleteSession(ses)
	if err != nil {
		return err
	}
	return c.record(ctx, ev)
}

type memSkills struct {
	c *core
}

func (s *memSkills) Upsert(ctx context.Context, sk Skill) (Skill, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	var existing *Skill
	if id, ok := c.slugToID[scopeKey(sk.Scope)+"\x00"+sk.Slug]; ok {
		e := c.skillsByID[id]
		existing = &e
	}
	rec, ev, err := c.st.UpsertSkill(existing, sk)
	if err != nil {
		return Skill{}, err
	}
	if err := c.record(ctx, ev); err != nil {
		return Skill{}, err
	}
	return rec, nil
}

func (s *memSkills) Get(_ context.Context, idOrSlug string) (Skill, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.resolveSkill(idOrSlug)
	if !ok {
		return Skill{}, ErrNotFound
	}
	return c.skillsByID[id], nil
}

func (s *memSkills) List(_ context.Context, scope Scope) ([]Skill, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Skill, 0)
	//ordered:ok sorted below by slug and ID
	for _, sk := range c.skillsByID {
		if sk.Scope == scope && !sk.Deleted {
			out = append(out, sk)
		}
	}
	sort.SliceStable(out, sliceSkillsBySlug(out))
	return out, nil
}

func (s *memSkills) Search(_ context.Context, query string, limit int) ([]Skill, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	q := strings.TrimSpace(query)
	live := make([]Skill, 0, len(c.skillsByID))
	//ordered:ok sorted below by slug and ID
	for _, sk := range c.skillsByID {
		if !sk.Deleted {
			live = append(live, sk)
		}
	}
	out := live
	if q != "" {
		hits := recallplan.Select(q, live, skillText, 0)
		out = make([]Skill, 0, len(hits))
		for _, h := range hits {
			out = append(out, h.Item)
		}
	}
	sort.SliceStable(out, sliceSkillsBySlug(out))
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
	_, ev, err := c.st.DeleteSkill(c.skillsByID[id])
	if err != nil {
		return err
	}
	return c.record(ctx, ev)
}

// resolveSkill finds a live skill's id by id or slug. A slug held in several
// scopes resolves to the oldest, then the lowest ID, as the SQL backends order
// it. Callers hold mu.
func (c *core) resolveSkill(idOrSlug string) (string, bool) {
	if sk, ok := c.skillsByID[idOrSlug]; ok && !sk.Deleted {
		return idOrSlug, true
	}
	var found Skill
	ok := false
	for _, id := range slices.Sorted(maps.Keys(c.skillsByID)) {
		sk := c.skillsByID[id]
		if sk.Slug == idOrSlug && !sk.Deleted && (!ok || sk.CreatedAt.Before(found.CreatedAt)) {
			found, ok = sk, true
		}
	}
	return found.ID, ok
}

// sliceSkillsBySlug breaks Slug ties by ID, since slugs repeat across scopes.
func sliceSkillsBySlug(s []Skill) func(i, j int) bool {
	return func(i, j int) bool {
		if s[i].Slug != s[j].Slug {
			return s[i].Slug < s[j].Slug
		}
		return s[i].ID < s[j].ID
	}
}

// skillText joins every field a skill search matches.
func skillText(sk Skill) string {
	return strings.Join([]string{sk.Name, sk.Description, sk.Body, strings.Join(sk.Tags, " ")}, "\n")
}

type memMemory struct {
	c *core
}

func (m *memMemory) Write(ctx context.Context, it MemoryItem) (MemoryItem, error) {
	c := m.c
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ev, err := c.st.WriteMemory(it)
	if err != nil {
		return MemoryItem{}, err
	}
	if err := c.record(ctx, ev); err != nil {
		return MemoryItem{}, err
	}
	return rec, nil
}

func (m *memMemory) Recall(_ context.Context, q RecallQuery) ([]MemoryItem, error) {
	c := m.c
	c.mu.Lock()
	defer c.mu.Unlock()
	query := strings.TrimSpace(q.Query)
	// One clock reading, so every item is judged against the same instant.
	now := c.st.Now()
	// A nil chain is unfiltered; otherwise it is the scope, plus its ancestors
	// when widened.
	var chain map[Scope]bool
	if scopes := q.ScopeChain(); scopes != nil {
		chain = make(map[Scope]bool, len(scopes))
		for _, s := range scopes {
			chain[s] = true
		}
	}
	candidates := make([]MemoryItem, 0)
	for _, it := range c.memItems {
		if it.Deleted {
			continue
		}
		if chain != nil && !chain[it.Scope] {
			continue
		}
		if !q.Selects(it, now) {
			continue
		}
		candidates = append(candidates, it)
	}
	// With no query every item scores 1, which MemoryItem.Score reserves for
	// "matched, no grade", so MinScore excludes nothing. A query goes through
	// recallplan.Plan, whose partial matches score below 1.
	out := make([]MemoryItem, 0, len(candidates))
	if query == "" {
		for _, it := range candidates {
			it.Score = 1
			if it.Score >= q.MinScore {
				out = append(out, it)
			}
		}
	} else {
		for _, h := range recallplan.Select(query, candidates, func(it MemoryItem) string { return it.Content }, q.MinScore) {
			h.Item.Score = h.Share
			out = append(out, h.Item)
		}
	}
	SortRecall(q, out)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (m *memMemory) RecordPush(ctx context.Context, memoryIDs []string) error {
	if len(memoryIDs) == 0 {
		return nil
	}
	c := m.c
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, err := c.usagePrevLocked(memoryIDs)
	if err != nil {
		return err
	}
	_, ev, err := c.st.RecordMemoryPush(prev, memoryIDs)
	if err != nil {
		return err
	}
	return c.record(ctx, ev)
}

func (m *memMemory) RecordUse(ctx context.Context, memoryID string, origin UsageOrigin) error {
	c := m.c
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, err := c.usagePrevLocked([]string{memoryID})
	if err != nil {
		return err
	}
	_, ev, err := c.st.RecordMemoryUse(prev, memoryID, origin)
	if err != nil {
		return err
	}
	return c.record(ctx, ev)
}

func (m *memMemory) Usage(_ context.Context, memoryIDs []string) ([]MemoryUsage, error) {
	c := m.c
	c.mu.Lock()
	defer c.mu.Unlock()
	out := recordsFor(c.memUsage, memoryIDs, func(u MemoryUsage) string { return u.MemoryID })
	SortUsage(out)
	return out, nil
}

// recordsFor returns the records for memoryIDs, or all of them when it is
// empty. The result is unordered; callers sort.
func recordsFor[T any](held map[string]T, memoryIDs []string, idOf func(T) string) []T {
	var want map[string]bool
	if len(memoryIDs) > 0 {
		want = make(map[string]bool, len(memoryIDs))
		for _, id := range memoryIDs {
			want[id] = true
		}
	}
	out := make([]T, 0, len(held))
	//ordered:ok documented as unordered; callers sort
	for _, rec := range held {
		if want != nil && !want[idOf(rec)] {
			continue
		}
		out = append(out, rec)
	}
	return out
}

func (m *memMemory) Promote(ctx context.Context, d PromotionDecision) (MemoryPromotion, error) {
	c := m.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if !d.Valid() {
		return MemoryPromotion{}, ErrInvalid
	}
	// Checked before stamping, so a decision on a missing item errors instead
	// of leaving an orphan row.
	if !c.liveMemoryLocked(d.MemoryID) {
		return MemoryPromotion{}, ErrNotFound
	}
	var prev *MemoryPromotion
	if p, ok := c.memPromo[d.MemoryID]; ok {
		prev = &p
	}
	rec, ev, err := c.st.RecordMemoryPromotion(prev, d)
	if err != nil {
		return MemoryPromotion{}, err
	}
	if err := c.record(ctx, ev); err != nil {
		return MemoryPromotion{}, err
	}
	return rec, nil
}

func (m *memMemory) Promotions(_ context.Context, memoryIDs []string) ([]MemoryPromotion, error) {
	c := m.c
	c.mu.Lock()
	defer c.mu.Unlock()
	out := recordsFor(c.memPromo, memoryIDs, func(p MemoryPromotion) string { return p.MemoryID })
	SortPromotions(out)
	return out, nil
}

// liveMemoryLocked reports whether id names an untombstoned item. Callers hold
// mu.
func (c *core) liveMemoryLocked(id string) bool {
	for i := range c.memItems {
		if c.memItems[i].ID == id && !c.memItems[i].Deleted {
			return true
		}
	}
	return false
}

// usagePrevLocked returns this instance's rows for ids and rejects any id with
// no live item, before stamping, so one bad id records nothing. Callers hold
// mu.
func (c *core) usagePrevLocked(memoryIDs []string) (map[string]MemoryUsage, error) {
	live := make(map[string]bool, len(c.memItems))
	for i := range c.memItems {
		if !c.memItems[i].Deleted {
			live[c.memItems[i].ID] = true
		}
	}
	prev := make(map[string]MemoryUsage, len(memoryIDs))
	for _, id := range memoryIDs {
		if !live[id] {
			return nil, ErrNotFound
		}
		if u, ok := c.memUsage[usageKey(id, c.st.InstanceID())]; ok {
			prev[id] = u
		}
	}
	return prev, nil
}

func (m *memMemory) Delete(ctx context.Context, id string) error {
	c := m.c
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.memItems {
		if c.memItems[i].ID == id && !c.memItems[i].Deleted {
			_, ev, err := c.st.DeleteMemory(c.memItems[i])
			if err != nil {
				return err
			}
			return c.record(ctx, ev)
		}
	}
	return ErrNotFound
}
