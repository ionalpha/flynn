package playbook

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
)

// specsFS holds the official playbooks shipped in the binary. Each is a JSON file admitted
// against the Playbook kind like any other resource, so the procedures Flynn ships knowing
// how to run are data in the tree, auditable, not code.
//
//go:embed specs/*.json
var specsFS embed.FS

// Entry is one official playbook from the embedded catalog.
type Entry struct {
	Name string
	Spec Spec
	Raw  json.RawMessage
}

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
	files, err := fs.ReadDir(specsFS, "specs")
	if err != nil {
		catalog.err = fmt.Errorf("playbook: read embedded specs: %w", err)
		return
	}
	catalog.reserved = map[string]bool{}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		raw, err := specsFS.ReadFile(path.Join("specs", f.Name()))
		if err != nil {
			catalog.err = fmt.Errorf("playbook: read %s: %w", f.Name(), err)
			return
		}
		var spec Spec
		if err := json.Unmarshal(raw, &spec); err != nil {
			catalog.err = fmt.Errorf("playbook: decode %s: %w", f.Name(), err)
			return
		}
		name := strings.TrimSuffix(f.Name(), ".json")
		if catalog.reserved[name] {
			catalog.err = fmt.Errorf("playbook: duplicate official playbook name %q", name)
			return
		}
		catalog.reserved[name] = true
		catalog.entries = append(catalog.entries, Entry{Name: name, Spec: spec, Raw: append(json.RawMessage(nil), raw...)})
	}
	sort.Slice(catalog.entries, func(i, j int) bool { return catalog.entries[i].Name < catalog.entries[j].Name })
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
