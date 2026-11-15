package extension

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// Loader mounts each surface of a stored extension through its handler. A load
// is all-or-nothing: on failure, surfaces already mounted are unloaded first.
// Load is level-triggered, so re-applying a spec is idempotent and a changed
// spec replaces the old one. Safe for concurrent use.
type Loader struct {
	reg *Registry

	mu     sync.Mutex
	loaded map[string][]string // extension id -> mounted surface keys (sorted)
}

// NewLoader returns a loader that resolves handlers from reg.
func NewLoader(reg *Registry) *Loader {
	return &Loader{reg: reg, loaded: map[string][]string{}}
}

// Load mounts every declared surface in sorted order and returns the mounted
// keys. An unknown surface fails the load. On failure this call's mounts are
// rolled back and the original error returned.
func (l *Loader) Load(ctx context.Context, r resource.Resource) ([]string, error) {
	if r.ID == "" {
		return nil, fault.New(fault.Terminal, "extension_no_id", "extension: cannot load a resource with no id")
	}
	spec, err := DecodeSpec(r)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "extension_decode", err)
	}

	keys := sortedSurfaceKeys(spec.Surfaces)

	// Resolve every handler before mounting any, so an unknown surface
	// rejects the extension without a partial mount.
	handlers := make([]Point, len(keys))
	for i, key := range keys {
		h, err := l.reg.Resolve(key)
		if err != nil {
			return nil, err
		}
		handlers[i] = h
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if prev, ok := l.loaded[r.ID]; ok {
		// Best effort: OnUnload is idempotent and the new spec supersedes the
		// old, so an unload error must not block the reload.
		_ = l.unmount(ctx, r.ID, prev)
		delete(l.loaded, r.ID)
	}

	mounted := make([]string, 0, len(keys))
	for i, key := range keys {
		m := Mount{ID: r.ID, Name: r.Name, Spec: spec, Surface: key, Block: spec.Surfaces[key]}
		if err := handlers[i].OnLoad(ctx, m); err != nil {
			// Return the load error; a rollback error is secondary.
			_ = l.unmount(ctx, r.ID, mounted)
			return nil, fault.Wrap(fault.Terminal, "extension_surface_load", err)
		}
		mounted = append(mounted, key)
	}

	if len(mounted) > 0 {
		l.loaded[r.ID] = mounted
	}
	return mounted, nil
}

// Unload releases every surface for id, idempotently. Errors are joined, and
// every surface is attempted so one failing handler strands no others.
func (l *Loader) Unload(ctx context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys, ok := l.loaded[id]
	if !ok {
		return nil
	}
	err := l.unmount(ctx, id, keys)
	delete(l.loaded, id)
	return err
}

// Mounted reports the sorted surface keys currently mounted for an extension id.
func (l *Loader) Mounted(id string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := l.loaded[id]
	out := make([]string, len(keys))
	copy(out, keys)
	return out
}

// unmount unloads in reverse mount order, skipping unregistered handlers so it
// always progresses. The caller holds l.mu.
func (l *Loader) unmount(ctx context.Context, id string, keys []string) error {
	var errs []error
	for i := len(keys) - 1; i >= 0; i-- {
		h, err := l.reg.Resolve(keys[i])
		if err != nil {
			continue
		}
		if err := h.OnUnload(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fault.Wrap(fault.Terminal, "extension_unload", errors.Join(errs...))
}

func sortedSurfaceKeys(surfaces map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(surfaces))
	for k := range surfaces {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
