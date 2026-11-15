package dependency

import (
	"context"
	"embed"
	"fmt"
	"sync"

	"github.com/ionalpha/flynn/internal/embeddedspecs"
)

// specsFS holds the official dependency specs shipped in the binary, each admitted
// against the Dependency kind, so the programs Flynn can provision are auditable
// data in the tree.
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

// Entries returns the official catalog, parsed once and ordered by name. A malformed
// embedded spec is a bug here, returned as an error and caught by the build-time
// gate.
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

// Sync writes every bundled spec into the store and returns the count written. The
// store dedups unchanged content, so syncing on every start is a no-op.
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
