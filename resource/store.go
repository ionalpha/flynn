package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ResourceStream is the spine stream for every resource mutation. One ordered
// stream lets Replay fold the whole store back into existence.
const ResourceStream = "resources"

// Resource event types. Each event carries the canonical post-image, so replaying
// in order reproduces identical state. Durable backends project the same types.
const (
	EvPut     = "resource.put"
	EvDeleted = "resource.deleted"
	// EvMerged records a resource applied by Merge. Its payload is the winning
	// post-image with the remote envelope unchanged, so every replica folding the
	// stream converges. It projects like EvPut and is a separate type so the log
	// shows which records arrived by fleet sync.
	EvMerged = "resource.merged"
)

const payloadKey = "resource"

// Store is the event-sourced port every kind is read and written through, the one
// interface a backend implements. A new domain registers a Kind and never edits
// Store. Backends (in-memory, SQLite, a host) are held to one contract by
// resourcetest.RunSuite.
type Store interface {
	// Put creates or updates the resource at (Kind, Scope, Name): it admits Spec,
	// assigns identity, envelope, hash and timestamps, and records the event.
	// Optimistic concurrency is opt-in through SyncVersion.
	Put(ctx context.Context, r Resource) (Resource, error)
	// Get returns the live resource for (kind, scope, name), or ErrNotFound.
	Get(ctx context.Context, kind string, scope Scope, name string) (Resource, error)
	// GetByID returns the live resource by its stable id, or ErrNotFound.
	GetByID(ctx context.Context, id string) (Resource, error)
	// List returns the live resources of a kind in a scope matching the selector
	// (nil matches all), ordered by name.
	List(ctx context.Context, kind string, scope Scope, sel Selector) ([]Resource, error)
	// ListAll returns the live resources of a kind in every scope matching the
	// selector (nil matches all), ordered by scope then name. Facades that resolve
	// a scope-independent handle, such as a skill slug, read through it.
	ListAll(ctx context.Context, kind string, sel Selector) ([]Resource, error)
	// Delete requests deletion of the resource at (kind, scope, name), or returns
	// ErrNotFound. Without finalizers it tombstones now; with them it sets
	// DeletionTimestamp and Put completes the deletion when the last is removed.
	// Deleting a terminating resource is an idempotent no-op.
	Delete(ctx context.Context, kind string, scope Scope, name string) error
	// Merge applies a resource replicated from another instance without losing a
	// write. It trusts the remote envelope (ID, origin, HLC, versions, provenance)
	// and never restamps it, so replicas reach byte-identical state in any arrival
	// order. Resolve has the conflict rules; the result says whether the remote was
	// applied, ignored or already present.
	//
	// A record merges against the local record with the same ID. The same (Kind,
	// Scope, Name) created on two instances has two IDs and stays two records; the
	// apply path does not resolve name collisions.
	Merge(ctx context.Context, remote Resource) (MergeResult, error)
	// Snapshot checkpoints the projection onto the log, so a rebuild folds only
	// later events. It changes how fast a rebuild is and never what a read returns.
	Snapshot(ctx context.Context) error
	// Close releases backend resources.
	Close() error
}

// KeyLister is an optional Store capability: the keys of every live resource of a
// kind, ordered by scope then name. The reconcile resync uses it when offered, so
// a sweep does not copy every record. Both bundled backends implement it.
type KeyLister interface {
	ListKeys(ctx context.Context, kind string) ([]Key, error)
}

// AnyScopeGetter is an optional Store capability: the first live resource of a kind
// with a name in any scope, in ListAll's order. The control plane's cross-scope get
// uses it when offered, so the lookup seeks the name instead of scanning the kind;
// otherwise it falls back to ListAll. found is false with a nil error when no scope
// has the name.
type AnyScopeGetter interface {
	GetAnyScope(ctx context.Context, kind, name string) (Resource, bool, error)
}

// OwnerGone reports whether r's controller owner is gone or terminating, which
// makes r an orphan to reap so an owner's deletion cascades. A resource with no
// controller owner is a root and never orphaned. The owner resolves by stable id,
// so a rename never breaks the link.
func OwnerGone(ctx context.Context, store Store, r Resource) (bool, error) {
	owner, ok := r.Controller()
	if !ok {
		return false, nil
	}
	o, err := store.GetByID(ctx, owner.ID)
	if errors.Is(err, ErrNotFound) {
		return true, nil // the owner is gone: r is an orphan
	}
	if err != nil {
		return false, err
	}
	return o.DeletionTimestamp != nil, nil // owner terminating: cascade the reap
}

// encodePayload serializes r to the event payload {"resource": <record>} with one
// Marshal. The spine stores the bytes verbatim, so a write serializes its
// post-image once.
func encodePayload(r Resource) (json.RawMessage, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	// Appending to the fixed prefix lets append size the buffer, so no capacity is
	// computed from the marshalled length.
	p := append([]byte(`{"`+payloadKey+`":`), b...)
	return append(p, '}'), nil
}

// DecodeResource reconstructs a Resource from an event payload, so durable backends
// project the same records the in-memory core does.
func DecodeResource(payload map[string]any) (Resource, error) {
	b, err := json.Marshal(payload[payloadKey])
	if err != nil {
		return Resource{}, err
	}
	var r Resource
	if err := json.Unmarshal(b, &r); err != nil {
		return Resource{}, err
	}
	return r, nil
}

// kindSpecSchema is the JSON schema for a Kind's spec, so kinds the agent authors
// at runtime are admitted like any other resource.
var kindSpecSchema = json.RawMessage(`{
  "type": "object",
  "required": ["apiVersion", "name"],
  "properties": {
    "apiVersion": {"type": "string", "minLength": 1},
    "name": {"type": "string", "minLength": 1},
    "schema": {"type": "object"},
    "singular": {"type": "string"},
    "plural": {"type": "string"}
  }
}`)

// RegisterCoreKinds registers the built-in kinds, starting with Kind itself, so a
// Kind can be stored and validated as a Resource.
func RegisterCoreKinds(reg *Registry) error {
	return reg.Register(Kind{
		APIVersion: CoreGroupVersion,
		Name:       KindKind,
		Schema:     kindSpecSchema,
		Singular:   "kind",
		Plural:     "kinds",
	})
}

// KindResource renders a Kind as a Resource of kind "Kind", so kind definitions
// store and sync like everything else. Empty optional fields are omitted so the
// spec satisfies the Kind schema.
func KindResource(k Kind, scope Scope) (Resource, error) {
	specMap := map[string]any{
		"apiVersion": k.APIVersion,
		"name":       k.Name,
	}
	if len(k.Schema) > 0 {
		specMap["schema"] = k.Schema
	}
	if k.Singular != "" {
		specMap["singular"] = k.Singular
	}
	if k.Plural != "" {
		specMap["plural"] = k.Plural
	}
	spec, err := json.Marshal(specMap)
	if err != nil {
		return Resource{}, fmt.Errorf("resource: render kind: %w", err)
	}
	return Resource{
		APIVersion: CoreGroupVersion,
		Kind:       KindKind,
		Name:       k.Name,
		Scope:      scope,
		Spec:       spec,
	}, nil
}
