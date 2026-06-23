// Package memory provides the agent's memory as a typed facade over the unified
// resource substrate. A memory item is stored as a resource.Resource of kind
// "Memory": it has no natural name, so each write is a create with a server-assigned
// name (GenerateName), and the kind/content/source live in a schema-validated Spec.
// The facade implements state.MemoryStore, so call sites keep the same ergonomic API
// while the data lives on one event-sourced store with one envelope, one
// schema/admission path, and one provenance/sync model shared with every other kind.
//
// Recall is a read model over that store: the facade ranks live items by a
// case-insensitive content scan, most-recent first, and a backend can maintain a
// full-text or vector projection of the same resource events without changing this
// contract.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

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
// event-sourced substrate.
type Store struct {
	rs resource.Store
}

var _ state.MemoryStore = (*Store)(nil)

// NewStore returns a memory facade over rs. The caller must have registered the
// Memory kind with the registry rs admits against (see RegisterKind).
func NewStore(rs resource.Store) *Store { return &Store{rs: rs} }

// Write persists a memory item as a new Memory resource, assigning the id and name
// from the substrate's single ID source. Memory is append-only: each Write is a
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
// insensitive), most-recent first, capped at q.Limit (<= 0 means no cap). A zero
// Scope spans every scope; a set Scope narrows to it. An empty query matches every
// live item.
func (s *Store) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	var (
		rs  []resource.Resource
		err error
	)
	if q.Scope == (state.Scope{}) {
		rs, err = s.rs.ListAll(ctx, Kind, nil)
	} else {
		rs, err = s.rs.List(ctx, Kind, resource.Scope(q.Scope), nil)
	}
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
		if query == "" || strings.Contains(strings.ToLower(it.Content), query) {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt) // most-recent first
		}
		return out[i].ID < out[j].ID // total order: deterministic regardless of store iteration
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
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
// name, so a create carries GenerateName and the substrate assigns Name = mem-<id>;
// the sync version carries through so the substrate enforces optimistic concurrency.
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
			SyncVersion:      m.SyncVersion,
			OriginInstanceID: m.OriginInstanceID,
		},
	}, nil
}

// toItem maps a Memory resource back to the typed memory item. The shared envelope
// fields carry across so provenance and sync behave like every other kind.
func toItem(r resource.Resource) (state.MemoryItem, error) {
	var sp spec
	if len(r.Spec) > 0 {
		if err := json.Unmarshal(r.Spec, &sp); err != nil {
			return state.MemoryItem{}, fmt.Errorf("memory: decode spec: %w", err)
		}
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

// translateErr maps the resource substrate's errors onto the state seam's, so a
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
