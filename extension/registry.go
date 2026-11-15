package extension

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/fault"
)

// Mount is what a [Point] receives for one loaded surface. Only the handler
// decodes Block.
type Mount struct {
	// ID is the key OnUnload is called with.
	ID      string
	Name    string
	Spec    Spec
	Surface string
	Block   json.RawMessage
}

// Point handles one surface kind; registering one makes every spec declaring
// that surface loadable. OnLoad returns an error to abort the load. OnUnload
// must release what OnLoad acquired and be idempotent, since a roll-back may
// unload a surface that never fully loaded. Implementations must be safe for
// concurrent use.
type Point interface {
	Capability() string
	OnLoad(ctx context.Context, m Mount) error
	OnUnload(ctx context.Context, id string) error
}

// Registry resolves a [Point] by surface key. An unregistered surface fails
// the load, so an extension is fully wired or rejected. Safe for concurrent
// use.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Point
}

// NewRegistry returns an empty handler registry.
func NewRegistry() *Registry { return &Registry{handlers: map[string]Point{}} }

// Register refuses a nil handler, an empty capability, or a duplicate.
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

// Resolve returns the handler, or a Terminal error naming the available
// surfaces.
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

// Capabilities lists the registered surface keys, sorted.
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
