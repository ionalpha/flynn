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

// ErrNotFound is returned by stores when a requested record does not exist.
var ErrNotFound = errors.New("state: not found")

// ErrConflict is returned by a write when optimistic concurrency fails: the
// caller passed a non-zero SyncVersion that no longer matches the stored record
// (someone else wrote in between). Re-read and retry.
var ErrConflict = errors.New("state: version conflict")

// Scope partitions a store into three nested levels so skills and memory can be
// kept apart and resolved most-specific-first. The zero Scope is the global
// scope, which is where a single-node agent that partitions nothing keeps
// everything. Scope is comparable.
//
// The three levels are Flynn's own and carry exactly one meaning here:
// containment. Instance is the widest, one installation of the agent, the whole
// of what a single operator runs. Project sits inside an instance and Workspace
// inside a project. Each level is an opaque label that Flynn stores and compares
// and never interprets, and an empty label means "not narrowed at this level"
// rather than a level named "". Ancestors is the only rule about them: the walk
// from a scope to the global one, which is what "encloses" means for every
// backend.
//
// Nothing here is a taxonomy borrowed from whoever embeds Flynn. A host with its
// own hierarchy chooses which of its levels to write into which label, or leaves
// them empty and keeps one flat store; either way Flynn's behaviour is defined
// without knowing what the labels stand for. Three fixed levels rather than an
// arbitrary path is a deliberate limit: depth is then a fixed expression a store
// can order by (see Depth), which an unbounded path could not be.
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

// BundledScope is where skills shipped inside the binary live, and nothing else
// may be written there. It exists because the agent's own scopes are occupied:
// the CLI lists, regrades and distills at the zero scope, so a pack seeded there
// would share a slug namespace with the learning loop's output.
//
// The instance name is not a legal instance identifier - identifiers are host
// names and UUIDs - so no real instance can land in this scope by accident, and a
// generator drawing instance names from an alphabet cannot produce it either.
//
// Reserving it is a correctness boundary rather than a naming convention. Skills
// are keyed by (Scope, Slug) on write but resolved by slug alone on read, and Get
// breaks a cross-scope slug tie by taking the earliest created row. A bundled
// skill is seeded at install, so it is always the earliest: were the loop allowed
// to mint a learned skill with a bundled skill's slug, every later read of that
// slug would return the bundled record, and the learned one would be unreachable
// while still counting as stored. learn.Curate enforces the boundary from the
// write side; see the guards there for what is refused and why.
var BundledScope = Scope{Instance: "@bundled"}

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
	ID   string
	Slug string
	Name string
	// Description is the one-paragraph statement of what the skill is for and when
	// to reach for it. The Agent Skills specification requires it, and it is the
	// only text a conformant runtime loads at discovery, so it is what activation
	// keys on: a skill with no description is invisible until something else has
	// already decided to read its body. Kept separate from Body for that reason,
	// and bounded by skillmd.MaxDescriptionLen wherever a skill is minted.
	Description string
	Body        string
	Tags        []string
	Scope       Scope
	// Offers, Reads and Wins are the three separable facts about a skill, and the
	// reason there are three of them is that the first is not evidence about the
	// skill at all. Offers counts the runs that were shown this skill's name and
	// description; Reads counts the runs that then asked for its body; Wins counts
	// the reads on runs that went on to succeed. Ranking and retirement key on
	// Reads and Wins, so a skill is graded on runs that took it up rather than on
	// runs whose objective happened to share a keyword with it.
	//
	// Offers is kept because offered-and-never-read is a real signal with a real
	// repair: it says a description is failing to sell a skill that might have
	// helped, which is an authoring defect rather than a bad skill.
	//
	// The wire keys are deliberate. `Uses` was the old counter, and what it
	// actually accumulated was one increment per run the skill was injected into,
	// which is exactly an offer; it decodes into Offers so a record written before
	// this split keeps the count it really held. Wins used to mean offers on
	// successful runs, a quantity with no meaning under the new definition, so it
	// is left on the old key and decodes into nothing.
	Offers int `json:"Uses"`
	Reads  int `json:"Reads"`
	Wins   int `json:"ReadWins"`
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
	// (limit <= 0 means no cap). A skill matches on its name, description, body
	// or tags; the description is included because it is the text discovery
	// keys on.
	Search(ctx context.Context, query string, limit int) ([]Skill, error)
	// Delete tombstones a skill by ID or slug (soft delete), or returns
	// ErrNotFound.
	Delete(ctx context.Context, idOrSlug string) error
}

