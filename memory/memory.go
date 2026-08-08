// Package memory provides the agent's memory as a typed facade over the unified
// resource foundation. A memory item is stored as a resource.Resource of kind
// "Memory": it has no natural name, so each write is a create with a server-assigned
// name (GenerateName), and the kind/content/source live in a schema-validated Spec.
// The facade implements state.MemoryStore, so call sites keep the same ergonomic API
// while the data lives on one event-sourced store with one envelope, one
// schema/admission path, and one provenance/sync model shared with every other kind.
//
// Recall is a read model over that store: the facade ranks live items by a
// case-insensitive content scan, most-specific scope first then most-recent first,
// and a backend can maintain a full-text or vector projection of the same resource
// events without changing this contract.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/envelope"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

// GroupVersion is the memory kind's API group/version. The `.ionagent.io` suffix
// marks it unmistakably as ours, never a Kubernetes built-in.
const GroupVersion = "memory.ionagent.io/v1"

// Kind is the resource kind name memory items are stored under.
const Kind = "Memory"

// UsageKind is the resource kind name per-instance memory usage is stored under.
const UsageKind = "MemoryUsage"

// PromotionKind is the resource kind name a memory item's push-eligibility
// decision is stored under.
const PromotionKind = "MemoryPromotion"

// namePrefix is the GenerateName prefix for a memory item's server-assigned name
// (Name = "mem-" + ID), since memory items carry no natural name.
const namePrefix = "mem-"

// usageNamePrefix starts the natural name of a usage record,
// "memuse-<memoryID>-<instanceID>". Usage has a natural name where the item it is
// about does not: the pair it is keyed by is exactly what identifies it, so the
// record is addressable without a lookup and an update is an ordinary Put.
const usageNamePrefix = "memuse-"

// promotionNamePrefix starts the natural name of a decision record,
// "mempromo-<memoryID>". One item has one decision in force, so the item's id is
// the whole key and a revision is an ordinary Put.
const promotionNamePrefix = "mempromo-"

// specSchema is the JSON Schema a memory item's Spec must satisfy (admission). It
// constrains structure without over-requiring, so an item carrying only content is
// still valid.
var specSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "kind": {"type": "string"},
    "content": {"type": "string"},
    "subject": {"type": "string"},
    "supersedes": {"type": "array", "items": {"type": "string"}},
    "sources": {"type": "array", "items": {"type": "string"}},
    "source": {"type": "string"},
    "anchors": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "kind": {"type": "string", "minLength": 1},
          "id": {"type": "string", "minLength": 1}
        },
        "required": ["kind", "id"],
        "additionalProperties": false
      }
    },
    "expires_at": {"type": "string"},
    "tainted": {"type": "boolean"}
  },
  "additionalProperties": false
}`)

// KindDef is the Memory kind definition, the value registered with a resource
// registry so the store admits memory items.
var KindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       Kind,
	Schema:     specSchema,
	Singular:   "memory",
	Plural:     "memories",
}

// UsageKindDef is the MemoryUsage kind definition: one resource per memory item
// per instance, holding how often that instance pushed the item at a reader and
// how often it was used (see state.MemoryUsage).
//
// Usage is its own kind rather than a field on the item, because a memory item is
// append-only and its spec is what the writer asserted. Folding a counter into it
// would rewrite the record, and its content hash, every time somebody read it.
var UsageKindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       UsageKind,
	Schema:     usageSpecSchema,
	Singular:   "memoryusage",
	Plural:     "memoryusages",
}

// PromotionKindDef is the MemoryPromotion kind definition: one resource per memory
// item, holding the reviewer decision in force about whether it may be auto-pushed
// (see state.MemoryPromotion).
//
// It is a separate kind for the same reason usage is. The item is append-only and
// its spec is what its writer asserted; a promotion is a later, revisable judgment
// by somebody else, and folding it in would move the item's content hash every time
// a reviewer changed their mind.
var PromotionKindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       PromotionKind,
	Schema:     promotionSpecSchema,
	Singular:   "memorypromotion",
	Plural:     "memorypromotions",
}

// RegisterKind registers the Memory, MemoryUsage and MemoryPromotion kinds with reg
// so a resource store admits memory items, their usage and their push decisions. It
// is idempotent: registering again replaces the definitions.
func RegisterKind(reg *resource.Registry) error {
	if err := reg.Register(KindDef); err != nil {
		return err
	}
	if err := reg.Register(UsageKindDef); err != nil {
		return err
	}
	return reg.Register(PromotionKindDef)
}

// spec is the typed shape of a memory resource's Spec (the JSON validated by
// specSchema). Empty fields are omitted so a minimal item hashes and validates as a
// small object.
// Spec fields are omitempty so a minimal item hashes and validates as a small
// object. Source is the pre-list provenance field: never written any more, still
// decoded, because resources already stored under the old shape must keep their
// provenance when they are read back.
// ExpiresAt is a pointer so the common never-expires item omits it entirely: a
// time.Time value would encode the zero time into every spec and change what a
// minimal item hashes to.
// Tainted is omitempty for the same reason: the common untainted item encodes and
// hashes exactly as it did before the field existed, so adding it does not rewrite
// what every stored item hashes to.
type spec struct {
	Kind    string `json:"kind,omitempty"`
	Content string `json:"content,omitempty"`
	// Subject and Supersedes are omitempty for the same reason Tainted is: the
	// common item carries neither, and encoding an empty slug and a null list into
	// every spec would move what every already-stored item hashes to.
	Subject    string     `json:"subject,omitempty"`
	Supersedes []string   `json:"supersedes,omitempty"`
	Sources    []string   `json:"sources,omitempty"`
	Source     string     `json:"source,omitempty"`
	Anchors    []anchor   `json:"anchors,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Tainted    bool       `json:"tainted,omitempty"`
}

