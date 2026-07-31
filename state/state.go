// Package state defines the persistence boundary between the open-source agent and
// any host. This is the host boundary: the agent reaches all durable state
// - sessions, skills, memory - only through the interfaces here.
//
// The open agent ships a local implementation (in-memory in memory.go; a durable
// SQLite implementation in storage/sqlite). A commercial host such as an Ion
// Alpha instance can supply its own Provider backed by a knowledge graph and
// fleet-wide learning, without this package ever importing the host.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/ionalpha/flynn/envelope"
)

// ErrNotFound is returned by stores when a requested record does not exist.
var ErrNotFound = errors.New("state: not found")

// ErrConflict is returned by a write when optimistic concurrency fails: the
// caller passed a non-zero SyncVersion that no longer matches the stored record
// (someone else wrote in between). Re-read and retry.
var ErrConflict = errors.New("state: version conflict")

// Scope locates a resource on the instance/project/workspace axis, so skills and
// memory can be partitioned and resolved most-specific-first. The zero Scope is
// the global (instance) scope. Scope is comparable.
//
// Matching a scope is exact by default: {Project: "x"} and {Project: "x",
// Workspace: "w"} are two different scopes, and a read at one does not see the
// other. Most-specific-first resolution is the opt-in widening a reader asks for
// with RecallQuery.IncludeAncestors, which walks Ancestors. Making it the default
// would silently break partitioning for every existing reader, so the widening is
// stated at the call site rather than assumed.
//
// SkillStore.List is exact-match only and has no ancestor walk yet; a caller that
// wants a skill's enclosing scopes walks Ancestors itself and lists each. Memory
// resolves hierarchically first because it is the read that runs on every turn.
type Scope struct {
	Instance  string
	Project   string
	Workspace string
}

// Ancestors returns s and the scopes enclosing it, most-specific first and ending
// at the global (zero) scope: {I,P,W}, {I,P}, {I}, {}. It is the resolution chain
// a widened read walks, and the single definition of "encloses" every backend
// shares rather than three re-derivations of it.
//
// s is always the first element and no scope repeats, so a Scope whose outer
// fields are already empty yields a shorter chain: {Project: "x"} resolves
// through {Project: "x"} then the global scope, with no phantom instance level in
// between.
func (s Scope) Ancestors() []Scope {
	out := make([]Scope, 0, 4)
	out = append(out, s)
	if s.Workspace != "" {
		s.Workspace = ""
		out = append(out, s)
	}
	if s.Project != "" {
		s.Project = ""
		out = append(out, s)
	}
	if s.Instance != "" {
		out = append(out, Scope{})
	}
	return out
}

// Depth reports how specific a scope is: 3 workspace, 2 project, 1 instance, 0
// global. Ordering by descending Depth is most-specific-first, which is how a
// backend ranks a recall widened across several scope levels.
//
// Depth reads the innermost set field rather than counting set fields, so it
// stays a total order over an ancestor chain even when an outer field is empty:
// every scope in a given chain has a distinct Depth. It is deliberately
// expressible in a backend's query language as well as in Go - see the ORDER BY
// in the SQLite store - so one rule orders every implementation.
func (s Scope) Depth() int {
	switch {
	case s.Workspace != "":
		return 3
	case s.Project != "":
		return 2
	case s.Instance != "":
		return 1
	default:
		return 0
	}
}

