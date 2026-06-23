package state

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/ids"
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

// NewMemory returns an empty in-memory Provider so the agent runs with zero
// setup. It is safe for concurrent use and intended as the standalone default
// and for tests; the durable SQLite Provider lands in a follow-up.
func NewMemory(opts ...Option) Provider {
	p := &memProvider{instanceID: "local", hlc: hlc.NewClock()}
	for _, o := range opts {
		o(p)
	}
	p.sessions = &memSessions{instanceID: p.instanceID, hlc: p.hlc, byID: map[string]Session{}, turns: map[string][]Turn{}}
	p.skills = &memSkills{instanceID: p.instanceID, hlc: p.hlc, byID: map[string]Skill{}, slugToID: map[string]string{}}
	p.memory = &memMemory{instanceID: p.instanceID, hlc: p.hlc}
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
	hlc        *hlc.Clock
	sessions   *memSessions
	skills     *memSkills
	memory     *memMemory
}

func (m *memProvider) Name() string           { return "memory" }
func (m *memProvider) Sessions() SessionStore { return m.sessions }
func (m *memProvider) Skills() SkillStore     { return m.skills }
func (m *memProvider) Memory() MemoryStore    { return m.memory }
func (m *memProvider) Close() error           { return nil }

// newID returns a new time-sortable, globally-unique identifier (UUIDv7).
func newID() string {
	return ids.New()
}

// scopeKey is a stable map key for a Scope.
func scopeKey(s Scope) string {
	return s.Instance + "\x00" + s.Project + "\x00" + s.Workspace
}

type memSessions struct {
	instanceID string
	hlc        *hlc.Clock
	mu         sync.Mutex
	byID       map[string]Session
	turns      map[string][]Turn
}

func (s *memSessions) Create(_ context.Context, ses Session) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ses.ID == "" {
		ses.ID = newID()
	}
	now := time.Now().UTC()
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
	s.byID[ses.ID] = ses
	return ses, nil
}

func (s *memSessions) Get(_ context.Context, id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ses, ok := s.byID[id]
	if !ok || ses.Deleted {
		return Session{}, ErrNotFound
	}
	return ses, nil
}

func (s *memSessions) List(_ context.Context) ([]Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Session, 0, len(s.byID))
	for _, ses := range s.byID {
		if ses.Deleted {
			continue
		}
		out = append(out, ses)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *memSessions) AppendTurn(_ context.Context, t Turn) (Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ses, ok := s.byID[t.SessionID]
	if !ok || ses.Deleted {
		return Turn{}, ErrNotFound
	}
	if t.ID == "" {
		t.ID = newID()
	}
	t.Seq = int64(len(s.turns[t.SessionID]) + 1)
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.OriginInstanceID == "" {
		t.OriginInstanceID = s.instanceID
	}
	now := s.hlc.Now()
	t.LastWriterID = s.instanceID
	t.UpdatedHLC = now
	t.SyncVersion = 1
	s.turns[t.SessionID] = append(s.turns[t.SessionID], t)
	// Appending a turn mutates the session.
	ses.UpdatedAt = t.CreatedAt
	ses.LastWriterID = s.instanceID
	ses.UpdatedHLC = now
	ses.SyncVersion++
	s.byID[t.SessionID] = ses
	return t, nil
}

func (s *memSessions) Turns(_ context.Context, sessionID string) ([]Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.turns[sessionID]
	out := make([]Turn, len(src))
	copy(out, src)
	return out, nil
}

func (s *memSessions) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ses, ok := s.byID[id]
	if !ok || ses.Deleted {
		return ErrNotFound
	}
	ses.Deleted = true
	ses.LastWriterID = s.instanceID
	ses.UpdatedHLC = s.hlc.Now()
	ses.UpdatedAt = time.Now().UTC()
	ses.SyncVersion++
	s.byID[id] = ses
	return nil
}

type memSkills struct {
	instanceID string
	hlc        *hlc.Clock
	mu         sync.Mutex
	byID       map[string]Skill
	slugToID   map[string]string // scopeKey+"\x00"+slug -> id
}

