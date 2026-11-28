package resource

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// CoreGroupVersion is the API group/version of the built-in kinds. Domains define
// their own groups (skill.ionagent.io/v1); the .ionagent.io suffix keeps every kind
// distinct from a Kubernetes built-in.
const CoreGroupVersion = "core.ionagent.io/v1alpha1"

// KindKind is the kind that describes kinds. A Kind is stored as a Resource of this
// kind, so the agent can author and validate types at runtime.
const KindKind = "Kind"

// Kind describes a resource kind: its group/version, name and the JSON Schema its
// Spec must satisfy at admission. A nil Schema leaves specs unconstrained.
type Kind struct {
	APIVersion string          // group/version of resources of this kind, e.g. "skill.ionagent.io/v1"
	Name       string          // e.g. "Skill"
	Schema     json.RawMessage // JSON Schema for Spec; nil = unconstrained
	Singular   string          // optional display form
	Plural     string          // optional display form
}

func kindKey(apiVersion, name string) string { return apiVersion + "/" + name }

// Registry holds the registered kinds and is the admission point: a resource of an
// unregistered kind, or whose Spec fails its schema, is rejected before it is
// stored. It is safe for concurrent use.
type Registry struct {
	compiler SchemaCompiler
	mu       sync.RWMutex
	kinds    map[string]registeredKind
}

type registeredKind struct {
	kind   Kind
	schema Validator // compiled; nil when the kind has no schema
}

// RegistryOption configures a Registry.
type RegistryOption func(*Registry)

// WithSchemaCompiler replaces the built-in subset compiler, so a host can inject a
// full JSON Schema engine.
func WithSchemaCompiler(c SchemaCompiler) RegistryOption {
	return func(r *Registry) {
		if c != nil {
			r.compiler = c
		}
	}
}

// NewRegistry returns an empty registry.
func NewRegistry(opts ...RegistryOption) *Registry {
	r := &Registry{compiler: newBuiltinCompiler(), kinds: map[string]registeredKind{}}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Register adds or replaces a kind, compiling its schema now so an invalid schema
// fails at registration rather than at first write.
func (r *Registry) Register(k Kind) error {
	if k.APIVersion == "" || k.Name == "" {
		return fmt.Errorf("%w: kind requires APIVersion and Name", ErrInvalid)
	}
	var compiled Validator
	if len(k.Schema) > 0 {
		c, err := r.compiler.Compile(k.Schema)
		if err != nil {
			return fmt.Errorf("%w: kind %q schema: %w", ErrInvalid, k.Name, err)
		}
		compiled = c
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kinds[kindKey(k.APIVersion, k.Name)] = registeredKind{kind: k, schema: compiled}
	return nil
}

// Lookup returns the registered kind for (apiVersion, name).
func (r *Registry) Lookup(apiVersion, name string) (Kind, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rk, ok := r.kinds[kindKey(apiVersion, name)]
	return rk.kind, ok
}

// Kinds returns every registered kind, ordered by group/version then name.
func (r *Registry) Kinds() []Kind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Kind, 0, len(r.kinds))
	//ordered:ok sorted below by apiVersion and name, which are unique
	for _, rk := range r.kinds {
		out = append(out, rk.kind)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].APIVersion != out[j].APIVersion {
			return out[i].APIVersion < out[j].APIVersion
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Validate admits spec: the kind must be registered and spec must satisfy its
// schema, or the error wraps ErrInvalid.
func (r *Registry) Validate(apiVersion, kind string, spec []byte) error {
	r.mu.RLock()
	rk, ok := r.kinds[kindKey(apiVersion, kind)]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: unregistered kind %s/%s", ErrInvalid, apiVersion, kind)
	}
	if rk.schema == nil {
		return nil
	}
	inst, err := decodeInstance(spec)
	if err != nil {
		return fmt.Errorf("%w: spec is not valid JSON: %w", ErrInvalid, err)
	}
	if err := rk.schema.Validate(inst); err != nil {
		return fmt.Errorf("%w: spec does not satisfy kind %q schema: %w", ErrInvalid, kind, err)
	}
	return nil
}

// decodeInstance decodes raw JSON for validation. Empty input is an empty object,
// so a schema with no required fields admits an empty spec.
func decodeInstance(b []byte) (any, error) {
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return v, nil
}
