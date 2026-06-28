package extension

import (
	"sort"

	"github.com/ionalpha/flynn/mission"
)

// ToolSource is the optional interface a Point implements when its
// surface contributes callable tools to the agent. Not every surface does (an auth
// provider contributes credentials, not tools), so it is separate from
// Point: the loader type-asserts for it and collects tools only from
// handlers that expose them. This is the tool-bridge boundary. The actual tools a
// surface builds (an API call per endpoint, a deploy action) are the surface
// handler's concern; the keystone only defines how they reach the agent.
//
// Tools returns the live tools mounted for one extension id, or nil if that
// extension contributes none. It is called after OnLoad has mounted the surface.
type ToolSource interface {
	Tools(id string) []mission.Tool
}

// Tools returns every tool contributed by the currently loaded extensions, in a
// deterministic order (by extension id, then by tool name), so the agent's tool
// surface is reproducible across runs. Authority to call a tool is enforced
// separately at the dispatch waist by the capability grant and credential check;
// presence here only makes a tool reachable, never automatically permitted.
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
		// Each mounted surface key resolves to a distinct handler (the registry holds
		// one handler per surface), so querying each yields that extension's full tool
		// set without duplication.
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