// ErrInvalid is returned by a write whose record is malformed in a way no backend
// should have to store: today, an anchor missing its kind or its id.
var ErrInvalid = errors.New("state: invalid record")

// Anchor is a reference from a memory item to something outside this store: the
// piece of work, the record, the file the fact is about. It is the cue that makes
// recall associative rather than purely lexical - reading the anchored thing can
// surface what was learned about it, without the reader having to suspect the fact
// exists and go looking for it.
//
// An anchor is deliberately opaque. Kind and ID are the referring system's own
// vocabulary and identifiers, and this package neither interprets them nor resolves
// them. It cannot: whatever owns the referent is outside Flynn, and a store that
// pretended otherwise would either need a resolver it has no way to obtain or would
// have to enumerate somebody else's record types in this file. So the rule is
// narrow and total: anchors are stored, indexed and matched, never dereferenced.
//
// The consequence worth stating plainly is that a dangling anchor is a normal
// state, not an error. The referent can be deleted, renamed or never have existed;
// the memory remains valid and recallable, and matching it by anchor simply stops
// returning it once nobody asks with that ref. Validation is therefore shape only
// (see Anchor.Valid).
type Anchor struct {
	// Kind names the referent's type in the referring system's vocabulary, so ids
	// from two different systems cannot collide.
	Kind string
	// ID identifies the referent within that kind. It is an opaque string: a UUID,
	// a path, a URL, whatever the owner uses.
	ID string
}

// Valid reports whether the anchor is well formed: both halves present. This is
// the whole of what a store may check. An anchor with an empty kind is ambiguous
// against every other system's ids, and one with an empty id refers to nothing, so
// both are rejected at the write rather than stored as a ref that can never match.
func (a Anchor) Valid() bool { return a.Kind != "" && a.ID != "" }

// AnchorKindSkill is the anchor kind for a skill in this store: a memory anchored
// with it is about the procedure that skill holds.
//
// It is the one anchor kind named in this package, and it is named here because it
// is the one referent Flynn owns. Every other kind belongs to whoever refers with
// it (see Anchor), and this file does not enumerate other systems' record types.
// A skill is not another system's record: it is a row in this store, with an id
// this package issued, so Flynn can both write the anchor and read it back without
// a host present. That is the whole reason the ride-along works standalone.
const AnchorKindSkill = "skill"

// SkillAnchor returns the anchor for the skill with this id, or the zero Anchor for
// an empty id, so a caller can build anchors from a list of ids without filtering
// it first (an invalid anchor is refused at the write, by NormalizeAnchors).
//
// Ids rather than slugs: a slug is renameable and resolves across scopes, so two
// skills can answer to one slug and a renamed skill would silently drop every
// memory anchored to it. The id is issued once and never moves.
func SkillAnchor(id string) Anchor {
	if id == "" {
		return Anchor{}
	}
	return Anchor{Kind: AnchorKindSkill, ID: id}
}

// SkillAnchors returns the anchors for these skill ids, skipping empty ones. Nil in,
// nil out.
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

// NormalizeAnchors returns the anchors in canonical form - sorted by kind then id,
// with exact duplicates collapsed - or ErrInvalid if any of them is not Valid.
// Nil and empty both normalize to nil, so an item with no anchors has one
// representation rather than two.
//
// Canonicalizing rather than preserving the writer's order is what makes the same
// anchor set encode, and therefore content-hash, identically on every write. Order
// carries no meaning here (unlike MemoryItem.Sources, where it is the writer's
// credit), so there is nothing to lose by fixing it and a stable hash to gain.
//
// Every write path normalizes through this, so a backend cannot invent its own
// answer to what a duplicate anchor means.
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

// NormalizeSubject returns the canonical form of a subject slug: lower-cased,
// with each run of characters that is neither a letter nor a digit collapsed to a
// single hyphen and the ends trimmed. "DB choice" and "db_choice" both normalize
// to "db-choice", so two writers naming the same topic in their own house style
// key on the same subject rather than forking it.
//
// An empty input normalizes to empty, which is the item that is about no
// particular subject. A non-empty input that leaves nothing behind - punctuation
// only - is ErrInvalid: the writer named a subject and there is nothing to key on,
// and silently storing it as unsubjected would hide the mistake at exactly the
// point the caller was relying on the key.
//
// Letters and digits are judged by Unicode category rather than by the ASCII
// ranges, so a subject written in a non-Latin script normalizes to itself instead
// of collapsing to nothing.
//
// Every write path normalizes through this, so a backend cannot invent its own
// answer to what the same subject is.
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

