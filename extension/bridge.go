package extension

import (
	"sort"

	"github.com/ionalpha/flynn/mission"
)

// ToolSource is implemented by a [Point] whose surface contributes tools; an
// auth provider, for one, contributes none. Tools is called after OnLoad and
// returns nil for an extension with no tools.
type ToolSource interface {
	Tools(id string) []mission.Tool
}

// Tools returns the loaded extensions' tools ordered by extension id then tool
// name, so runs are reproducible. Listing a tool grants no authority to call
// it; dispatch checks the capability grant and credential.
func (l *Loader) Tools() []mission.Tool {
	l.mu.Lock()
	ids := make([]string, 0, len(l.loaded))
	for id := range l.loaded {
		ids = append(ids, id)
	}
	keysByID := make(map[string][]string, len(l.loaded))
	for id, keys := range l.loaded {
		cp := make([]string, len(keys))
		copy(cp, keys)
		keysByID[id] = cp
	}
	l.mu.Unlock()

	sort.Strings(ids)

	var out []mission.Tool
	for _, id := range ids {
		// One handler per surface, so no tool is collected twice.
		for _, key := range keysByID[id] {
			h, err := l.reg.Resolve(key)
			if err != nil {
				continue
			}
			src, ok := h.(ToolSource)
			if !ok {
				continue
			}
			out = append(out, src.Tools(id)...)
		}
	}
	return out
}
