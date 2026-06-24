// Package resource is the agent's unified substrate: everything in the system is
// a Resource, a typed, versioned, schema-validated record on one event-sourced
// store. One model, one envelope, one scope/label system, one provenance path.
//
// The shape is deliberately Kubernetes-aligned (APIVersion + Kind + Spec/Status +
// Labels/Selectors), because that declarative model is proven and instantly
// familiar, but these are Flynn resources, not Kubernetes objects: they carry our
// own API groups (e.g. core.ionagent.io/v1alpha1), live in this package, and are
// never the upstream k8s types (which belong to the Kubernetes integration). The
// alignment keeps a future where resources export as CRDs / GitOps-manage open,
// without depending on a cluster.
//
// Every kind is data, including Kind itself (a Kind is a Resource of kind "Kind"),
// so the agent can author and validate new kinds at runtime. Typed domains layer
// on top as thin facades; specialized indexes (full-text, vector, ordering) are
// projections over the resource event log, not parallel stores.
package resource

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/spine"
)

// ErrNotFound is returned when a requested resource does not exist.
var ErrNotFound = errors.New("resource: not found")

// ErrConflict is returned when optimistic concurrency fails: the caller passed a
// non-zero SyncVersion that no longer matches the stored record. Re-read and retry.
var ErrConflict = errors.New("resource: version conflict")

// ErrInvalid is returned when a resource fails admission (missing required field,
// unregistered kind, or a Spec that does not satisfy its kind's JSON schema).
var ErrInvalid = errors.New("resource: invalid")

// Scope locates a resource on the instance/project/workspace axis (the namespace),
// so resources can be partitioned and resolved most-specific-first and shared
// selectively across a fleet. The zero Scope is the global (instance) scope.
// Scope is comparable.
type Scope struct {
	Instance  string
	Project   string
	Workspace string
}

// Envelope is the universal metadata carried by every resource: sync/concurrency,
// content provenance, and bitemporal time. Designing the whole envelope in from
// the first write keeps replay, optimistic concurrency, fleet merge, verifiable
// history, and as-of queries reachable without ever migrating it in later.
type Envelope struct {
	// --- sync / concurrency ---

	// SyncVersion is bumped on every write (1 on create). It powers local
	// optimistic concurrency: pass the version you read and the write fails with
	// ErrConflict if it has moved; a zero SyncVersion writes unconditionally.
	SyncVersion int64
	// OriginInstanceID is the instance that first created the resource; preserved
	// across updates so fleet/P2P merge can attribute provenance.
	OriginInstanceID string
	// UpdatedHLC is the hybrid-logical-clock time of the last write. It orders
	// writes across instances for last-writer-wins merge (the LWW key is
	// (UpdatedHLC, LastWriterID)), where SyncVersion (local-only) cannot.
	UpdatedHLC hlc.Time
	// LastWriterID is the instance that performed the last write (distinct from
	// OriginInstanceID, the creator).
	LastWriterID string
	// WriterActor records who authored the last write: a human, the agent, or the
	// runtime (system). It is the provenance signal cross-instance Merge uses for
	// precedence, so a person's correction outranks a later automated write and is
	// never silently overwritten by the fleet. It is metadata, not content, so it is
	// excluded from the content hash (identical content authored by different actors
	// shares a hash). The zero value is treated as the agent.
	WriterActor spine.ActorType
	// Deleted marks a tombstone: a soft delete that still carries its envelope so
	// it propagates in sync and prevents a stale replica from resurrecting it.
	Deleted bool

	// --- content provenance ---

	// Version is the content revision (incremented on every content change),
	// distinct from SyncVersion (the sync/concurrency token).
	Version int64
	// ContentHash is a stable hash of the resource's canonical content (see
	// Hash). Equal content yields an equal hash across machines, which makes
	// history a Merkle DAG: dedup, "which version produced this", tamper-evidence,
	// and efficient diff-based sync.
	ContentHash string

	// --- bitemporal time ---

	// ValidFrom and ValidTo are the resource's valid-time: when it became and
	// ceased to be true in the world, distinct from event-time (when we recorded
	// it, carried by UpdatedHLC and the event log). Nil ValidFrom means "valid
	// since creation"; nil ValidTo means "still valid". Reserved from day one so a
	// second time axis never requires a schema migration; the query surface that
	// uses them can follow.
	ValidFrom *time.Time
	ValidTo   *time.Time

	// --- timestamps ---

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Resource is the universal record for every data-defined kind in the system.
//
// Identity: ID is a stable, globally unique id assigned on creation. Name is the
// logical name, unique within (Kind, Scope), the handle callers use to upsert and
// fetch (a Skill's slug, an Agent's name). APIVersion is the kind's group/version,
// e.g. "skill.ionagent.io/v1".
//
// Kinds without a natural name (a memory fact, a turn, a run) set GenerateName
// instead of Name: the store assigns Name = GenerateName + ID on create, so every
// such record gets a unique, sortable name from the one deterministic ID source
// rather than each facade minting its own. See GenerateName.
type Resource struct {
	APIVersion string
	Kind       string
	ID         string
	Name       string
	// GenerateName requests a server-assigned Name for a kind that has no natural
	// one. When Name is empty and GenerateName is set, a create assigns
	// Name = GenerateName + ID (e.g. "mem-01J..."); it is a create-only directive,
	// consumed and cleared on write, never stored. Setting Name takes precedence
	// and GenerateName is ignored. Mirrors Kubernetes metadata.generateName, but
	// uses our globally unique, sortable ID as the suffix instead of a random one,
	// so there is never a name collision to retry.
	GenerateName string
	Scope        Scope
	Labels       map[string]string
	Annotations  map[string]string
	// Spec is the desired state (validated against the kind's JSON schema). It is
	// raw JSON so it embeds readably in events and hashes canonically; nil when
	// unset.
	Spec json.RawMessage
	// Status is the observed state (set by controllers/reconcilers, not admitted
	// against the spec schema). Raw JSON; nil when unset.
	Status json.RawMessage
	Envelope
}

// Key uniquely identifies a resource by its logical coordinates (Kind, Scope,
// Name), the address an upsert or fetch targets. ID addresses the same record by
// its stable id. Key is comparable.
type Key struct {
	Kind  string
	Scope Scope
	Name  string
}

// Key returns the resource's logical key.
func (r Resource) Key() Key { return Key{Kind: r.Kind, Scope: r.Scope, Name: r.Name} }
