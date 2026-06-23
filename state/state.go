// Package state defines the persistence seam between the open-source agent and
// any host. This is the open-core boundary: the agent reaches all durable state
// — sessions, skills, memory — only through the interfaces here.
//
// The open agent ships a local implementation (in-memory in memory.go; a durable
// SQLite implementation lands in a follow-up). A commercial host such as an Ion
// Alpha instance can supply its own Provider backed by a knowledge graph and
// fleet-wide learning, without this package ever importing the host.
package state

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by stores when a requested record does not exist.
var ErrNotFound = errors.New("state: not found")

// Scope locates a resource on the instance/project/workspace axis, so skills and
// memory can be partitioned and resolved most-specific-first. The zero Scope is
// the global (instance) scope. Scope is comparable.
type Scope struct {
	Instance  string
	Project   string
	Workspace string
}

// Provider is the agent's durable backend: the single interface a host
// implements to back the agent with its own storage. The agent never depends on
// a concrete store, only on this Provider and the stores it returns.
type Provider interface {
	// Name identifies the backend for diagnostics, e.g. "memory", "sqlite".
	Name() string
	// Sessions returns the durable conversation store.
	Sessions() SessionStore
	// Skills returns the scoped, searchable skill store.
	Skills() SkillStore
	// Memory returns the durable memory store.
	Memory() MemoryStore
	// Close releases any resources held by the provider.
	Close() error
}

// Session is a durable, resumable conversation. Sessions survive process
// restarts so a crashed or disconnected run can be picked back up — the agent's
// answer to message loss in ephemeral, file-based agents.
type Session struct {
	ID        string
	Title     string
	Model     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Turn is one entry in a session's ordered transcript. Seq is assigned by the
// store and is monotonic within a session.
type Turn struct {
	ID        string
	SessionID string
	Seq       int64
	Role      string // "user", "assistant", "tool", or "system"
	Content   string
	CreatedAt time.Time
}

// SessionStore persists conversations and their transcripts. Turns are
// append-only; resuming a session means reading its turns back in Seq order.
type SessionStore interface {
	// Create persists a new session, assigning an ID if one is not set.
	Create(ctx context.Context, s Session) (Session, error)
	// Get returns the session by ID, or ErrNotFound.
	Get(ctx context.Context, id string) (Session, error)
	// List returns all sessions, oldest first.
	List(ctx context.Context) ([]Session, error)
	// AppendTurn appends a turn to its session, assigning ID and Seq. It returns
	// ErrNotFound if the session does not exist.
	AppendTurn(ctx context.Context, t Turn) (Turn, error)
	// Turns returns a session's transcript in Seq order.
	Turns(ctx context.Context, sessionID string) ([]Turn, error)
}

// Skill is a reusable, versioned unit of learned procedure. Slug is unique
// within a Scope; Body is the skill content.
type Skill struct {
	ID        string
	Slug      string
	Name      string
	Body      string
	Tags      []string
	Scope     Scope
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SkillStore persists scoped skills and searches them. The durable
// implementation backs Search with full-text search (SQLite FTS5); the
// in-memory implementation does a case-insensitive substring scan.
type SkillStore interface {
	// Upsert creates or updates a skill keyed by (Scope, Slug). On update the
	// Version is incremented and CreatedAt preserved.
	Upsert(ctx context.Context, sk Skill) (Skill, error)
	// Get returns a skill by ID or slug, or ErrNotFound.
	Get(ctx context.Context, idOrSlug string) (Skill, error)
	// List returns the skills in a scope, ordered by slug.
	List(ctx context.Context, scope Scope) ([]Skill, error)
	// Search returns skills matching query, ordered by slug, capped at limit
	// (limit <= 0 means no cap).
	Search(ctx context.Context, query string, limit int) ([]Skill, error)
}

// MemoryItem is a durable fact the agent has learned, attributable to its
// source for provenance and rollback.
type MemoryItem struct {
	ID        string
	Kind      string // e.g. "fact", "preference", "observation"
	Content   string
	Scope     Scope
	Source    string // provenance: where this memory came from
	CreatedAt time.Time
}

// RecallQuery selects memory for prefetch into context. Query is matched
// lexically (and, in vector-capable backends, semantically); Scope narrows the
// search; Limit caps results (<= 0 means no cap).
type RecallQuery struct {
	Query string
	Scope Scope
	Limit int
}

// MemoryStore persists and recalls memory. The durable implementation combines
// lexical (FTS5) and vector (chromem-go) recall; the in-memory implementation
// does a case-insensitive substring scan, most-recent first.
type MemoryStore interface {
	// Write persists a memory item, assigning an ID if one is not set.
	Write(ctx context.Context, m MemoryItem) (MemoryItem, error)
	// Recall returns memory matching the query.
	Recall(ctx context.Context, q RecallQuery) ([]MemoryItem, error)
}
