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

// Store keeps credentials in the instance scope and holds the invariant that an
// integration has at most one default.
type Store struct {
	rs    resource.Store
	scope resource.Scope
	// mu serializes Put and SetDefault so two concurrent default writes cannot
	// leave an integration with two defaults.
	mu sync.Mutex
}

// NewStore returns a Store over rs, whose registry must already hold the kind
// (see [RegisterKind]).
func NewStore(rs resource.Store) *Store { return &Store{rs: rs} }

// Put creates or updates a credential. A new default clears the integration's
// other defaults. The vault is not touched; the caller writes the value under
// [Credential.Ref].
func (s *Store) Put(ctx context.Context, spec Spec) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putChecked(ctx, spec)
}

// putChecked runs with s.mu held.
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

// Get returns the named credential, or [ErrNotFound].
func (s *Store) Get(ctx context.Context, integration, name string) (Credential, error) {
	r, err := s.rs.Get(ctx, Kind, s.scope, resourceName(integration, name))
	if err != nil {
		return Credential{}, translateErr(err)
	}
	return toCredential(r)
}

// All returns every credential, ordered by integration then name.
func (s *Store) All(ctx context.Context) ([]Credential, error) {
	out, err := s.all(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Spec.Integration != out[j].Spec.Integration {
			return out[i].Spec.Integration < out[j].Spec.Integration
		}
		return out[i].Spec.Name < out[j].Spec.Name
	})
	return out, nil
}

// List returns one integration's credentials, ordered by name.
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

// Default returns the integration's default credential, or [ErrNotFound].
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

// Resolve selects a credential: "<integration>/<name>" by name, a bare
// "<integration>" its default.
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

// SetDefault makes the named credential the integration's only default.
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

// Delete removes a credential's metadata. The caller deletes the vault value
// separately.
func (s *Store) Delete(ctx context.Context, integration, name string) error {
	return translateErr(s.rs.Delete(ctx, Kind, s.scope, resourceName(integration, name)))
}

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

// put writes without clearing other defaults.
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

func toCredential(r resource.Resource) (Credential, error) {
	spec, err := DecodeSpec(r)
	if err != nil {
		return Credential{}, err
	}
	return Credential{ID: r.ID, Spec: spec, Version: int(r.Version)}, nil
}

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
