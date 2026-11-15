// Package resource is the agent's foundation: every record is a Resource, typed,
// versioned and schema-validated, on one event-sourced store with one envelope, one
// scope and label system and one provenance path.
//
// The model follows Kubernetes (APIVersion, Kind, Spec and Status, label selectors),
// because it is familiar and proven, so resources could export as CRDs without a
// cluster. They are Flynn types with their own API groups, never the upstream ones.
//
// Every kind is data, Kind included, so the agent can author and validate kinds at
// runtime. Typed domains are facades over it, and full-text, vector and ordering
// indexes are projections of the resource log.
package resource

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/ionalpha/flynn/envelope"
	"github.com/ionalpha/flynn/spine"
)

// ErrNotFound is returned when a requested resource does not exist.
var ErrNotFound = errors.New("resource: not found")

// ErrConflict means a non-zero SyncVersion no longer matches the stored record.
// Re-read and retry.
var ErrConflict = errors.New("resource: version conflict")

// ErrInvalid means admission failed: a missing required field, an unregistered
// kind, or a Spec that fails its kind's schema.
var ErrInvalid = errors.New("resource: invalid")

// Scope is the resource namespace, the three levels state.Scope defines, so a
// resource can be partitioned, resolved most specific first and shared across a
// fleet. The zero Scope is global, and Scope is comparable.
//
// Instance is one installation; Project nests inside it and Workspace inside
// Project. Each is an opaque label Flynn compares and never interprets; empty means
// not narrowed at that level. A host maps its hierarchy onto them or leaves them
// empty.
type Scope struct {
	Instance  string
	Project   string
	Workspace string
}

// OwnerReference records a resource's owner, the run or goal that created it. The
// garbage collector follows it, so deleting an owner cascades to its subtree. It is
// envelope metadata, outside the content hash.
type OwnerReference struct {
	// APIVersion and Kind identify the owner's kind.
	APIVersion string
	Kind       string
	// Name and ID address the owner. The collector resolves by ID; Name serves
	// selectors and debugging.
	Name string
	ID   string
	// Controller marks the one owner that manages the lifecycle; when it is gone or
	// terminating, the resource is collected.
	Controller bool
}

// Envelope is the metadata every resource carries: sync and concurrency, content
// provenance and bitemporal time.
type Envelope struct {
	// The sync envelope, shared with state records so fleet merge follows one set
	// of rules. Embedding inlines its fields, so wire format and hashes match.
	envelope.Envelope
	// WriterActor is who authored the last write: human, agent or system. Merge
	// ranks a human write first, so a person's correction outlives later automated
	// writes. It is outside the content hash; the zero value is the agent.
	WriterActor spine.ActorType

	// Finalizers are keys to clear before removal. While any remain, Delete sets
	// DeletionTimestamp and the resource stays readable, so each owner can clean up
	// and remove its key; removing the last one tombstones the record. A controller
	// re-reads the pending deletion every reconcile, so cleanup of a worktree or child
	// run survives a crash.
	Finalizers []string
	// DeletionTimestamp marks a terminating resource that still has finalizers; nil
	// means not being deleted. Only Delete sets it and resurrection clears it, so a Put
	// cannot fake or cancel a deletion.
	DeletionTimestamp *time.Time
	// OwnerReferences link to the owning resources. The controller owner drives the
	// lifecycle: the collector reaps the resource once it is gone or terminating. They
	// are outside the content hash. None means a root.
	OwnerReferences []OwnerReference

	// Version is the content revision, bumped on every content change, apart from
	// SyncVersion.
	Version int64
	// ContentHash is Hash of the canonical content.
	ContentHash string
	// SpecHash is SpecHash of the desired state, stamped on write so a controller's
	// no-op check reads a field.
	SpecHash string

	// ValidFrom and ValidTo are the valid-time interval; see ValidAt. Nil ValidFrom
	// means since creation, nil ValidTo still valid.
	ValidFrom *time.Time
	ValidTo   *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Resource is the record for every data-defined kind. ID is a stable unique id set
// on creation. Name is unique within (Kind, Scope) and is the handle for upserts
// and fetches. APIVersion is the kind's group/version.
//
// A kind with no natural name sets GenerateName instead, so every such record takes
// its name from the one deterministic ID source.
type Resource struct {
	APIVersion string
	Kind       string
	ID         string
	Name       string
	// GenerateName makes a create with empty Name assign Name = GenerateName + ID
	// ("mem-01J..."). It is consumed on write and never stored; a set Name wins. The ID
	// suffix is unique, so there is no collision to retry.
	GenerateName string
	Scope        Scope
	Labels       map[string]string
	Annotations  map[string]string
	// Spec is the desired state, validated against the kind's schema. It is raw JSON so
	// it embeds readably in events and hashes canonically.
	Spec json.RawMessage
	// Status is the observed state, set by controllers and not admitted against the
	// schema.
	Status json.RawMessage
	Envelope
}

// Key addresses a resource by (Kind, Scope, Name), the target of an upsert or
// fetch. Key is comparable.
type Key struct {
	Kind  string
	Scope Scope
	Name  string
}

// Key returns the resource's logical key.
func (r Resource) Key() Key { return Key{Kind: r.Kind, Scope: r.Scope, Name: r.Name} }

// Controller returns the controller owner reference and whether one is set.
func (r Resource) Controller() (OwnerReference, bool) {
	for _, o := range r.OwnerReferences {
		if o.Controller {
			return o, true
		}
	}
	return OwnerReference{}, false
}
