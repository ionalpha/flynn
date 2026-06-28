package controlplane

import (
	"context"
	"sort"

	"github.com/ionalpha/flynn/resource"
)

// ChangeKind labels how a resource changed between two polls.
type ChangeKind string

const (
	// Added is a resource that appeared since the last poll.
	Added ChangeKind = "Added"
	// Modified is a resource whose stored version advanced since the last poll.
	Modified ChangeKind = "Modified"
	// Deleted is a resource that was live at the last poll and is now gone.
	Deleted ChangeKind = "Deleted"
)

// Change is one resource transition reported by a Watcher.
type Change struct {
	Kind     ChangeKind
	Resource resource.Resource
}

// Watcher reports resource changes for a kind. It is the interface a remote API
// serves over a streaming transport; locally it is driven by polling. Defining it
// alongside List and Describe keeps list, get, and watch one read model, so adding
// live streaming later is a new transport over this interface, not a new query
// path bolted on.
type Watcher interface {
	Poll(ctx context.Context) ([]Change, error)
}

// PollWatcher detects changes by comparing successive list snapshots of a kind. It
// holds the last-seen sync version per resource id; each Poll returns the
// transitions since the previous Poll and advances the baseline. A production
// caller drives Poll on a ticker; tests drive it directly, so change detection is
// deterministic and free of any clock. The first Poll reports every current
// resource as Added.
type PollWatcher struct {
	store resource.Store
	kind  string
	sel   resource.Selector
	seen  map[string]seenRecord
}

type seenRecord struct {
	syncVersion int64
	resource    resource.Resource
}

// NewPollWatcher returns a watcher over the live resources of kind matching sel.
func NewPollWatcher(store resource.Store, kind string, sel resource.Selector) *PollWatcher {
	return &PollWatcher{store: store, kind: kind, sel: sel, seen: map[string]seenRecord{}}
}

// Poll lists the kind and returns the changes since the previous Poll: Added for
// ids not seen before, Modified for ids whose SyncVersion advanced, and Deleted
// for ids that were present before and are absent now. Changes are ordered by
// resource name then id, so the output is deterministic.
func (w *PollWatcher) Poll(ctx context.Context) ([]Change, error) {
	rs, err := w.store.ListAll(ctx, w.kind, w.sel)
	if err != nil {
		return nil, err
	}
	next := make(map[string]seenRecord, len(rs))
	var changes []Change
	for _, r := range rs {
		next[r.ID] = seenRecord{syncVersion: r.SyncVersion, resource: r}
		switch prev, ok := w.seen[r.ID]; {
		case !ok:
			changes = append(changes, Change{Kind: Added, Resource: r})
		case r.SyncVersion != prev.syncVersion:
			changes = append(changes, Change{Kind: Modified, Resource: r})
		}
	}
	for id, prev := range w.seen {
		if _, ok := next[id]; !ok {
			changes = append(changes, Change{Kind: Deleted, Resource: prev.resource})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		a, b := changes[i].Resource, changes[j].Resource
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	w.seen = next
	return changes, nil
}

var _ Watcher = (*PollWatcher)(nil)
