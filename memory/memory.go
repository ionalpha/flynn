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

	"github.com/ionalpha/flynn/envelope"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

// GroupVersion is the memory kind's API group/version. The `.ionagent.io` suffix
// marks it unmistakably as ours, never a Kubernetes built-in.
const GroupVersion = "memory.ionagent.io/v1"

// Kind is the resource kind name memory items are stored under.
const Kind = "Memory"

// namePrefix is the GenerateName prefix for a memory item's server-assigned name
// (Name = "mem-" + ID), since memory items carry no natural name.
const namePrefix = "mem-"

// specSchema is the JSON Schema a memory item's Spec must satisfy (admission). It
// constrains structure without over-requiring, so an item carrying only content is
// still valid.
var specSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "kind": {"type": "string"},
    "content": {"type": "string"},
    "source": {"type": "string"}
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

// RegisterKind registers the Memory kind with reg so a resource store admits memory
// items. It is idempotent: registering again replaces the definition.
func RegisterKind(reg *resource.Registry) error { return reg.Register(KindDef) }

// spec is the typed shape of a memory resource's Spec (the JSON validated by
// specSchema). Empty fields are omitted so a minimal item hashes and validates as a
// small object.
type spec struct {
	Kind    string `json:"kind,omitempty"`
	Content string `json:"content,omitempty"`
	Source  string `json:"source,omitempty"`
}

// Store is the typed memory facade over a resource.Store. It is the MemoryStore the
// agent uses; underneath, every write and recall is a resource operation on one
// event-sourced foundation.
type Store struct {
	rs resource.Store
}

var _ state.MemoryStore = (*Store)(nil)

// NewStore returns a memory facade over rs. The caller must have registered the
// Memory kind with the registry rs admits against (see RegisterKind).
func NewStore(rs resource.Store) *Store { return &Store{rs: rs} }

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
	out := make([]state.MemoryItem, 0, len(all))
	for _, it := range all {
		if !q.Selects(it) {
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
	body, err := json.Marshal(spec{Kind: m.Kind, Content: m.Content, Source: m.Source})
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
	return state.MemoryItem{
		ID:        r.ID,
		Kind:      sp.Kind,
		Content:   sp.Content,
		Source:    sp.Source,
		Scope:     state.Scope(r.Scope),
		CreatedAt: r.CreatedAt,
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