// NormalizeSupersedes returns the superseded ids in canonical form - sorted, with
// duplicates collapsed - or ErrInvalid if self is among them or any id is blank.
// Nil and empty both normalize to nil.
//
// An item may not supersede itself: the chain is what a reader walks backwards to
// find what a fact replaced, and a self-loop makes that walk non-terminating for
// no expressible meaning. Order carries nothing here, so fixing it buys a stable
// encoding at no cost, exactly as it does for anchors.
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

// MemoryItem is a durable fact the agent has learned, attributable to its
// sources for provenance and rollback.
type MemoryItem struct {
	ID      string
	Kind    string // e.g. "fact", "preference", "observation"
	Content string
	Scope   Scope
	// Subject is the slug this item is about, the key a host groups writes on when
	// it decides what a new fact does to the ones already stored: replace the
	// standing answer, or join a running series. Kind says what sort of thing the
	// item is; Subject says which topic it belongs to, and neither is derivable
	// from the other.
	//
	// It exists as its own field rather than as an Anchor of kind "subject"
	// because the two answer different questions and are matched differently.
	// Anchors are opaque refs into somebody else's namespace, an item carries any
	// number of them, and a dangling one is normal. A subject is this store's own
	// vocabulary, an item has exactly one, and it is the key a write path keys on -
	// which means it has to be normalized (see NormalizeSubject) so two spellings
	// of one topic do not fork the chain, and anchors are deliberately never
	// normalized beyond their shape.
	//
	// Empty is the item about no particular subject, and is the common case for an
	// observation nobody intends to revise. Nothing here decides what a subject
	// implies: a store records the key, and the host's write policy decides whether
	// the next write on that subject replaces or appends (see MemoryStore).
	Subject string
	// Supersedes are the ids of the items this one replaces: the correction's link
	// back to what it corrected. It is recorded at the write because that is the
	// only moment anybody knows - a later reader looking at two contradictory facts
	// on one subject cannot tell a revision from an honest disagreement.
	//
	// Superseding does not retire anything. The store still returns both items
	// until the loser is tombstoned with Delete, and keeping the two acts separate
	// is what lets a host retire the old fact while the audit trail still says what
	// replaced it, or keep both live deliberately. A backend that inferred a delete
	// from this field would make the record lossy in a way nobody asked for.
	//
	// Ids are opaque here in the way anchors are: nothing resolves them, so an id
	// naming an item that has since been purged is a dead link in the chain rather
	// than an error.
	Supersedes []string
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
	Sources []string
	// Anchors are the things outside this store that the fact is about, the cues a
	// reader can recall by (see Anchor and RecallQuery.Anchors). They are stored in
	// canonical form - sorted, deduplicated - by every write path; a caller may pass
	// them in any order.
	//
	// Anchors are not provenance. Sources records where a fact came from, which is
	// what a purge follows; Anchors records what it is about, which is what a read
	// matches on. A fact learned from a tool's output while working on something is
	// perfectly normal, and the two lists then hold different things.
	Anchors []Anchor
	// Tainted records that attacker-influenceable input was in the context that
	// produced this fact, whatever its Sources say. It is set at write and never
	// cleared: taint only ever spreads.
	//
	// The field exists because the channel a write arrives on is not the provenance
	// of its content. An agent that reads a poisoned tool output, draws a conclusion
	// from it and writes that conclusion as its own has laundered the untrusted
	// input into a semi-trusted record, and Sources - which credits the inputs the
	// writer names - records the laundered story faithfully. Nothing derivable from
	// the stored item can tell that apart from an honest agent note, so the writing
	// context has to say so at the time, and the store has to keep it.
	//
	// A taint is not a refusal. A tainted fact is written, recalled, and used like
	// any other; what it cannot do is enter the wake digest, which is the one path
	// that reaches every reader without anyone asking for it (see
	// guard.PushEligibility). Marking is the host's job: a store with no taint
	// source behind it records only what provenance already implies, so a host that
	// has not wired one up is trusting its own agent's channel labels.
	Tainted   bool
	CreatedAt time.Time
	// ExpiresAt is the first instant this item stops being recallable; the zero
	// value never expires. It is the half-open convention RecallQuery.Since and
	// Until already use, so an item expiring at T is recalled at T-1ns and not at T.
	//
	// Expiry belongs in the record rather than in host policy because the death date
	// is known at write time and nowhere else: whoever learns "this credential
	// rotates on Friday" or "this plan is void at the end of the month"
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
	// Subjects restricts the recall to items whose Subject is one of these; empty
	// matches every item. The values are matched exactly and are not normalized
	// here, so a caller filtering on a subject it did not get from a stored item
	// passes it through NormalizeSubject first, the same form the write path stored.
	//
	// This is the read a write policy takes before it decides what a new fact does
	// to the ones already on its subject, and the read a consolidation pass takes to
	// collect the series it is about to distil. Both want every item on one topic and
	// nothing else, which a lexical query cannot express: the subject is a key, and
	// searching for its text finds the items that merely mention it and misses the
	// ones that never spell it out.
	Subjects []string
	// Anchors restricts the recall to items carrying at least one of these anchors;
	// empty matches every item. An item matches on any one of them, not all, because
	// the caller reading a thing wants what is about that thing, and an item anchored
	// to it and to two others is still about it.
	//
	// This is the retrieval half of associative recall: a reader who has a ref in
	// hand can ask what is known about it without composing a lexical query, which is
	// exactly the case pull-only search cannot serve, because it requires already
	// suspecting the fact exists. An anchored recall with an empty Query is the
	// normal shape.
	//
	// Anchors are matched exactly, on both halves. Nothing here resolves a ref, so
	// an anchor naming a referent that no longer exists simply matches nothing.
	Anchors []Anchor
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

// Selects reports whether it satisfies the query's kind, subject and anchor
// filters and its time window, and has not expired as of now. These are the per-item selectors, separate from scope
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

// matchesAnchors reports whether it carries any of the anchors the query asked
// for, which an unanchored query trivially satisfies. Both sides are short - a
// handful of refs at most - so this is a nested scan rather than a set built per
// call, which would allocate on every item for a membership test over three
// elements.
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
//     stay live, and Recall will return both. A host that needs one truth per
//     subject groups the writes by MemoryItem.Subject, links the correction to
//     what it corrects with MemoryItem.Supersedes, and retires the loser with
//     Delete. The store records all three and decides none of them, because the
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

	// RecordPush counts one push of each of these items at a reader: the store
	// put them in front of somebody who did not ask for them. It is recorded
	// against the writing instance (see MemoryUsage). Every id is checked before
	// anything is recorded, so a set carrying one bad id records nothing.
	//
	// It is the caller that knows a push happened; no read path infers one.
	// Recall does not count as a push and never records anything, because a
	// store that instrumented its own reads would count the instrumentation.
	//
	// An unknown or tombstoned ID is ErrNotFound and nothing is recorded, so a
	// stale id in a digest surfaces instead of quietly accruing counts against
	// an item nobody can read. An empty list is a no-op.
	RecordPush(ctx context.Context, memoryIDs []string) error
	// RecordUse counts one use of an item, attributed by origin: the caller
	// declares whether the reader went and found it (UsageOrganic) or whether it
	// had already been pushed at them (UsagePrimed). An origin that is not one of
	// those two is ErrInvalid; there is no default, because the split is the
	// measurement (see UsageOrigin). An unknown or tombstoned ID is ErrNotFound.
	RecordUse(ctx context.Context, memoryID string, origin UsageOrigin) error
	// Usage returns the per-instance usage rows for these items, ordered by item
	// then instance (SortUsage). An empty list returns every row the store holds,
	// which is the read the fleet-wide metrics take.
	//
	// Items with no usage yet have no rows, rather than a zero row: never pushed
	// and never used is the absence of an observation, and materializing it would
	// make an untouched corpus indistinguishable from an ignored one. Rows survive
	// their item's tombstone, so a curator can still see what was pushed at
	// readers before it was retired.
	Usage(ctx context.Context, memoryIDs []string) ([]MemoryUsage, error)

	// Promote records a trusted reviewer's standing decision about whether an item
	// may be pushed at a reader who did not ask for it, and returns the post-image
	// row (see MemoryPromotion). A decision on an item that already has one replaces
	// it, so revoking is Promote with Promoted false rather than a second method:
	// there is one current answer per item, and the history of how it got there
	// lives on the event stream, not in a pile of rows.
	//
	// A decision missing an item or a reviewer is ErrInvalid, and an unknown or
	// tombstoned item is ErrNotFound: a promotion nobody can attribute, or one
	// pointing at nothing, is an audit trail with a hole in it.
	Promote(ctx context.Context, d PromotionDecision) (MemoryPromotion, error)
	// Promotions returns the decision rows for these items, ordered by item
	// (SortPromotions). An empty list returns every row the store holds.
	//
	// An item nobody has reviewed has no row rather than a false one, the same rule
	// usage follows: silence is not a decision. A reader building the push set
	// treats both as not-promoted (see PromotedSet), and only an audit needs to tell
	// them apart. Rows survive their item's tombstone.
	Promotions(ctx context.Context, memoryIDs []string) ([]MemoryPromotion, error)
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