// Envelope is the sync/concurrency metadata carried by every persisted record,
// embedded into Session, Turn, Skill, and MemoryItem. It is the shared sync
// envelope (see the envelope package for the fields and the stamping rules):
// state records and resources carry the identical five fields under identical
// rules, which is what keeps fleet merge one discipline instead of two.
//
// SyncVersion powers optimistic concurrency: on an update, pass the version you
// read and the write fails with ErrConflict if the stored version has moved (a
// zero SyncVersion means "unconditional").
type Envelope = envelope.Envelope

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
// restarts so a crashed or disconnected run can be picked back up - the agent's
// answer to message loss in ephemeral, file-based agents.
type Session struct {
	ID        string
	Title     string
	Model     string
	CreatedAt time.Time
	UpdatedAt time.Time
	Envelope
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
	Envelope
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
	// AppendTurn appends a turn to its session, assigning ID and Seq, and bumps
	// the session's SyncVersion. It returns ErrNotFound if the session does not
	// exist.
	AppendTurn(ctx context.Context, t Turn) (Turn, error)
	// Turns returns a session's transcript in Seq order.
	Turns(ctx context.Context, sessionID string) ([]Turn, error)
	// Delete tombstones a session by ID (soft delete), or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// Skill is a reusable, versioned unit of learned procedure. Slug is unique
// within a Scope; Body is the skill content. Version is the content revision
// (for provenance/rollback), distinct from Envelope.SyncVersion (the
// concurrency/sync token).
type Skill struct {
	ID    string
	Slug  string
	Name  string
	Body  string
	Tags  []string
	Scope Scope
	// Uses and Wins are outcome evidence: how many runs recalled this skill, and
	// how many of those runs then succeeded. They let a skill be ranked and retired
	// by how well it has actually performed, not by recency alone.
	Uses int
	Wins int
	// Check is an optional shell command that verifies the skill still works, kept
	// so the skill can be re-graded later (re-run as the environment changes).
	Check     string
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
	Envelope
}

// SkillStore persists scoped skills and searches them. The durable
// implementation backs Search with full-text search (SQLite FTS5); the
// in-memory implementation does a case-insensitive substring scan.
type SkillStore interface {
	// Upsert creates or updates a skill keyed by (Scope, Slug). On update the
	// content Version is incremented, CreatedAt and OriginInstanceID preserved,
	// and SyncVersion bumped. Optimistic concurrency is opt-in: a non-zero
	// SyncVersion on the passed skill must match the stored record, else
	// ErrConflict; a zero SyncVersion writes unconditionally.
	Upsert(ctx context.Context, sk Skill) (Skill, error)
	// Get returns a skill by ID or slug, or ErrNotFound.
	Get(ctx context.Context, idOrSlug string) (Skill, error)
	// List returns the skills in a scope, ordered by slug.
	List(ctx context.Context, scope Scope) ([]Skill, error)
	// Search returns skills matching query, ordered by slug, capped at limit
	// (limit <= 0 means no cap).
	Search(ctx context.Context, query string, limit int) ([]Skill, error)
	// Delete tombstones a skill by ID or slug (soft delete), or returns
	// ErrNotFound.
	Delete(ctx context.Context, idOrSlug string) error
}

// MemoryItem is a durable fact the agent has learned, attributable to its
// sources for provenance and rollback.
type MemoryItem struct {
	ID      string
	Kind    string // e.g. "fact", "preference", "observation"
	Content string
	Scope   Scope
	// Sources is the provenance: every input this fact came from, in the order the
	// writer credits them. It is a list because a distilled item genuinely has more
	// than one origin - a curator that reads a run's tool output, a chat turn and an
	// earlier memory produces one fact from three inputs, and a single string can
	// only record one of them or a lossy join of all three.
	//
	// Provenance is what makes a purge exact rather than approximate: retiring
	// everything a compromised tool contributed to means finding every item that
	// lists it, including the ones where it was one input among several. A single
	// source field silently under-reports exactly those.
	//
	// Order is the writer's, and carries no ranking. An empty or nil Sources is an
	// item with no recorded provenance, which readers that classify by origin (see
	// the memory/guard package) treat as the least-trusted answer they can defend,
	// never as trusted.
	Sources   []string
	CreatedAt time.Time
	// ExpiresAt is the first instant this item stops being recallable; the zero
	// value never expires. It is the half-open convention RecallQuery.Since and
	// Until already use, so an item expiring at T is recalled at T-1ns and not at T.
	//
	// Expiry belongs in the record rather than in host policy because the death date
	// is known at write time and nowhere else: whoever learns "this credential
	// rotates on Friday" or "this sprint's plan is void at the end of the month"
	// knows it as they write, and a host sweeping the store later can only guess.
	// Recall omits an expired item on every backend (see MemoryStore.Recall);
	// nothing else changes, so an expired item is still on the event stream and
	// still tombstoned by Delete. Expiry hides a fact from retrieval; it does not
	// erase it, and it is not a deletion.
	//
	// Write accepts an already-expired item rather than rejecting it. A clock skew
	// or a replayed event must not turn into a write error on a store that would
	// simply never return the row.
	ExpiresAt time.Time
	// Score is how well this item matched the query that recalled it, in [0,1]
	// with 1 the best match. It is a read-side annotation, not part of the record:
	// Recall sets it, Write ignores it, and no backend persists it.
	//
	// A backend that cannot rank reports 1 for every match - "it matched, and I
	// have no opinion on how well" - rather than 0. A floor (RecallQuery.MinScore)
	// then excludes nothing on a store with no ranking to offer, instead of
	// silently emptying every recall against it.
	//
	// The scale is the backend's own. Scores order one recall's results against
	// each other; they are not comparable across backends, and not stable across
	// queries or as the corpus grows, because a term's weight depends on how rare
	// it is in the stored memory at the time of the read. A caller tuning MinScore
	// is tuning it for one backend and one corpus, not setting a portable constant.
	Score float64
	Envelope
}

// UnmarshalJSON decodes a memory item, accepting the single-valued "Source" that
// items carried before provenance became a list.
//
// The spine is durable and replayable: it still holds events written under the old
// shape, and a rebuild that decoded them into an item with no provenance would
// quietly rewrite the history it exists to reproduce. Reading the old field into a
// one-element Sources is the only decode that makes a replay of those events agree
// with the state they originally produced. A payload carrying both fields is a new
// writer's, so Sources wins.
func (m *MemoryItem) UnmarshalJSON(b []byte) error {
	// item sheds the method set, so the embedded decode below is the default one
	// and not a recursive call back into here.
	type item MemoryItem
	var v struct {
		item
		Source string
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*m = MemoryItem(v.item)
	if len(m.Sources) == 0 && v.Source != "" {
		m.Sources = []string{v.Source}
	}
	return nil
}

// RecallQuery selects memory for prefetch into context. Query is matched
// lexically (and, in vector-capable backends, semantically); Scope narrows the
// search; Limit caps results (<= 0 means no cap).
type RecallQuery struct {
	Query string
	// Scope narrows the search to one exact scope. The zero Scope is the
	// unfiltered read that spans every scope, not the global scope: recall is a
	// search, and searching everything is the useful default for a caller that has
	// no scope in hand.
	Scope Scope
	// IncludeAncestors widens a scoped recall to Scope.Ancestors(), so an item
	// written at {Project: X} is recalled by a reader at {Project: X, Workspace: W}.
	// This is what makes memory usable for an agent running workspace-under-project:
	// the durable, general facts are written at the outer scope and every inner
	// scope should see them. Results come back most-specific-first (see Recall).
	//
	// It has no effect on an unfiltered recall (a zero Scope already spans
	// everything).
	IncludeAncestors bool
	// Kinds restricts the recall to these MemoryItem kinds; empty matches every
	// kind. Injecting the user's preferences into a prompt should not have to drag
	// every observation along with them, and over-fetching to filter afterwards
	// pays the retrieval cost for rows it then throws away.
	Kinds []string
	// Since and Until bound CreatedAt to the half-open window [Since, Until). A
	// zero Since has no lower bound and a zero Until no upper bound, so the zero
	// query still spans all of history.
	Since time.Time
	Until time.Time
	// MinScore drops results the backend scored below it, the relevance floor that
	// makes Limit a "top K of what is actually relevant" rather than "K rows,
	// however weak". Zero (the default) admits everything. See MemoryItem.Score
	// for what a backend with no ranking reports.
	MinScore float64
	// Order selects the ranking; the zero value is the recency order Recall has
	// always returned.
	Order RecallOrder
	Limit int
}

// RecallOrder selects how Recall ranks the results it returns.
type RecallOrder int

const (
	// OrderRecent returns the most recent memory first. It is the zero value, and
	// the right default for "what has happened lately".
	OrderRecent RecallOrder = iota
	// OrderRelevance returns the best-matching memory first, recency breaking
	// ties. Combined with Limit it is top-K-by-relevance, which is the lever that
	// decides how much a per-turn recall costs in context: recency order caps how
	// many rows come back but not how good they are, so the strongest match can be
	// truncated away by a row that merely arrived later.
	//
	// Against a backend that cannot rank, every match scores 1 and this degrades
	// to recency rather than to an arbitrary order.
	OrderRelevance
)

// ExpiredAt reports whether the item has reached its expiry as of now. The zero
// ExpiresAt never expires, and expiry is inclusive of the instant itself, matching
// the half-open window RecallQuery.Until describes.
//
// It is the single definition of "expired": Selects calls it, and a backend that
// evaluates expiry in its own query language is answering this question and must
// answer it identically.
func (m MemoryItem) ExpiredAt(now time.Time) bool {
	return !m.ExpiresAt.IsZero() && !now.Before(m.ExpiresAt)
}

// Selects reports whether it satisfies the query's kind filter and time window and
// has not expired as of now. These are the per-item selectors, separate from scope
// resolution (a set membership test a backend does once, via ScopeChain) and from
// the lexical match (the backend's own, and what Score grades). Backends that
// filter in Go share it so the implementations cannot drift; one that pushes the
// same predicates into its query language must produce the identical answer.
//
// now is the backend's own reading of the current time, taken once per recall so
// every item in one result set is judged against the same instant. Expiry rides
// here rather than in a separate pass because a backend that forgot the separate
// pass would serve expired memory and pass every other test.
func (q RecallQuery) Selects(it MemoryItem, now time.Time) bool {
	if it.ExpiredAt(now) {
		return false
	}
	if len(q.Kinds) > 0 && !slices.Contains(q.Kinds, it.Kind) {
		return false
	}
	if !q.Since.IsZero() && it.CreatedAt.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && !it.CreatedAt.Before(q.Until) {
		return false
	}
	return true
}

// ScopeChain returns the scopes a recall reads, most-specific first, or nil when
// the recall spans every scope (a zero Scope). Backends resolve through it so
// "most-specific-first" has one definition rather than one per backend.
func (q RecallQuery) ScopeChain() []Scope {
	switch {
	case q.Scope == (Scope{}):
		return nil
	case !q.IncludeAncestors:
		return []Scope{q.Scope}
	default:
		return q.Scope.Ancestors()
	}
}

// RanksByScope reports whether this recall reads more than one scope level and so
// orders by scope before recency. It is false for every query that resolves to a
// single scope, including an unfiltered one, which is what keeps widening from
// changing the order of reads that did not ask for it.
func (q RecallQuery) RanksByScope() bool { return len(q.ScopeChain()) > 1 }

// MemoryStore persists and recalls memory. The durable implementation combines
// lexical (FTS5) and vector (chromem-go) recall; the in-memory implementation
// does a case-insensitive substring scan, most-recent first.
//
// Memory is append-only: a Write is always a new record, never an edit of an
// earlier one, and Delete tombstones rather than erases. Two consequences are
// deliberately the host's to resolve, not this interface's, and are stated here so
// a backend does not quietly invent its own answer:
//
//   - Dedup and supersede. Two writes can assert contradictory facts and both
//     stay live; nothing here links a correction to what it corrects, and Recall
//     will return both. A host that needs one truth per subject decides what
//     counts as the same subject and retires the loser with Delete, because the
//     rule is domain knowledge (one preference per key, but every observation
//     kept) that a store cannot infer from content.
//   - Retention. Nothing here ages memory out on its own. An item with a known
//     death date carries MemoryItem.ExpiresAt and stops being recalled without
//     any sweep; everything else accumulates until a host deletes it.
//
// What a backend may not do is answer these itself: silently dropping a write it
// judges duplicate, or garbage-collecting old items, makes the store lossy in a
// way no caller asked for and no other backend matches.
type MemoryStore interface {
	// Write persists a memory item, assigning an ID if one is not set.
	Write(ctx context.Context, m MemoryItem) (MemoryItem, error)
	// Recall returns memory matching the query, most-recent first, with the item
	// ID as the final tiebreak so the order is total and deterministic. A recall
	// widened over several scope levels (RecallQuery.IncludeAncestors) orders by
	// scope first, most-specific first, so the nearest memory wins; see
	// SortRecall, which every backend sorts through.
	//
	// Tombstoned items and items whose MemoryItem.ExpiresAt has passed are never
	// returned. Expiry is judged against one reading of the clock per call, so a
	// single result set is internally consistent.
	Recall(ctx context.Context, q RecallQuery) ([]MemoryItem, error)
	// Delete tombstones a memory item by ID (soft delete), or returns
	// ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// SortRecall orders q's results into the order MemoryStore.Recall promises, in
// place. Every backend sorts through it, so the contract has one implementation
// instead of three that drift.
//
// The keys, in order: relevance when q asked for it, then scope when the read
// widened across levels, then recency, then ID to make the order total. Ranking
// by scope is what makes widening useful - a workspace's own memory outranks the
// project-wide memory it inherits, however old that is - and it is skipped
// entirely for a read that did not widen, so an unfiltered or single-scope recall
// keeps the plain most-recent-first order it has always had.
func SortRecall(q RecallQuery, items []MemoryItem) {
	byScore := q.Order == OrderRelevance
	byScope := q.RanksByScope()
	sort.Slice(items, func(i, j int) bool {
		if byScore && items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		if byScope {
			if di, dj := items[i].Scope.Depth(), items[j].Scope.Depth(); di != dj {
				return di > dj
			}
		}
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID // total order: deterministic reads regardless of store iteration
	})
}
