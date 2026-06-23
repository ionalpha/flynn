package resource

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// CoreGroupVersion is the API group/version for the substrate's own built-in
// kinds. Domains define their own groups (e.g. skill.ionagent.io/v1); the
// `.ionagent.io` suffix marks every kind unmistakably as ours, never a Kubernetes
// built-in.
const CoreGroupVersion = "core.ionagent.io/v1alpha1"

// KindKind is the name of the kind that describes kinds. A Kind is itself stored
// as a Resource of this kind, which is what makes the type system data the agent
// can author and validate at runtime (meta-circular self-extension).
const KindKind = "Kind"

// Kind describes a resource kind: the API group/version its resources carry, the
// kind name, and the JSON Schema their Spec must satisfy (admission). A nil Schema
// means specs are unconstrained.
type Kind struct {
	APIVersion string          // group/version of resources of this kind, e.g. "skill.ionagent.io/v1"
	Name       string          // e.g. "Skill"
	Schema     json.RawMessage // JSON Schema for Spec; nil = unconstrained
	Singular   string          // optional display form
	Plural     string          // optional display form
}

func kindKey(apiVersion, name string) string { return apiVersion + "/" + name }

// Registry holds the registered kinds and validates resources against them. It is
// safe for concurrent use, and is the admission control point: a resource of an
// unregistered kind, or one whose Spec fails its kind's schema, is rejected before
// it is ever stored.
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

// WithSchemaCompiler overrides the schema compiler (default: the built-in,
// dependency-free subset compiler). A host can inject a full JSON Schema engine.
func WithSchemaCompiler(c SchemaCompiler) RegistryOption {
	return func(r *Registry) {
		if c != nil {
			r.compiler = c
		}
	}
}

// NewRegistry returns an empty registry using the built-in schema compiler unless
// one is injected.
func NewRegistry(opts ...RegistryOption) *Registry {
	r := &Registry{compiler: newBuiltinCompiler(), kinds: map[string]registeredKind{}}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Register adds (or replaces) a kind, compiling its JSON Schema up front so an
// invalid schema is rejected at registration rather than at first write.
func (r *Registry) Register(k Kind) error {
	if k.APIVersion == "" || k.Name == "" {
		return fmt.Errorf("%w: kind requires APIVersion and Name", ErrInvalid)
	}
	var compiled Validator
	if len(k.Schema) > 0 {
		c, err := r.compiler.Compile(k.Schema)
		if err != nil {
			return fmt.Errorf("%w: kind %q schema: %v", ErrInvalid, k.Name, err)
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

// Kinds returns every registered kind, ordered by group/version then name, for
// introspection ("what can this agent represent?").
func (r *Registry) Kinds() []Kind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Kind, 0, len(r.kinds))
	for _, rk := range r.kinds {
		out = append(out, rk.kind)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].APIVersion != out[j].APIVersion {
			return out[i].APIVersion < out[j].APIVersion
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Validate is admission: the kind must be registered, and spec must satisfy its
// JSON Schema (if the kind declares one). Returns an ErrInvalid-wrapped error
// otherwise.
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
		return fmt.Errorf("%w: spec is not valid JSON: %v", ErrInvalid, err)
	}
	if err := rk.schema.Validate(inst); err != nil {
		return fmt.Errorf("%w: spec does not satisfy kind %q schema: %v", ErrInvalid, kind, err)
	}
	return nil
}

// decodeInstance decodes raw JSON to a generic value for validation. Empty input
// is treated as an empty object, so a schema with no required fields admits an
// empty spec.
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
