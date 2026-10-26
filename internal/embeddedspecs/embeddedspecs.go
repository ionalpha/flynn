// Package embeddedspecs reads a directory of official JSON specs shipped inside the binary.
//
// Dependencies, playbooks and extensions each bundle their official set as one JSON file per
// resource, named for the resource. Reading that directory is the same job every time, and
// the rules it enforces are the ones that keep the official namespace trustworthy: a spec
// that does not decode fails the build-time gate, and two files cannot claim one name, so
// the reserved-name set a runtime-authored spec is checked against is unambiguous. They
// live here so a fourth bundled kind inherits them instead of re-deriving them.
package embeddedspecs

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Dir is the directory, at the root of the filesystem handed to [Parse], that holds the
// spec files.
const Dir = "specs"

// Entry is one official spec: its name (the file name without ".json"), the decoded spec,
// and the file's exact bytes, which are what gets admitted against the kind.
type Entry[S any] struct {
	Name string
	Spec S
	Raw  json.RawMessage
}

// Naming says how a package's errors read. Prefix leads every message and Noun fills the
// duplicate-name message ("duplicate official <Noun> name"); they differ only where the
// package name is not the thing it bundles.
type Naming struct {
	Prefix string
	Noun   string
}

// Parse reads every ".json" file under [Dir] in fsys, decodes each into an S, runs check
// on it when check is not nil, and returns the entries ordered by name together with the
// set of names they claim. Ordering by name makes a sync deterministic.
//
// It fails on the first file that cannot be read or decoded, that fails check, or whose
// name an earlier file already claimed. A partial result is never returned: a catalog with
// one entry missing would let a runtime-authored spec take that entry's name.
func Parse[S any](fsys fs.FS, n Naming, check func(name string, spec S) error) ([]Entry[S], map[string]bool, error) {
	files, err := fs.ReadDir(fsys, Dir)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: read embedded specs: %w", n.Prefix, err)
	}
	reserved := map[string]bool{}
	var entries []Entry[S]
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		raw, err := fs.ReadFile(fsys, path.Join(Dir, f.Name()))
		if err != nil {
			return nil, nil, fmt.Errorf("%s: read %s: %w", n.Prefix, f.Name(), err)
		}
		var spec S
		if err := json.Unmarshal(raw, &spec); err != nil {
			return nil, nil, fmt.Errorf("%s: decode %s: %w", n.Prefix, f.Name(), err)
		}
		name := strings.TrimSuffix(f.Name(), ".json")
		if reserved[name] {
			return nil, nil, fmt.Errorf("%s: duplicate official %s name %q", n.Prefix, n.Noun, name)
		}
		if check != nil {
			if err := check(name, spec); err != nil {
				return nil, nil, err
			}
		}
		reserved[name] = true
		entries = append(entries, Entry[S]{Name: name, Spec: spec, Raw: append(json.RawMessage(nil), raw...)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, reserved, nil
}
