// Package embeddedspecs reads a directory of official JSON specs shipped in the
// binary, one file per resource named for it. Dependencies, playbooks and
// extensions use it, so the rules that keep the official namespace trustworthy are
// written once: a spec that does not decode fails the build-time gate, and two files
// cannot claim one name, so the reserved-name set is unambiguous.
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

// Entry is one official spec: its name (the file name without ".json"), the decoded
// spec, and the file's exact bytes, which are what is admitted.
type Entry[S any] struct {
	Name string
	Spec S
	Raw  json.RawMessage
}

// Naming sets how a package's errors read: Prefix leads every message and Noun fills
// "duplicate official <Noun> name".
type Naming struct {
	Prefix string
	Noun   string
}

// Parse decodes every ".json" file under [Dir] in fsys into an S, runs check when
// non-nil, and returns the entries ordered by name, so a sync is deterministic,
// with the set of names they claim.
//
// It fails on the first unreadable, undecodable, check-failing or duplicate file
// and never returns a partial result, because a missing entry would let a
// runtime-authored spec take its name.
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
