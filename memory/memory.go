// Package memory implements state.MemoryStore over resource.Store. An item is a
// resource of kind "Memory" with a server-assigned name, because it has no natural
// one, and a schema-validated Spec. It shares the envelope, admission and sync
// model of every other kind.
//
// Recall is a case-insensitive content scan. A backend may project the same events
// into a full-text or vector index without changing the contract.
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
	"github.com/ionalpha/flynn/recallplan"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

// GroupVersion is the memory API group and version. The `.ionagent.io` suffix keeps
// it apart from Kubernetes built-ins.
const GroupVersion = "memory.ionagent.io/v1"

// Kind is the resource kind of a memory item.
const Kind = "Memory"

// UsageKind is the resource kind of per-instance usage.
const UsageKind = "MemoryUsage"

// PromotionKind is the resource kind of a push-eligibility decision.
const PromotionKind = "MemoryPromotion"

// namePrefix makes an item's name "mem-" + ID.
const namePrefix = "mem-"

// usageNamePrefix starts "memuse-<memoryID>-<instanceID>". The pair identifies the
// record, so it is addressable without a lookup and an update is a Put.
const usageNamePrefix = "memuse-"

// promotionNamePrefix starts "mempromo-<memoryID>". One item has one decision in
// force, so its id is the whole key.
const promotionNamePrefix = "mempromo-"

// specSchema requires no field, so an item with only content is valid.
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

// KindDef is the Memory kind definition.
var KindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       Kind,
	Schema:     specSchema,
	Singular:   "memory",
	Plural:     "memories",
}

// UsageKindDef is the MemoryUsage kind definition: one record per item per
// instance (see state.MemoryUsage). It is a separate kind because items are
// append-only, and a counter inside one would change its hash on every read.
var UsageKindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       UsageKind,
	Schema:     usageSpecSchema,
	Singular:   "memoryusage",
	Plural:     "memoryusages",
}

// PromotionKindDef is the MemoryPromotion kind definition: one reviewer decision per
// item on whether it may be auto-pushed (see state.MemoryPromotion). It is separate
// so a revised decision does not change the item's hash.
var PromotionKindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       PromotionKind,
	Schema:     promotionSpecSchema,
	Singular:   "memorypromotion",
	Plural:     "memorypromotions",
}

// RegisterKind registers the Memory, MemoryUsage and MemoryPromotion kinds with reg.
// Registering again replaces the definitions.
func RegisterKind(reg *resource.Registry) error {
	if err := reg.Register(KindDef); err != nil {
		return err
	}
	if err := reg.Register(UsageKindDef); err != nil {
		return err
	}
	return reg.Register(PromotionKindDef)
}

