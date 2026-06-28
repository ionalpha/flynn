package extension

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/fault"
)

// Mount is what a handler receives when one surface of an extension is loaded. It
// carries the extension's identity and full spec (so the handler can read the base
// URL, auth, and safety envelope) alongside the raw block for the specific surface
// it handles. The handler decodes Block into its own type; nothing else does.
type Mount struct {
	// ID is the extension resource's stable id, the key OnUnload is called with.
	ID string
	// Name is the extension's natural name (its slug).
	Name string
	// Spec is the full decoded extension spec, shared context for every surface.
	Spec Spec
	// Surface is the surface key this mount is for (one of the Surface* constants or a
	// host-registered key).
	Surface string
	// Block is the raw typed block for this surface, for the handler to decode.
	Block json.RawMessage
}

// Point is the handler for one surface kind. Registering a Point
// under its Capability makes every spec that declares that surface loadable, which
// is how the engine gains new abilities without edits to the kind or the loader: a
// new surface is a new registration, not a new code path in the core.
//
// OnLoad is called when an extension declaring this surface is loaded; it wires the
// surface live (registers tools, opens a provider) and returns an error to abort
// the load. OnUnload is called with the extension id when the extension is
// unloaded or replaced; it must release whatever OnLoad acquired and be idempotent,
// since a roll-back may unload a surface that never fully loaded. Implementations
// must be safe for concurrent use.
type Point interface {
	// Capability is the surface key this handler serves (e.g. "integration").
	Capability() string
	// OnLoad wires one surface of an extension live.
	OnLoad(ctx context.Context, m Mount) error
	// OnUnload releases the surface previously loaded for the given extension id.
	OnUnload(ctx context.Context, id string) error
}

// Registry resolves a Point by the surface key it serves. It is
// fail-closed: a surface with no registered handler is an error at load time, never
// a silent skip, so an extension is either fully wired or rejected. It is safe for
// concurrent use.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Point
}

// NewRegistry returns an empty handler registry.
func NewRegistry() *Registry { return &Registry{handlers: map[string]Point{}} }

// Register adds a handler under its Capability. It refuses a nil handler, an empty
// capability, or a duplicate, so the registry can never hold two handlers for one
// surface (which would make routing ambiguous).
func (r *Registry) Register(h Point) error {
	if h == nil {
		return fault.New(fault.Terminal, "extension_handler_nil", "extension: refusing to register a nil handler")
	}
	capName := h.Capability()
	if capName == "" {
		return fault.New(fault.Terminal, "extension_handler_empty_capability", "extension: refusing to register a handler with an empty capability")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.handlers[capName]; ok {
		return fault.New(fault.Terminal, "extension_handler_duplicate", "extension: handler already registered for surface "+capName)
	}
	r.handlers[capName] = h
	return nil
}

// Resolve returns the handler for a surface key, or a Terminal error naming the
// available surfaces when none matches.
func (r *Registry) Resolve(capability string) (Point, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[capability]
	if !ok {
		return nil, fault.New(fault.Terminal, "extension_surface_unknown",
			"extension: no handler for surface "+capability+"; available: "+joinSorted(r.handlers))
	}
	return h, nil
}

// Has reports whether a handler is registered for a surface key.
func (r *Registry) Has(capability string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.handlers[capability]
	return ok
}

// Capabilities lists the registered surface keys in sorted order, for diagnostics
// and a kube-style listing of what surfaces this engine can serve.
func (r *Registry) Capabilities() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedKeys(r.handlers)
}

func sortedKeys(m map[string]Point) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func joinSorted(m map[string]Point) string {
	keys := sortedKeys(m)
	if len(keys) == 0 {
		return "(none)"
	}
	return strings.Join(keys, ", ")
}
