package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/resource"
)

// ErrNotFound is returned when a credential does not exist.
var ErrNotFound = errors.New("credential: not found")

// ErrInvalid is returned when a credential is missing a required field.
var ErrInvalid = errors.New("credential: invalid")

// Store is the typed credential facade over a resource.Store. Credentials live in
// one scope (the instance's), addressed by "<integration>/<name>"; the facade keeps
// the single-default-per-integration invariant and resolves a selection reference to
// a concrete credential.
type Store struct {
	rs    resource.Store
	scope resource.Scope
	// mu serializes the read-modify-write operations that maintain the
	// single-default-per-integration invariant (Put and SetDefault), so two concurrent
	// default writes cannot interleave and leave an integration with two defaults.
	mu sync.Mutex
}

// NewStore returns a credential facade over rs. The caller must have registered the
// Credential kind with the registry rs admits against (see RegisterKind).
func NewStore(rs resource.Store) *Store { return &Store{rs: rs} }

// Put creates or updates a credential. When the credential is marked default, any
// other default for the same integration is cleared first, so an integration always
// has at most one default. The secret value is not touched here: a credential is
// metadata, and the value is written to the vault separately under the credential's
// Ref.
func (s *Store) Put(ctx context.Context, spec Spec) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putChecked(ctx, spec)
}

// putChecked validates a spec and applies the default invariant. The caller holds
// s.mu, so the clear-then-write is atomic against other invariant operations.
func (s *Store) putChecked(ctx context.Context, spec Spec) (Credential, error) {
	if spec.Integration == "" || spec.Name == "" {
		return Credential{}, fmt.Errorf("%w: a credential needs an integration and a name", ErrInvalid)
	}
	if strings.ContainsRune(spec.Integration, '/') || strings.ContainsRune(spec.Name, '/') {
		// The resource name and the selection reference both join on "/", so a "/" in
		// either field would make two different credentials collide on one key.
		return Credential{}, fmt.Errorf("%w: integration and name must not contain '/'", ErrInvalid)
	}
	if spec.Role != "" && !spec.Role.Valid() {
		return Credential{}, fmt.Errorf("%w: unknown role %q", ErrInvalid, spec.Role)
	}
	if spec.IsDefault {
		// Write the new default before clearing the old one, so a concurrent reader
		// resolving the integration's default never falls into a window with none.
		out, err := s.put(ctx, spec)
		if err != nil {
			return Credential{}, err
		}
		if err := s.clearDefaults(ctx, spec.Integration, spec.Name); err != nil {
			return Credential{}, err
		}
		return out, nil
	}
	return s.put(ctx, spec)
}

// Get returns the credential named within an integration, or ErrNotFound.
func (s *Store) Get(ctx context.Context, integration, name string) (Credential, error) {
	r, err := s.rs.Get(ctx, Kind, s.scope, resourceName(integration, name))
	if err != nil {
		return Credential{}, translateErr(err)
	}
	return toCredential(r)
}

// List returns the credentials of an integration, ordered by name.
func (s *Store) List(ctx context.Context, integration string) ([]Credential, error) {
	all, err := s.all(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Credential, 0, len(all))
	for _, c := range all {
		if c.Spec.Integration == integration {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out, nil
}

// Default returns an integration's default credential, or ErrNotFound when the
// integration has no default.
func (s *Store) Default(ctx context.Context, integration string) (Credential, error) {
	creds, err := s.List(ctx, integration)
	if err != nil {
		return Credential{}, err
	}
	for _, c := range creds {
		if c.Spec.IsDefault {
			return c, nil
		}
	}
	return Credential{}, ErrNotFound
}

// Resolve resolves a selection reference to a concrete credential. A reference of
// the form "<integration>/<name>" selects that credential by name; a bare
// "<integration>" selects the integration's default. It is the call-site entry
// point: a request that names a credential gets exactly that one, and a request that
// names only the integration gets its default.
func (s *Store) Resolve(ctx context.Context, ref string) (Credential, error) {
	integration, name, named := strings.Cut(ref, "/")
	if integration == "" {
		return Credential{}, fmt.Errorf("%w: empty credential reference", ErrInvalid)
	}
	if named {
		// A slash was given, so a name was intended; an empty name is a malformed
		// reference rather than a request for the default.
		if name == "" {
			return Credential{}, fmt.Errorf("%w: malformed credential reference %q", ErrInvalid, ref)
		}
		return s.Get(ctx, integration, name)
	}
	return s.Default(ctx, integration)
}

// SetDefault makes the named credential the integration's default, clearing any
// previous default.
func (s *Store) SetDefault(ctx context.Context, integration, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.Get(ctx, integration, name)
	if err != nil {
		return err
	}
	c.Spec.IsDefault = true
	_, err = s.putChecked(ctx, c.Spec)
	return err
}

// Delete removes a credential. It does not touch the vault; deleting the secret
// value is the caller's separate, deliberate step.
func (s *Store) Delete(ctx context.Context, integration, name string) error {
	return translateErr(s.rs.Delete(ctx, Kind, s.scope, resourceName(integration, name)))
}

// clearDefaults unsets IsDefault on every default credential of an integration whose
// name differs from keep, so a new default does not coexist with the old one.
func (s *Store) clearDefaults(ctx context.Context, integration, keep string) error {
	creds, err := s.List(ctx, integration)
	if err != nil {
		return err
	}
	for _, c := range creds {
		if c.Spec.IsDefault && c.Spec.Name != keep {
			c.Spec.IsDefault = false
			if _, err := s.put(ctx, c.Spec); err != nil {
				return err
			}
		}
	}
	return nil
}

// put writes a credential resource without the default-clearing logic, the inner
// half of Put used both by Put and by clearDefaults.
func (s *Store) put(ctx context.Context, spec Spec) (Credential, error) {
	body, err := json.Marshal(spec)
	if err != nil {
		return Credential{}, fmt.Errorf("credential: encode spec: %w", err)
	}
	out, err := s.rs.Put(ctx, resource.Resource{
		APIVersion: GroupVersion,
		Kind:       Kind,
		Name:       resourceName(spec.Integration, spec.Name),
		Scope:      s.scope,
		Labels:     map[string]string{integrationLabel: spec.Integration},
		Spec:       body,
	})
	if err != nil {
		return Credential{}, translateErr(err)
	}
	return toCredential(out)
}

// all returns every credential across the store's scope.
func (s *Store) all(ctx context.Context) ([]Credential, error) {
	rs, err := s.rs.List(ctx, Kind, s.scope, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Credential, 0, len(rs))
	for _, r := range rs {
		c, err := toCredential(r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// toCredential maps a resource to the typed credential.
func toCredential(r resource.Resource) (Credential, error) {
	spec, err := DecodeSpec(r)
	if err != nil {
		return Credential{}, err
	}
	return Credential{ID: r.ID, Spec: spec, Version: int(r.Version)}, nil
}

// translateErr maps the resource store's errors onto this package's sentinels.
func translateErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, resource.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, resource.ErrInvalid):
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	default:
		return err
	}
}