// anchor is the stored shape of a state.Anchor. It exists so the spec's JSON keys
// are lowercase like every other field here, rather than inheriting the exported Go
// names, and so the schema above has something stable to validate against.
type anchor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

func encodeAnchors(in []state.Anchor) []anchor {
	if len(in) == 0 {
		return nil
	}
	out := make([]anchor, len(in))
	for i, a := range in {
		out[i] = anchor{Kind: a.Kind, ID: a.ID}
	}
	return out
}

func decodeAnchors(in []anchor) []state.Anchor {
	if len(in) == 0 {
		return nil
	}
	out := make([]state.Anchor, len(in))
	for i, a := range in {
		out[i] = state.Anchor{Kind: a.Kind, ID: a.ID}
	}
	return out
}

// Store is the typed memory facade over a resource.Store. It is the MemoryStore the
// agent uses; underneath, every write and recall is a resource operation on one
// event-sourced foundation.
type Store struct {
	rs         resource.Store
	clk        clock.Clock
	instanceID string
}

var _ state.MemoryStore = (*Store)(nil)

// Option configures a Store.
type Option func(*Store)

// WithClock sets the clock Recall judges MemoryItem.ExpiresAt against. The default
// is the system clock; a test that writes an item expiring in an hour and then
// wants to see it gone passes a clock.Manual and moves it, rather than sleeping.
// A nil clock is ignored.
func WithClock(c clock.Clock) Option {
	return func(s *Store) {
		if c != nil {
			s.clk = c
		}
	}
}

// WithInstanceID sets the instance usage is attributed to (default "local", the
// same default the in-memory state provider uses). It must be the identity the
// underlying resource store stamps its writes with: usage is counted per
// instance, so two names for one instance would split its counters in half and
// report the fleet as more diverse than it is. A write that lands under a
// different origin is rejected rather than recorded, so a mismatch shows up on
// the first push instead of in the metrics months later.
func WithInstanceID(id string) Option {
	return func(s *Store) {
		if id != "" {
			s.instanceID = id
		}
	}
}

// NewStore returns a memory facade over rs. The caller must have registered the
// Memory and MemoryUsage kinds with the registry rs admits against (see
// RegisterKind).
func NewStore(rs resource.Store, opts ...Option) *Store {
	s := &Store{rs: rs, clk: clock.System{}, instanceID: defaultInstanceID}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Write persists a memory item as a new Memory resource, assigning the id and name
// from the foundation's single ID source. Memory is append-only: each Write is a
// distinct record, never an update of a prior one.
func (s *Store) Write(ctx context.Context, m state.MemoryItem) (state.MemoryItem, error) {
	r, err := toResource(m)
	if err != nil {
		return state.MemoryItem{}, err
	}
	out, err := s.rs.Put(ctx, r)
	if err != nil {
		return state.MemoryItem{}, translateErr(err)
	}
	return toItem(out)
}

// Recall returns live memory items whose content contains the query (case
// insensitive), most-specific scope first then most-recent first, capped at
// q.Limit (<= 0 means no cap). A zero Scope spans every scope; a set Scope
// narrows to it, and widens back out over that scope's ancestors when
// q.IncludeAncestors is set. An empty query matches every live item.
func (s *Store) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	rs, err := s.list(ctx, q)
	if err != nil {
		return nil, err
	}
	all, err := toItems(rs)
	if err != nil {
		return nil, err
	}
	query := strings.ToLower(strings.TrimSpace(q.Query))
	// One clock reading for the whole recall, so every item is judged expired
	// against the same instant.
	now := s.clk.Now()
	out := make([]state.MemoryItem, 0, len(all))
	for _, it := range all {
		if !q.Selects(it, now) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(it.Content), query) {
			continue
		}
		// A substring scan cannot grade a match, so every hit scores 1 rather than
		// 0 - see state.MemoryItem.Score. A projection that can rank (a full-text or
		// vector index over the same resource events) reports its own score here
		// without any other part of this contract changing.
		it.Score = 1
		if it.Score < q.MinScore {
			continue
		}
		out = append(out, it)
	}
	state.SortRecall(q, out)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// list reads the resources a recall query covers: every scope for an unfiltered
