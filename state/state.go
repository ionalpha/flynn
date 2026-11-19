// Package state is the host boundary: the agent reaches all durable sessions,
// skills and memory only through these interfaces.
//
// The agent ships [NewMemory] and a SQLite Provider in storage/sqlite. A host
// can supply its own Provider without this package importing the host.
package state

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/ionalpha/flynn/envelope"
)

// ErrNotFound means the requested record does not exist.
var ErrNotFound = errors.New("state: not found")

// ErrConflict means a write's non-zero SyncVersion no longer matches the
// stored record. Re-read and retry.
var ErrConflict = errors.New("state: version conflict")

// Scope partitions a store into three nested levels, resolved
// most-specific-first. The zero Scope is global, and Scope is comparable.
//
// Instance (one installation) contains Project, which contains Workspace.
// Labels are opaque and never interpreted; empty means not narrowed at that
// level, and a host decides what to put in each. The levels are fixed so a
// store can order by [Scope.Depth].
//
// Matching is exact: {Project: "x"} and {Project: "x", Workspace: "w"} differ.
// Widening is opt-in through [RecallQuery].IncludeAncestors.
type Scope struct {
	Instance  string
	Project   string
	Workspace string
}

// BundledScope holds only the skills shipped in the binary, apart from the
// zero scope where the learning loop writes. Its instance name is not a legal
// instance identifier, so no real instance lands here.
//
// Reads resolve by slug alone and Get takes the earliest row on a tie, which
// is always the bundled one. A learned skill reusing a bundled slug would be
// stored but unreachable, so learn.Curate refuses it.
var BundledScope = Scope{Instance: "@bundled"}

// Ancestors returns s and its enclosing scopes, ending at the zero scope:
// {I,P,W}, {I,P}, {I}, {}. Every backend uses it as the definition of
// "encloses". No scope repeats, so {Project: "x"} yields itself then {}.
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

// Depth is 3 for workspace, 2 project, 1 instance, 0 global; a widened recall
// ranks by it descending. It reads the innermost set field, so scopes in one
// [Scope.Ancestors] chain always differ, and SQL can express it (see the
// SQLite store's ORDER BY).
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

// Envelope is the sync metadata on every persisted record, shared with
// resources so fleet merge follows one set of rules (see package envelope).
// Pass the SyncVersion you read on an update to get [ErrConflict] if it moved;
// zero is unconditional.
type Envelope = envelope.Envelope

// Provider is the one interface a host implements to back the agent's
// storage; the agent depends on no concrete store.
type Provider interface {
	// Name identifies the backend in diagnostics, e.g. "sqlite".
	Name() string
	Sessions() SessionStore
	Skills() SkillStore
	Memory() MemoryStore
	Close() error
}

// Session is a conversation that survives restarts, so a crashed run can be
// resumed.
type Session struct {
	ID        string
	Title     string
	Model     string
	CreatedAt time.Time
	UpdatedAt time.Time
	Envelope
}

// Turn is one transcript entry. The store assigns Seq, monotonic within a
// session.
type Turn struct {
	ID        string
	SessionID string
	Seq       int64
	Role      string // "user", "assistant", "tool", or "system"
	Content   string
	CreatedAt time.Time
	Envelope
}

