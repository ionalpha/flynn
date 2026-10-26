package dependency

import (
	"context"
	"embed"
	"fmt"
	"sync"

	"github.com/ionalpha/flynn/internal/embeddedspecs"
)

// specsFS holds the official dependency specs shipped in the binary. Each is a JSON file
// admitted against the Dependency kind like any other resource, so the set of programs
// Flynn knows how to provision is data, auditable in the tree, not code.
//
//go:embed specs/*.json
var specsFS embed.FS

// Entry is one official dependency from the embedded catalog.
type Entry = embeddedspecs.Entry[Spec]

var catalog struct {
	once     sync.Once
	entries  []Entry
	reserved map[string]bool
	err      error
}

// Entries returns the official dependency catalog, parsed once and ordered by name. A
// malformed embedded spec is a programming error in this package, surfaced as an error here
// and caught by the build-time gate.
func Entries() ([]Entry, error) {
	catalog.once.Do(loadCatalog)
	return catalog.entries, catalog.err
}

func loadCatalog() {
	catalog.entries, catalog.reserved, catalog.err = embeddedspecs.Parse[Spec](specsFS, embeddedspecs.Naming{Prefix: "dependency", Noun: "dependency"}, nil)
}

// Reserved reports whether a name belongs to an official bundled dependency, so a
// runtime-authored spec cannot impersonate one by claiming its name.
func Reserved(name string) bool {
	if _, err := Entries(); err != nil {
		return false
	}
	return catalog.reserved[name]
}

// Sync writes every bundled dependency spec into the store. It is idempotent: the resource
// store dedups an unchanged spec by content, so re-syncing on each start is a no-op. It
// returns the number of specs written.
func Sync(ctx context.Context, store *Store) (int, error) {
	entries, err := Entries()
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if _, err := store.Put(ctx, e.Name, e.Spec); err != nil {
			return 0, fmt.Errorf("dependency: sync %s: %w", e.Name, err)
		}
	}
	return len(entries), nil
}
