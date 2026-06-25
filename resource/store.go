package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ResourceStream is the spine stream every resource mutation is recorded on. One
// ordered stream means a Replay folds the whole substrate back into existence.
const ResourceStream = "resources"

// Resource event types: the vocabulary of the resource stream. Each event is the
// canonical post-image of the affected resource, so replaying in order reproduces
// identical state. Exported so durable backends project the same vocabulary.
const (
	EvPut     = "resource.put"
	EvDeleted = "resource.deleted"
	// EvMerged records a resource applied by cross-instance Merge: its payload is
	// the winning post-image verbatim (the remote envelope preserved, not
	// restamped), so every replica that folds the stream converges identically. It
	// projects exactly like EvPut, but is a distinct type so the log stays auditable
	// about which records arrived by local write versus fleet sync.
	EvMerged = "resource.merged"
)

// payloadKey is the event payload key under which the post-image resource lives.
const payloadKey = "resource"

// Store is the generic, event-sourced port every kind is read and written
// through: the single interface a backend implements to persist the entire
// substrate. Adding a domain is registering a Kind, never editing this interface
// or forking a backend. Backends (in-memory, SQLite, a host) are interchangeable,
// held to one contract by resourcetest.RunSuite.
type Store interface {
	// Put creates or updates the resource addressed by (Kind, Scope, Name). It
	// validates Spec against the kind's schema (admission), assigns identity,
	// envelope, content hash, and timestamps, and records the mutation on the
	// event log. Optimistic concurrency is opt-in via SyncVersion.
	Put(ctx context.Context, r Resource) (Resource, error)
	// Get returns the live resource for (kind, scope, name), or ErrNotFound.
	Get(ctx context.Context, kind string, scope Scope, name string) (Resource, error)
	// GetByID returns the live resource by its stable id, or ErrNotFound.
	GetByID(ctx context.Context, id string) (Resource, error)
	// List returns the live resources of a kind in a scope whose labels satisfy
	// the selector (nil selector matches all), ordered by name.
	List(ctx context.Context, kind string, scope Scope, sel Selector) ([]Resource, error)
	// ListAll returns the live resources of a kind across every scope whose labels
	// satisfy the selector (nil selector matches all), ordered by scope then name.
	// It is the cross-namespace query: typed facades that resolve by a
	// scope-independent handle (a skill slug, say) and selector-driven views over a
	// whole kind read through it, the way Kubernetes lists a kind across all
	// namespaces.
	ListAll(ctx context.Context, kind string, sel Selector) ([]Resource, error)
	// Delete requests deletion of the resource addressed by (kind, scope, name), or
	// returns ErrNotFound. With no finalizers it tombstones immediately; with
	// finalizers it marks the resource terminating (sets DeletionTimestamp, keeps it
	// live) and the deletion completes via Put when the last finalizer is removed.
	// Deleting an already-terminating resource is an idempotent no-op.
	Delete(ctx context.Context, kind string, scope Scope, name string) error
	// Merge applies a resource replicated from another instance, converging the two
	// without losing a write. Distinct from Put (the local-write command): Merge
	// trusts the remote envelope (ID, origin, HLC, versions, provenance) and never
	// restamps it, so all replicas reach byte-identical state regardless of the
	// order replicas arrive. See Resolve for the conflict rules; the result reports
	// whether the remote was applied, ignored as stale, or already present.
	//
	// Identity is the global ID: a record is merged against the local record with
	// the same ID. The same (Kind, Scope, Name) created independently on two
	// instances has two different IDs and so stays two distinct records; resolving
	// such a name collision is a higher-level concern, not part of the apply path.
	Merge(ctx context.Context, remote Resource) (MergeResult, error)
	// Close releases backend resources.
	Close() error
}

// OwnerGone reports whether r's controller owner no longer exists or is itself
// terminating, which makes r an orphan a garbage collector should reap so an
// owner's deletion cascades to the subtree it created. A resource with no
// controller owner is a root and is never orphaned. The owner is resolved by its
// stable id, so a rename never breaks the link. It is the reusable predicate a
// kind's reconciler calls to garbage-collect owned resources.
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

// encodeResource serialises a resource to a JSON-compatible value for an event
// payload (the spine is a JSON boundary).
func encodeResource(r Resource) (any, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DecodeResource reconstructs a Resource from an event payload. Durable backends
// use it to project the same records the in-memory core does.
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

// kindSpecSchema is the JSON schema for a Kind's spec: it makes "Kind" a
// schema-validated kind like any other, so kinds the agent authors at runtime are
// themselves admitted. This is the meta-circular base case.
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

// RegisterCoreKinds registers the substrate's built-in kinds, starting with Kind
// itself (kind == "Kind"), so a Kind can be stored and validated as a Resource.
// This bootstraps meta-circularity: the type system is data on the same store.
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
// are stored and synced through the same substrate as everything else. Optional
// fields are omitted when empty so the spec satisfies the Kind schema.
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