// SessionStore persists conversations. Turns are append-only; resuming reads
// them back in Seq order.
type SessionStore interface {
	// Create assigns an ID if none is set.
	Create(ctx context.Context, s Session) (Session, error)
	// Get returns the session by ID, or ErrNotFound.
	Get(ctx context.Context, id string) (Session, error)
	// List returns all sessions, oldest first.
	List(ctx context.Context) ([]Session, error)
	// AppendTurn assigns ID and Seq and bumps the session's SyncVersion, or
	// returns ErrNotFound.
	AppendTurn(ctx context.Context, t Turn) (Turn, error)
	// Turns returns the transcript in Seq order.
	Turns(ctx context.Context, sessionID string) ([]Turn, error)
	// Delete tombstones a session, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// Skill is a versioned learned procedure. Slug is unique within a Scope.
// Version is the content revision, separate from the SyncVersion used for
// concurrency.
type Skill struct {
	ID   string
	Slug string
	Name string
	// Description says what the skill is for and when to use it. It is the only
	// text loaded at discovery, so a skill without one is never activated. It is
	// capped at skillmd.MaxDescriptionLen.
	Description string
	Body        string
	Tags        []string
	Scope       Scope
	// Offers counts runs shown the description, Reads runs that then read the
	// body, Wins reads on runs that succeeded. Ranking and retirement use Reads
	// and Wins; offered and never read means the description is not working.
	//
	// Offers keeps the "Uses" wire key so older records keep their count. Wins
	// uses "ReadWins" so the old "Wins" key, which counted something else,
	// decodes into nothing.
	Offers int `json:"Uses"`
	Reads  int `json:"Reads"`
	Wins   int `json:"ReadWins"`
	// Check is an optional shell command that re-grades the skill.
	Check string
	// Triggers declare what a run does that makes the host load Body into the next
	// turn. Empty means the skill is only ever loaded by the model or by name.
	Triggers  []SkillTrigger `json:",omitempty"`
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
	Envelope
}

// SkillStore persists and searches scoped skills. SQLite searches with FTS5,
// the in-memory store by case-insensitive substring.
type SkillStore interface {
	// Upsert writes by (Scope, Slug). An update increments Version, keeps
	// CreatedAt and OriginInstanceID, and bumps SyncVersion. A non-zero
	// SyncVersion must match the stored one, else ErrConflict.
	Upsert(ctx context.Context, sk Skill) (Skill, error)
	// Get returns a skill by ID or slug, or ErrNotFound.
	Get(ctx context.Context, idOrSlug string) (Skill, error)
	// List returns skills in exactly scope, by slug, with no ancestor walk.
	List(ctx context.Context, scope Scope) ([]Skill, error)
	// Search matches name, description, body or tags, ordered by slug and
	// capped at limit (<= 0 is no cap).
	Search(ctx context.Context, query string, limit int) ([]Skill, error)
	// Delete tombstones a skill by ID or slug, or returns ErrNotFound.
	Delete(ctx context.Context, idOrSlug string) error
}

// ErrInvalid means a write's record is malformed, such as an anchor missing
// its kind or id.
var ErrInvalid = errors.New("state: invalid record")

// Anchor points a memory item at something outside this store that the fact
// is about, so reading that thing can recall what was learned about it.
//
// Anchors are opaque: stored, indexed and matched, never resolved. A dangling
// anchor is valid and its memory stays recallable. Validation is shape only
// (see [Anchor.Valid]).
type Anchor struct {
	// Kind is the referent's type in the referring system's vocabulary, so ids
	// from two systems cannot collide.
	Kind string
	// ID is the referent's opaque id within Kind.
	ID string
}

// Valid reports whether both halves are present, which is all a store may
// check. Either half empty is refused at the write, since it could never match.
func (a Anchor) Valid() bool { return a.Kind != "" && a.ID != "" }

// AnchorKindSkill anchors a memory to a skill in this store. It is the only
// kind defined here because a skill is the only referent whose id this package
// issues; every other kind belongs to whoever refers with it.
const AnchorKindSkill = "skill"

// SkillAnchor returns the anchor for skill id, or the zero Anchor for an empty
// id, which [NormalizeAnchors] refuses. It keys on the id because a slug can be
// renamed and can match skills in several scopes.
func SkillAnchor(id string) Anchor {
	if id == "" {
		return Anchor{}
	}
	return Anchor{Kind: AnchorKindSkill, ID: id}
}

// SkillAnchors returns the anchors for ids, skipping empty ones, or nil.
func SkillAnchors(ids []string) []Anchor {
	if len(ids) == 0 {
		return nil
	}
	out := make([]Anchor, 0, len(ids))
	for _, id := range ids {
		if a := SkillAnchor(id); a.Valid() {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeAnchors sorts by kind then id and drops duplicates, or returns
// [ErrInvalid] for an invalid anchor. Empty input gives nil. Anchor order
// carries no meaning, so one set always hashes the same. Every write path
// calls it, so backends agree on what a duplicate is.
func NormalizeAnchors(in []Anchor) ([]Anchor, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]Anchor, 0, len(in))
	for _, a := range in {
		if !a.Valid() {
			return nil, fmt.Errorf("%w: anchor needs both a kind and an id, got %+v", ErrInvalid, a)
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(x, y Anchor) int {
		if c := cmp.Compare(x.Kind, y.Kind); c != 0 {
			return c
		}
		return cmp.Compare(x.ID, y.ID)
	})
	return slices.Compact(out), nil
}

// NormalizeSubject lower-cases a subject and collapses each run of
// non-letters and non-digits (by Unicode category) to one hyphen, trimming the
// ends: "DB choice" and "db_choice" both give "db-choice". Empty stays empty.
// Input with no letters or digits is [ErrInvalid], since storing it as
// unsubjected would hide the mistake. Every write path calls it.
func NormalizeSubject(in string) (string, error) {
	var b strings.Builder
	b.Grow(len(in))
	dash := false
	for _, r := range in {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" && strings.TrimSpace(in) != "" {
		return "", fmt.Errorf("%w: subject %q has no letters or digits to key on", ErrInvalid, in)
	}
	return out, nil
}

// NormalizeSupersedes sorts and dedupes the ids, or returns [ErrInvalid] for a
// blank id or for self, since a self-loop never ends a reader's walk back
// along the chain. Empty input gives nil.
func NormalizeSupersedes(in []string, self string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	for _, id := range in {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("%w: superseded id is blank", ErrInvalid)
		}
		if self != "" && id == self {
			return nil, fmt.Errorf("%w: item %s cannot supersede itself", ErrInvalid, self)
		}
		out = append(out, id)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// MemoryItem is a learned fact, attributed to its sources for rollback.
type MemoryItem struct {
	ID      string
	Kind    string // e.g. "fact", "preference", "observation"
	Content string
	Scope   Scope
	// Subject is the topic slug a host groups writes on to decide whether a new
	// fact replaces or joins earlier ones (see [MemoryStore]). It is normalized
	// by [NormalizeSubject] so two spellings share a chain. Empty means no
	// particular subject.
	Subject string
	// Supersedes lists the ids this item corrects, so a reader can tell a
	// revision from a disagreement. It retires nothing: both stay recallable
	// until the host calls Delete, and a backend must never infer a delete from
	// it. Ids are never resolved, so one naming a purged item is not an error.
	Supersedes []string
	// Sources lists every input the fact came from, unranked, so a purge finds
	// each item that cites a compromised input. Empty Sources means unknown
	// provenance, which origin classifiers (package memory/guard) treat as
	// least trusted.
	Sources []string
	// Anchors are what the fact is about, matched by [RecallQuery].Anchors and
	// stored sorted and deduplicated. A purge follows Sources and ignores them.
	Anchors []Anchor
	// Tainted records that attacker-influenceable input was in the producing
	// context, whatever Sources says, since an agent can restate a poisoned
	// input as its own conclusion. It is set at write and never cleared.
	//
	// A tainted fact is stored, recalled and used normally but is kept out of
	// the wake digest, which reaches every reader unasked (see
	// guard.PushEligibility). The host does the marking.
	Tainted   bool
	CreatedAt time.Time
	// ExpiresAt is the first instant Recall omits the item; zero never expires.
	// Expiry only hides the item: it stays on the event stream and Delete still
	// applies. Write accepts an expired item so clock skew or replay is not an
	// error.
	ExpiresAt time.Time
	// Score is the recall match quality in [0,1], 1 best. Recall sets it; it is
	// never persisted. A backend that cannot rank reports 1, so
	// [RecallQuery].MinScore excludes nothing there. Scores only order one
	// recall and do not compare across backends, queries or corpus growth.
	Score float64
	Envelope
}

// UnmarshalJSON also accepts the single-valued "Source" key, which spine
// events still carry and replay must honour, as a one-element Sources. When
// both are present Sources wins.
func (m *MemoryItem) UnmarshalJSON(b []byte) error {
	// item sheds the method set, so this decode does not recurse.
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

// RecallQuery selects memory to prefetch. Query matches lexically, and
// semantically where the backend has vectors. Limit <= 0 is no cap.
type RecallQuery struct {
	Query string
	// Scope is the one exact scope read. The zero Scope reads every scope,
	// which differs from reading the global scope alone.
	Scope Scope
	// IncludeAncestors widens a scoped recall to [Scope.Ancestors],
	// most-specific first. It does nothing for the zero Scope.
	IncludeAncestors bool
	// Kinds restricts the recall to these kinds; empty matches all.
	Kinds []string
	// Subjects restricts the recall to these subjects; empty matches all. Values
	// match exactly and are not normalized here, so pass outside input through
	// [NormalizeSubject]. A write policy reads by this, since a lexical query
	// finds mentions and misses items that never spell the subject out.
	Subjects []string
	// Anchors restricts the recall to items carrying any one of these, matched
	// exactly on both halves; empty matches all. An anchored recall usually has
	// an empty Query.
	Anchors []Anchor
	// Since and Until bound CreatedAt to the half-open window [Since, Until). A
	// zero Since has no lower bound and a zero Until no upper bound, so the zero
	// query still spans all of history.
	Since time.Time
	Until time.Time
	// MinScore drops results scored below it, so Limit keeps only relevant
	// rows. Zero admits all (see [MemoryItem].Score).
	MinScore float64
	// Order selects the ranking; zero is [OrderRecent].
	Order RecallOrder
	Limit int
}

// RecallOrder selects how Recall ranks the results it returns.
type RecallOrder int

const (
	// OrderRecent returns the most recent memory first.
	OrderRecent RecallOrder = iota
	// OrderRelevance returns the best match first, recency breaking ties, so
	// Limit cannot truncate the strongest match in favour of a newer row. A
	// backend that cannot rank degrades it to recency.
	OrderRelevance
)

// ExpiredAt reports whether now is at or past ExpiresAt, matching the
// half-open [RecallQuery].Until. It defines "expired"; a backend computing
// expiry in its own query language must agree with it.
func (m MemoryItem) ExpiredAt(now time.Time) bool {
	return !m.ExpiresAt.IsZero() && !now.Before(m.ExpiresAt)
}

// Selects reports whether it passes the kind, subject, anchor and time filters
// and is unexpired at now, read once per recall. Scope ([RecallQuery.ScopeChain])
// and the lexical match are checked elsewhere. A backend evaluating these in
// its own query language must agree with it.
func (q RecallQuery) Selects(it MemoryItem, now time.Time) bool {
	if it.ExpiredAt(now) {
		return false
	}
	if len(q.Kinds) > 0 && !slices.Contains(q.Kinds, it.Kind) {
		return false
	}
	if len(q.Subjects) > 0 && !slices.Contains(q.Subjects, it.Subject) {
		return false
	}
	if !q.matchesAnchors(it) {
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

func (q RecallQuery) matchesAnchors(it MemoryItem) bool {
	if len(q.Anchors) == 0 {
		return true
	}
	for _, want := range q.Anchors {
		if slices.Contains(it.Anchors, want) {
			return true
		}
	}
	return false
}

// ScopeChain returns the scopes a recall reads, most-specific first, or nil
// for the zero Scope. Every backend resolves scope through it.
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

// RanksByScope reports whether the recall reads several scope levels and so
// orders by scope before recency. It is false for single-scope and unfiltered
// reads, so their order is unchanged by widening.
func (q RecallQuery) RanksByScope() bool { return len(q.ScopeChain()) > 1 }

// MemoryStore persists and recalls memory. SQLite combines FTS5 and
// chromem-go vectors; the in-memory store scans substrings.
//
// Memory is append-only: each Write is a new record and Delete tombstones.
// Dedup and retention belong to the host, so a backend must keep
// contradictory writes and never age items out. A host wanting one answer per
// subject groups by [MemoryItem].Subject, links with [MemoryItem].Supersedes,
// and deletes the loser; [MemoryItem].ExpiresAt covers a known end date.
type MemoryStore interface {
	// Write persists a memory item, assigning an ID if one is not set.
	Write(ctx context.Context, m MemoryItem) (MemoryItem, error)
	// Recall returns matches in [SortRecall] order. Tombstoned and expired
	// items are never returned, with expiry judged at one clock reading per
	// call.
	Recall(ctx context.Context, q RecallQuery) ([]MemoryItem, error)
	// Delete tombstones an item, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error

	// RecordPush counts one unrequested showing of each item, against the
	// writing instance (see [MemoryUsage]). Only the caller reports pushes;
	// Recall records nothing. An unknown or tombstoned id is [ErrNotFound] and
	// records nothing for the whole set. An empty list is a no-op.
	RecordPush(ctx context.Context, memoryIDs []string) error
	// RecordUse counts one use, which the caller marks UsageOrganic (the reader
	// found it) or UsagePrimed (it was pushed first). Any other origin is
	// ErrInvalid, with no default, since the split is the measurement. An
	// unknown or tombstoned id is ErrNotFound.
	RecordUse(ctx context.Context, memoryID string, origin UsageOrigin) error
	// Usage returns per-instance rows in SortUsage order; an empty list returns
	// all rows. An unused item has no row, so an untouched corpus stays
	// distinguishable from an ignored one. Rows survive tombstones.
	Usage(ctx context.Context, memoryIDs []string) ([]MemoryUsage, error)

	// Promote records a reviewer's decision on whether an item may be pushed
	// unasked and returns the row (see [MemoryPromotion]). It replaces any
	// earlier decision, so revoking is Promoted false; history is on the event
	// stream. A missing item or reviewer is [ErrInvalid] and an unknown or
	// tombstoned item [ErrNotFound], keeping the audit trail whole.
	Promote(ctx context.Context, d PromotionDecision) (MemoryPromotion, error)
	// Promotions returns rows in SortPromotions order; an empty list returns
	// all. An unreviewed item has no row, which PromotedSet treats as not
	// promoted. Rows survive tombstones.
	Promotions(ctx context.Context, memoryIDs []string) ([]MemoryPromotion, error)
}

// SortRecall sorts items in place into the order every backend's Recall
// returns: score if q asks for relevance, then scope depth if
// [RecallQuery.RanksByScope], then newest first, then ID so the order is
// total. Scope depth lets a workspace's memory outrank older inherited
// project memory.
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
		return items[i].ID < items[j].ID
	})
}