// spec is a memory item's Spec. Every field is omitempty, and ExpiresAt is a
// pointer, so adding a field never changes the hash of a stored item that lacks it.
// Source is read and never written, so items stored before Sources keep their
// provenance.
type spec struct {
	Kind       string     `json:"kind,omitempty"`
	Content    string     `json:"content,omitempty"`
	Subject    string     `json:"subject,omitempty"`
	Supersedes []string   `json:"supersedes,omitempty"`
	Sources    []string   `json:"sources,omitempty"`
	Source     string     `json:"source,omitempty"`
	Anchors    []anchor   `json:"anchors,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Tainted    bool       `json:"tainted,omitempty"`
}

// anchor stores a state.Anchor with the lowercase JSON keys specSchema validates.
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

// Store is the state.MemoryStore over a resource.Store.
type Store struct {
	rs         resource.Store
	clk        clock.Clock
	instanceID string
}

var _ state.MemoryStore = (*Store)(nil)

// Option configures a Store.
type Option func(*Store)

// WithClock sets the clock for expiry and usage times (default the system clock),
// so a test can advance a clock.Manual. A nil clock is ignored.
func WithClock(c clock.Clock) Option {
	return func(s *Store) {
		if c != nil {
			s.clk = c
		}
	}
}

// WithInstanceID sets the instance usage is attributed to (default "local"). It must
// match the identity the resource store stamps, or one instance's counters split
// across two names. A mismatch fails the first usage write.
func WithInstanceID(id string) Option {
	return func(s *Store) {
		if id != "" {
			s.instanceID = id
		}
	}
}

// NewStore returns a Store over rs, whose registry must hold the kinds from
// [RegisterKind].
func NewStore(rs resource.Store, opts ...Option) *Store {
	s := &Store{rs: rs, clk: clock.System{}, instanceID: defaultInstanceID}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Write stores m as a new record with a store-assigned id. Memory is append-only.
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

// Recall returns live items whose content holds the query's words, in any case and
// order, sorted most-specific scope then newest, capped at q.Limit (<= 0 is no cap).
// When no item holds every word, it widens to items holding some, per
// [recallplan.Plan]. A zero Scope spans all scopes; q.IncludeAncestors adds the
// scope's ancestors. An empty query matches every live item.
func (s *Store) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	rs, err := s.list(ctx, q)
	if err != nil {
		return nil, err
	}
	all, err := toItems(rs)
	if err != nil {
		return nil, err
	}
	query := strings.TrimSpace(q.Query)
	// One reading, so every item is judged against the same instant.
	now := s.clk.Now()
	selected := make([]state.MemoryItem, 0, len(all))
	for _, it := range all {
		if q.Selects(it, now) {
			selected = append(selected, it)
		}
	}
	var out []state.MemoryItem
	if query != "" {
		out = matchPlanned(selected, query, q.MinScore)
	} else {
		out = scoreAll(selected, q.MinScore)
	}
	state.SortRecall(q, out)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// scoreAll scores every item 1, since an empty query grades nothing.
func scoreAll(items []state.MemoryItem, floor float64) []state.MemoryItem {
	out := make([]state.MemoryItem, 0, len(items))
	for _, it := range items {
		it.Score = 1
		if it.Score >= floor {
			out = append(out, it)
		}
	}
	return out
}

// matchPlanned scores each hit by its share of the query words, so a widened partial
// match scores below 1, which state.MemoryItem.Score reserves for an ungraded match.
func matchPlanned(items []state.MemoryItem, query string, floor float64) []state.MemoryItem {
	hits := recallplan.Select(query, items, func(it state.MemoryItem) string { return it.Content }, floor)
	out := make([]state.MemoryItem, 0, len(hits))
	for _, h := range hits {
		h.Item.Score = h.Share
		out = append(out, h.Item)
	}
	return out
}

// list reads every scope for an unscoped query, else one List per scope in the
// chain. The chain is at most four deep, which is cheaper than ListAll and a filter.
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

// toResource carries the sync version through so the store enforces optimistic
// concurrency.
func toResource(m state.MemoryItem) (resource.Resource, error) {
	// Normalized here because this path skips state.Stamper, where other backends
	// normalize; otherwise one write would hash differently per backend.
	anchors, err := state.NormalizeAnchors(m.Anchors)
	if err != nil {
		return resource.Resource{}, err
	}
	subject, err := state.NormalizeSubject(m.Subject)
	if err != nil {
		return resource.Resource{}, err
	}
	// The self-loop check needs an id. A create has none yet, and a generated id
	// cannot appear in a list written before it existed.
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

// toItem maps a Memory resource to an item, envelope included.
func toItem(r resource.Resource) (state.MemoryItem, error) {
	sp, err := resource.DecodeSpec[spec](r)
	if err != nil {
		return state.MemoryItem{}, fmt.Errorf("memory: decode spec: %w", err)
	}
	sources := sp.Sources
	if len(sources) == 0 && sp.Source != "" {
		// An item stored with the single Source field.
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

// translateErr maps resource errors to state.ErrConflict and state.ErrNotFound.
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
