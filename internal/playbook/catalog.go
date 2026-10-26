package playbook

import (
	"context"
	"embed"
	"fmt"
	"sync"

	"github.com/ionalpha/flynn/internal/embeddedspecs"
)

// specsFS holds the official playbooks shipped in the binary. Each is a JSON file admitted
// against the Playbook kind like any other resource, so the procedures Flynn ships knowing
// how to run are data in the tree, auditable, not code.
//
//go:embed specs/*.json
var specsFS embed.FS

// Entry is one official playbook from the embedded catalog.
type Entry = embeddedspecs.Entry[Spec]

var catalog struct {
	once     sync.Once
	entries  []Entry
	reserved map[string]bool
	err      error
}

// Entries returns the official playbook catalog, parsed once and ordered by name. A
// malformed embedded spec is a programming error in this package, surfaced as an error here
// and caught by the build-time gate.
func Entries() ([]Entry, error) {
	catalog.once.Do(loadCatalog)
	return catalog.entries, catalog.err
}

func loadCatalog() {
	catalog.entries, catalog.reserved, catalog.err = embeddedspecs.Parse[Spec](specsFS, embeddedspecs.Naming{Prefix: "playbook", Noun: "playbook"}, nil)
}

// Reserved reports whether a name belongs to an official bundled playbook, so a
// runtime-authored one cannot impersonate it.
func Reserved(name string) bool {
	if _, err := Entries(); err != nil {
		return false
	}
	return catalog.reserved[name]
}

// Sync writes every bundled playbook into the store. It is idempotent: the resource store
// dedups an unchanged spec by content, so re-syncing on each start is a no-op. It returns
// the number of playbooks written.
func Sync(ctx context.Context, store *Store) (int, error) {
	entries, err := Entries()
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if _, err := store.Put(ctx, e.Name, e.Spec); err != nil {
			return 0, fmt.Errorf("playbook: sync %s: %w", e.Name, err)
		}
	}
	return len(entries), nil
}
