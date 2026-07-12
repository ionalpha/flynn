// Package catalog ships a curated set of official Extension specs inside the binary
// and syncs them into the resource store, so a freshly installed Flynn already knows
// how to reach common services. The specs are data, not code: each is a JSON file in
// the embedded filesystem, admitted against the Extension kind like any other
// resource. A bundled extension carries a provenance label so it can be told apart
// from one the user authored or forked, and its name is reserved so a runtime spec
// cannot impersonate an official one.
package catalog

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/resource"
)

//go:embed specs/*.json
var specsFS embed.FS

const (
	// SourceLabel records where an extension came from. A bundled extension is one
	// this binary ships; a forked extension started bundled and was edited by the
	// user, which the sync must not overwrite.
	SourceLabel = "catalog.ionagent.io/source"
	// SourceBundled marks an extension synced from the embedded catalog.
	SourceBundled = "bundled"
	// SourceForked marks a bundled extension the user has taken ownership of.
	SourceForked = "forked"
	// SourceDev marks an extension linked from a locally-built binary for authoring.
	// It is unsigned by nature and runs only under an explicitly dev-enabled path; a
	// normal run refuses it. Like a forked extension, the catalog sync never touches
	// it (retireMissing only reclaims bundled entries), so a dev link survives a sync.
	SourceDev = "dev"
)

// Entry is one official extension from the embedded catalog: its name (the resource
// name it syncs under), the decoded spec, and the raw spec bytes stored verbatim so
// the resource carries exactly what shipped.
type Entry struct {
	Name string
	Spec extension.Spec
	Raw  json.RawMessage
}

// state caches the once-parsed catalog. Keeping it in a struct rather than loose
// package variables keeps the cached error a field, not a package-level error var.
var state struct {
	once     sync.Once
	entries  []Entry
	reserved map[string]bool
	err      error
}

// Entries returns the official catalog, parsed once and ordered by name so a sync is
// deterministic. A malformed embedded spec is a programming error in this package
// (the build-time gate exists to catch it), surfaced as an error here.
func Entries() ([]Entry, error) {
	state.once.Do(load)
	return state.entries, state.err
}

func load() {
	files, err := fs.ReadDir(specsFS, "specs")
	if err != nil {
		state.err = fmt.Errorf("catalog: read embedded specs: %w", err)
		return
	}
	state.reserved = map[string]bool{}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		raw, err := specsFS.ReadFile(path.Join("specs", f.Name()))
		if err != nil {
			state.err = fmt.Errorf("catalog: read %s: %w", f.Name(), err)
			return
		}
		var spec extension.Spec
		if err := json.Unmarshal(raw, &spec); err != nil {
			state.err = fmt.Errorf("catalog: decode %s: %w", f.Name(), err)
			return
		}
		name := strings.TrimSuffix(f.Name(), ".json")
		if state.reserved[name] {
			state.err = fmt.Errorf("catalog: duplicate official extension name %q", name)
			return
		}
		if err := checkProcessSource(name, spec); err != nil {
			state.err = err
			return
		}
		state.reserved[name] = true
		state.entries = append(state.entries, Entry{Name: name, Spec: spec, Raw: append(json.RawMessage(nil), raw...)})
	}
	sort.Slice(state.entries, func(i, j int) bool { return state.entries[i].Name < state.entries[j].Name })
}

// checkProcessSource enforces the one thing a bundled extension may never be: a path to
// unsigned local code. A catalog spec ships inside the binary and carries the official
// name, so a process surface in it must name a published release the resolver can prove
// the origin of, and must not carry a dev source at all (Release wins over Dev at resolve
// time, but a bundled spec should not even contain the field). A spec that gets this
// wrong is a mistake in this repository, and it fails here and in the build-time gate
// rather than at a user's runtime.
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

// Reserved reports whether a name belongs to an official bundled extension. The
// runtime-authoring path consults this so a user or the agent cannot register a spec
// that impersonates an official one by claiming its name.
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

// resourceFor builds the resource an entry syncs to: an Extension named for the
// entry, in the bundled scope, carrying the bundled-source label and the spec bytes
// verbatim.
func resourceFor(e Entry) resource.Resource {
	return resource.Resource{
		APIVersion: extension.GroupVersion,
		Kind:       extension.Kind,
		Name:       e.Name,
		Scope:      bundledScope,
		Labels:     map[string]string{SourceLabel: SourceBundled},
		// Copy the cached bytes so the stored resource never aliases the shared
		// catalog cache.
		Spec: append(json.RawMessage(nil), e.Raw...),
	}
}

// bundledScope is the scope official extensions live in: the instance-global scope,
// so they are available to every workspace without per-scope duplication.
var bundledScope = resource.Scope{}
