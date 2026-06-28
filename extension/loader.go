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

// Loader turns a stored Extension resource into live surfaces by routing each
// declared surface block to its registered handler. It is the bridge from data to
// behaviour: the resource store admits and versions the spec, and the loader
// mounts it. A load is all-or-nothing. If any surface fails to mount, the surfaces
// already mounted for that extension are unloaded before the error returns, so an
// extension is never left half-wired.
//
// The loader is level-triggered: Load both mounts a new extension and replaces one
// already loaded (it unmounts the previous surfaces first), so re-applying the same
// spec is idempotent and applying a changed spec reconciles to it. It is safe for
// concurrent use.
type Loader struct {
	reg *Registry

	mu     sync.Mutex
	loaded map[string][]string // extension id -> mounted surface keys (sorted)
}

// NewLoader returns a loader that resolves handlers from reg.
func NewLoader(reg *Registry) *Loader {
	return &Loader{reg: reg, loaded: map[string][]string{}}
}

// Load mounts every surface an extension declares. It decodes the spec, resolves a
// handler for each surface (fail-closed on an unknown surface), and calls OnLoad in
// a deterministic, sorted order so a load is reproducible. If the extension was
// already loaded, its previous surfaces are unmounted first. On any failure, the
// surfaces mounted during this call are rolled back and the original error is
// returned. It returns the sorted list of surface keys that ended up mounted.
func (l *Loader) Load(ctx context.Context, r resource.Resource) ([]string, error) {
	if r.ID == "" {
		return nil, fault.New(fault.Terminal, "extension_no_id", "extension: cannot load a resource with no id")
	}
	spec, err := DecodeSpec(r)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "extension_decode", err)
	}

	keys := sortedSurfaceKeys(spec.Surfaces)

	// Resolve every handler before mounting any, so an extension that declares one
	// unknown surface is rejected whole rather than partly mounted then rolled back.
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

	// Replacing an already-loaded extension: unmount its current surfaces first so
	// the new spec fully supersedes the old one.
	if prev, ok := l.loaded[r.ID]; ok {
		// A best-effort release of the prior surfaces; the new spec supersedes them
		// regardless, and OnUnload is idempotent, so a release error must not block the
		// reload.
		_ = l.unmount(ctx, r.ID, prev)
		delete(l.loaded, r.ID)
	}

	mounted := make([]string, 0, len(keys))
	for i, key := range keys {
		m := Mount{ID: r.ID, Name: r.Name, Spec: spec, Surface: key, Block: spec.Surfaces[key]}
		if err := handlers[i].OnLoad(ctx, m); err != nil {
			// Roll back the surfaces mounted so far, then surface the original error
			// (the rollback's own error is secondary to the load failure that triggered it).
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

// Unload releases every surface mounted for an extension id. It is idempotent:
// unloading an extension that is not loaded is a no-op. Handler OnUnload errors are
// collected and returned joined, but every surface is still attempted so one
// stubborn handler cannot strand the others.
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

// unmount calls OnUnload for each surface in reverse mount order, collecting
// errors. The caller holds l.mu. A handler that is no longer registered is skipped
// rather than failing the unload, so an unmount always makes progress.
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

// sortedSurfaceKeys returns the surface keys of a spec in sorted order, the
// deterministic mount order the loader uses so a load is reproducible.
func sortedSurfaceKeys(surfaces map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(surfaces))
	for k := range surfaces {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