// query, else one List per scope in the resolution chain. The chain is at most
// four scopes deep, so the widened read costs a bounded handful of scoped Lists
// rather than a ListAll that pulls every scope's memory back to filter it here.
func (s *Store) list(ctx context.Context, q state.RecallQuery) ([]resource.Resource, error) {
	chain := q.ScopeChain()
	if chain == nil {
		return s.rs.ListAll(ctx, Kind, nil)
	}
	if len(chain) == 1 {
		return s.rs.List(ctx, Kind, resource.Scope(chain[0]), nil)
	}
	var out []resource.Resource
	for _, sc := range chain {
		rs, err := s.rs.List(ctx, Kind, resource.Scope(sc), nil)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

// Delete tombstones a memory item by id, or returns state.ErrNotFound.
func (s *Store) Delete(ctx context.Context, id string) error {
	r, err := s.rs.GetByID(ctx, id)
	if err != nil {
		return translateErr(err)
	}
	return translateErr(s.rs.Delete(ctx, Kind, r.Scope, r.Name))
}

// toResource maps a memory item to its Memory resource. The item has no natural
// name, so a create carries GenerateName and the store assigns Name = mem-<id>;
// the sync version carries through so the store enforces optimistic concurrency.
func toResource(m state.MemoryItem) (resource.Resource, error) {
	// Anchors are canonicalized here rather than trusted from the caller: this
	// facade writes through resource.Store and so never passes state.Stamper, which
	// is where the other backends normalize. Both paths have to agree, or the same
	// write would hash differently depending on which store it landed on.
	anchors, err := state.NormalizeAnchors(m.Anchors)
	if err != nil {
		return resource.Resource{}, err
	}
	subject, err := state.NormalizeSubject(m.Subject)
	if err != nil {
		return resource.Resource{}, err
	}
	// The self-loop half of the check needs an id, and a create has none until the
	// store assigns one below; an id the caller supplied is checked. A generated id
	// is fresh, so it cannot be in a list the caller wrote before it existed.
	supersedes, err := state.NormalizeSupersedes(m.Supersedes, m.ID)
	if err != nil {
		return resource.Resource{}, err
	}
	sp := spec{
		Kind: m.Kind, Content: m.Content, Subject: subject, Supersedes: supersedes,
		Sources: m.Sources, Anchors: encodeAnchors(anchors), Tainted: m.Tainted,
	}
	if !m.ExpiresAt.IsZero() {
		exp := m.ExpiresAt
		sp.ExpiresAt = &exp
	}
	body, err := json.Marshal(sp)
	if err != nil {
		return resource.Resource{}, fmt.Errorf("memory: encode spec: %w", err)
	}
	return resource.Resource{
		APIVersion:   GroupVersion,
		Kind:         Kind,
		ID:           m.ID,
		GenerateName: namePrefix,
		Scope:        resource.Scope(m.Scope),
		Spec:         body,
		Envelope: resource.Envelope{
			Envelope: envelope.Envelope{
				SyncVersion:      m.SyncVersion,
				OriginInstanceID: m.OriginInstanceID,
			},
		},
	}, nil
}

// toItem maps a Memory resource back to the typed memory item. The shared envelope
// fields carry across so provenance and sync behave like every other kind.
func toItem(r resource.Resource) (state.MemoryItem, error) {
	sp, err := resource.DecodeSpec[spec](r)
	if err != nil {
		return state.MemoryItem{}, fmt.Errorf("memory: decode spec: %w", err)
	}
	sources := sp.Sources
	if len(sources) == 0 && sp.Source != "" {
		// Written before provenance became a list; keep the one source it recorded
		// rather than reading the item back with none.
		sources = []string{sp.Source}
	}
	var expires time.Time
	if sp.ExpiresAt != nil {
		expires = *sp.ExpiresAt
	}
	return state.MemoryItem{
		ID:         r.ID,
		Kind:       sp.Kind,
		Content:    sp.Content,
		Subject:    sp.Subject,
		Supersedes: sp.Supersedes,
		Sources:    sources,
		Anchors:    decodeAnchors(sp.Anchors),
		Tainted:    sp.Tainted,
		Scope:      state.Scope(r.Scope),
		CreatedAt:  r.CreatedAt,
		ExpiresAt:  expires,
		Envelope: state.Envelope{
			SyncVersion:      r.SyncVersion,
			OriginInstanceID: r.OriginInstanceID,
			UpdatedHLC:       r.UpdatedHLC,
			LastWriterID:     r.LastWriterID,
			Deleted:          r.Deleted,
		},
	}, nil
}

func toItems(rs []resource.Resource) ([]state.MemoryItem, error) {
	out := make([]state.MemoryItem, 0, len(rs))
	for _, r := range rs {
		it, err := toItem(r)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// translateErr maps the resource foundation's errors onto the state boundary's, so a
// MemoryStore caller sees state.ErrConflict / state.ErrNotFound regardless of the
// backing store.
func translateErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, resource.ErrConflict):
		return state.ErrConflict
	case errors.Is(err, resource.ErrNotFound):
		return state.ErrNotFound
	default:
		return err
	}
}
