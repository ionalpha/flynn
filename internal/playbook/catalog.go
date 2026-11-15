package playbook

import (
	"context"
	"embed"
	"fmt"
	"sync"

	"github.com/ionalpha/flynn/internal/embeddedspecs"
)

// specsFS holds the bundled playbooks, admitted against the Playbook kind like any
// other resource.
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

// Entries returns the bundled playbooks, parsed once and ordered by name. The
// build-time gate fails on a malformed embedded spec.
func Entries() ([]Entry, error) {
	catalog.once.Do(loadCatalog)
	return catalog.entries, catalog.err
}

func loadCatalog() {
	catalog.entries, catalog.reserved, catalog.err = embeddedspecs.Parse[Spec](specsFS, embeddedspecs.Naming{Prefix: "playbook", Noun: "playbook"}, nil)
}

// Reserved reports whether name belongs to a bundled playbook, so a
// runtime-authored one cannot impersonate it.
func Reserved(name string) bool {
	if _, err := Entries(); err != nil {
		return false
	}
	return catalog.reserved[name]
}

// Sync writes every bundled playbook into store and returns the count. The store
// dedups an unchanged spec, so syncing on each start is a no-op.
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