func (s *memSkills) Upsert(_ context.Context, sk Skill) (Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	key := scopeKey(sk.Scope) + "\x00" + sk.Slug

	if id, ok := s.slugToID[key]; ok {
		existing := s.byID[id]
		// Opt-in optimistic concurrency: a non-zero SyncVersion must match.
		if sk.SyncVersion != 0 && sk.SyncVersion != existing.SyncVersion {
			return Skill{}, ErrConflict
		}
		sk.ID = id
		sk.CreatedAt = existing.CreatedAt
		sk.OriginInstanceID = existing.OriginInstanceID // origin is preserved
		sk.Version = existing.Version + 1
		sk.SyncVersion = existing.SyncVersion + 1
		sk.LastWriterID = s.instanceID
		sk.UpdatedHLC = s.hlc.Now()
		sk.UpdatedAt = now
		s.byID[id] = sk // an upsert over a tombstone resurrects it (Deleted from sk)
		return sk, nil
	}

	// Creating: a non-zero SyncVersion expected an existing record that is gone.
	if sk.SyncVersion != 0 {
		return Skill{}, ErrConflict
	}
	if sk.ID == "" {
		sk.ID = newID()
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
	s.byID[sk.ID] = sk
	s.slugToID[key] = sk.ID
	return sk, nil
}

func (s *memSkills) Get(_ context.Context, idOrSlug string) (Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sk, ok := s.byID[idOrSlug]; ok && !sk.Deleted {
		return sk, nil
	}
	for _, sk := range s.byID {
		if sk.Slug == idOrSlug && !sk.Deleted {
			return sk, nil
		}
	}
	return Skill{}, ErrNotFound
}

func (s *memSkills) List(_ context.Context, scope Scope) ([]Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Skill, 0)
	for _, sk := range s.byID {
		if sk.Scope == scope && !sk.Deleted {
			out = append(out, sk)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (s *memSkills) Search(_ context.Context, query string, limit int) ([]Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]Skill, 0)
	for _, sk := range s.byID {
		if sk.Deleted {
			continue
		}
		if q == "" || skillMatches(sk, q) {
			out = append(out, sk)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *memSkills) Delete(_ context.Context, idOrSlug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.resolve(idOrSlug)
	if !ok {
		return ErrNotFound
	}
	sk := s.byID[id]
	sk.Deleted = true
	sk.Version++
	sk.SyncVersion++
	sk.LastWriterID = s.instanceID
	sk.UpdatedHLC = s.hlc.Now()
	sk.UpdatedAt = time.Now().UTC()
	s.byID[id] = sk
	return nil
}

// resolve finds a live skill's id by id or slug.
func (s *memSkills) resolve(idOrSlug string) (string, bool) {
	if sk, ok := s.byID[idOrSlug]; ok && !sk.Deleted {
		return idOrSlug, true
	}
	for id, sk := range s.byID {
		if sk.Slug == idOrSlug && !sk.Deleted {
			return id, true
		}
	}
	return "", false
}

func skillMatches(sk Skill, lowerQuery string) bool {
	return strings.Contains(strings.ToLower(sk.Name), lowerQuery) ||
		strings.Contains(strings.ToLower(sk.Body), lowerQuery) ||
		strings.Contains(strings.ToLower(strings.Join(sk.Tags, " ")), lowerQuery)
}

type memMemory struct {
	instanceID string
	hlc        *hlc.Clock
	mu         sync.Mutex
	items      []MemoryItem
}

func (m *memMemory) Write(_ context.Context, it MemoryItem) (MemoryItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if it.ID == "" {
		it.ID = newID()
	}
	if it.CreatedAt.IsZero() {
		it.CreatedAt = time.Now().UTC()
	}
	if it.OriginInstanceID == "" {
		it.OriginInstanceID = m.instanceID
	}
	it.LastWriterID = m.instanceID
	it.UpdatedHLC = m.hlc.Now()
	it.SyncVersion = 1
	m.items = append(m.items, it)
	return it, nil
}

func (m *memMemory) Recall(_ context.Context, q RecallQuery) ([]MemoryItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	query := strings.ToLower(strings.TrimSpace(q.Query))
	out := make([]MemoryItem, 0)
	for _, it := range m.items {
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
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (m *memMemory) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.items {
		if m.items[i].ID == id && !m.items[i].Deleted {
			m.items[i].Deleted = true
			m.items[i].LastWriterID = m.instanceID
			m.items[i].UpdatedHLC = m.hlc.Now()
			m.items[i].SyncVersion++
			return nil
		}
	}
	return ErrNotFound
}
