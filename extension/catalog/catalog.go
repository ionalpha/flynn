// Package catalog embeds the official Extension specs as JSON and syncs them
// into the resource store. Bundled extensions carry a provenance label, and
// their names are reserved so a runtime spec cannot impersonate one.
package catalog

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sync"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/internal/embeddedspecs"
	"github.com/ionalpha/flynn/resource"
)

//go:embed specs/*.json
var specsFS embed.FS

const (
	// SourceLabel records where an extension came from.
	SourceLabel = "catalog.ionagent.io/source"
	// SourceBundled marks an extension synced from the embedded catalog.
	SourceBundled = "bundled"
	// SourceForked marks a bundled extension the user owns; Sync skips it.
	SourceForked = "forked"
	// SourceDev marks an unsigned local build. It runs only in dev mode, and
	// Sync never touches it.
	SourceDev = "dev"
)

// Entry is one official extension. Raw is stored verbatim, so the resource
// carries exactly what shipped.
type Entry = embeddedspecs.Entry[extension.Spec]

var state struct {
	once     sync.Once
	entries  []Entry
	reserved map[string]bool
	err      error
}

// Entries returns the catalog ordered by name. An error means a malformed
// embedded spec, which the build-time gate should have caught.
func Entries() ([]Entry, error) {
	state.once.Do(load)
	return state.entries, state.err
}

func load() {
	state.entries, state.reserved, state.err = parseCatalog(specsFS)
}

// parseCatalog takes fsys so tests can reach error paths the valid embedded
// catalog never hits.
func parseCatalog(fsys fs.FS) ([]Entry, map[string]bool, error) {
	return embeddedspecs.Parse(fsys, embeddedspecs.Naming{Prefix: "catalog", Noun: "extension"}, checkProcessSource)
}

// checkProcessSource requires a bundled process surface to name a release and
// carry no dev source at all, even though Release would win at resolve time.
func checkProcessSource(name string, spec extension.Spec) error {
	raw, ok := spec.Surfaces[extension.SurfaceProcess]
	if !ok {
		return nil
	}
	var block extension.ProcessBlock
	if err := json.Unmarshal(raw, &block); err != nil {
		return fmt.Errorf("catalog: %s: decode process surface: %w", name, err)
	}
	if block.Dev != nil {
		return fmt.Errorf("catalog: %s: bundled extension declares a dev source; official extensions run only signed releases", name)
	}
	if block.Release == nil || block.Release.Asset == "" || block.Release.Version == "" {
		return fmt.Errorf("catalog: %s: process surface must declare a release with an asset and a version", name)
	}
	return nil
}

// Reserved reports whether a name is official, so runtime authoring cannot
// claim it.
func Reserved(name string) bool {
	if _, err := Entries(); err != nil {
		return false
	}
	return state.reserved[name]
}

// Names returns the official extension names in sorted order.
func Names() ([]string, error) {
	entries, err := Entries()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names, nil
}

func resourceFor(e Entry) resource.Resource {
	return resource.Resource{
		APIVersion: extension.GroupVersion,
		Kind:       extension.Kind,
		Name:       e.Name,
		Scope:      bundledScope,
		Labels:     map[string]string{SourceLabel: SourceBundled},
		// Copy so the resource never aliases the shared cache.
		Spec: append(json.RawMessage(nil), e.Raw...),
	}
}

// bundledScope is instance-global, so every workspace sees official
// extensions.
var bundledScope = resource.Scope{}
