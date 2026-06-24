// Package skill provides skills as a typed facade over the unified resource
// substrate. A skill is stored as a resource.Resource of kind "Skill": the slug is
// the resource name, the scope is the resource namespace, and the name/body/tags
// live in a schema-validated Spec. The facade implements state.SkillStore, so call
// sites keep the same ergonomic, type-safe API while the data lives on one
// event-sourced store with one envelope, one schema/admission path, and one
// provenance/sync model shared with every other kind.
//
// Search is a read model over that store: the facade ranks live skills by a
// case-insensitive scan today, and a backend can maintain a full-text projection of
// the same resource events without changing this contract.
package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

// GroupVersion is the skill kind's API group/version. The `.ionagent.io` suffix
// marks it unmistakably as ours, never a Kubernetes built-in.
const GroupVersion = "skill.ionagent.io/v1"

// Kind is the resource kind name skills are stored under.
const Kind = "Skill"

// specSchema is the JSON Schema a skill's Spec must satisfy (admission). It
// constrains structure (typed fields, no stray keys) without over-requiring, so a
// skill that carries only a slug is still valid.
var specSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string"},
    "body": {"type": "string"},
    "tags": {"type": "array", "items": {"type": "string"}},
    "uses": {"type": "integer", "minimum": 0},
    "wins": {"type": "integer", "minimum": 0}
  },
  "additionalProperties": false
}`)

// KindDef is the Skill kind definition, the value registered with a resource
// registry so the store admits skills.
var KindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       Kind,
	Schema:     specSchema,
	Singular:   "skill",
	Plural:     "skills",
}

// RegisterKind registers the Skill kind with reg so a resource store admits
// skills. It is idempotent: registering again replaces the definition.
func RegisterKind(reg *resource.Registry) error { return reg.Register(KindDef) }

// spec is the typed shape of a skill resource's Spec (the JSON validated by
// specSchema). Empty fields are omitted so a bare skill hashes and validates as a
// minimal object.
type spec struct {
	Name string   `json:"name,omitempty"`
	Body string   `json:"body,omitempty"`
	Tags []string `json:"tags,omitempty"`
	Uses int      `json:"uses,omitempty"`
	Wins int      `json:"wins,omitempty"`
}

// Store is the typed skill facade over a resource.Store. It is the SkillStore the
// agent uses; underneath, every read and write is a resource operation on one
// event-sourced substrate.
type Store struct {
	rs resource.Store
}

var _ state.SkillStore = (*Store)(nil)

// NewStore returns a skill facade over rs. The caller must have registered the
// Skill kind with the registry rs admits against (see RegisterKind).
func NewStore(rs resource.Store) *Store { return &Store{rs: rs} }

// Upsert creates or updates a skill keyed by (Scope, Slug), mapping it to a Skill
// resource and back. Versioning, origin preservation, and optimistic concurrency
// are the resource substrate's, so the contract matches every other kind.
func (s *Store) Upsert(ctx context.Context, sk state.Skill) (state.Skill, error) {
	r, err := toResource(sk)
	if err != nil {
		return state.Skill{}, err
	}
	out, err := s.rs.Put(ctx, r)
	if err != nil {
		return state.Skill{}, translateErr(err)
	}
	return toSkill(out)
}

// Get returns a live skill by id or slug, or state.ErrNotFound. A slug is resolved
// across every scope (the SkillStore handle is scope-independent).
func (s *Store) Get(ctx context.Context, idOrSlug string) (state.Skill, error) {
	r, err := s.resolve(ctx, idOrSlug)
	if err != nil {
		return state.Skill{}, err
	}
	return toSkill(r)
}

// List returns the live skills in a scope, ordered by slug.
func (s *Store) List(ctx context.Context, scope state.Scope) ([]state.Skill, error) {
	rs, err := s.rs.List(ctx, Kind, resource.Scope(scope), nil)
	if err != nil {
		return nil, err
	}
	return toSkills(rs)
}

// Search returns live skills whose name, body, or tags contain query (case
// insensitive), across every scope, ordered by slug and capped at limit (limit <= 0
// means no cap). An empty query matches every live skill.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]state.Skill, error) {
	rs, err := s.rs.ListAll(ctx, Kind, nil)
	if err != nil {
		return nil, err
	}
	all, err := toSkills(rs)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]state.Skill, 0, len(all))
	for _, sk := range all {
		if q == "" || matches(sk, q) {
			out = append(out, sk)
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessBySlug(out[i], out[j]) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Delete tombstones a skill by id or slug, or returns state.ErrNotFound.
func (s *Store) Delete(ctx context.Context, idOrSlug string) error {
	r, err := s.resolve(ctx, idOrSlug)
	if err != nil {
		return err
	}
	return translateErr(s.rs.Delete(ctx, Kind, r.Scope, r.Name))
}

// resolve finds a live skill resource by id (fast path) or slug (a scope-spanning
// scan, since the slug handle carries no scope). Returns state.ErrNotFound if no
// live skill matches.
func (s *Store) resolve(ctx context.Context, idOrSlug string) (resource.Resource, error) {
	r, err := s.rs.GetByID(ctx, idOrSlug)
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, resource.ErrNotFound) {
		return resource.Resource{}, translateErr(err)
	}
	all, err := s.rs.ListAll(ctx, Kind, nil)
	if err != nil {
		return resource.Resource{}, err
	}
	for _, r := range all {
		if r.Name == idOrSlug {
			return r, nil
		}
	}
	return resource.Resource{}, state.ErrNotFound
}

// toResource maps a skill to its Skill resource. The slug is the resource name and
// the scope is the namespace; the sync version is carried through so the substrate
// enforces the same opt-in optimistic concurrency the skill contract promises.
func toResource(sk state.Skill) (resource.Resource, error) {
	body, err := json.Marshal(spec{Name: sk.Name, Body: sk.Body, Tags: sk.Tags, Uses: sk.Uses, Wins: sk.Wins})
	if err != nil {
		return resource.Resource{}, fmt.Errorf("skill: encode spec: %w", err)
	}
	return resource.Resource{
		APIVersion: GroupVersion,
		Kind:       Kind,
		ID:         sk.ID,
		Name:       sk.Slug,
		Scope:      resource.Scope(sk.Scope),
		Spec:       body,
		Envelope: resource.Envelope{
			SyncVersion:      sk.SyncVersion,
			OriginInstanceID: sk.OriginInstanceID,
		},
	}, nil
}

// toSkill maps a Skill resource back to the typed skill. The resource's content
// Version is the skill's revision, and the shared envelope fields carry across.
func toSkill(r resource.Resource) (state.Skill, error) {
	var sp spec
	if len(r.Spec) > 0 {
		if err := json.Unmarshal(r.Spec, &sp); err != nil {
			return state.Skill{}, fmt.Errorf("skill: decode spec: %w", err)
		}
	}
	return state.Skill{
		ID:        r.ID,
		Slug:      r.Name,
		Name:      sp.Name,
		Body:      sp.Body,
		Tags:      sp.Tags,
		Uses:      sp.Uses,
		Wins:      sp.Wins,
		Scope:     state.Scope(r.Scope),
		Version:   int(r.Version),
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
		Envelope: state.Envelope{
			SyncVersion:      r.SyncVersion,
			OriginInstanceID: r.OriginInstanceID,
			UpdatedHLC:       r.UpdatedHLC,
			LastWriterID:     r.LastWriterID,
			Deleted:          r.Deleted,
		},
	}, nil
}

func toSkills(rs []resource.Resource) ([]state.Skill, error) {
	out := make([]state.Skill, 0, len(rs))
	for _, r := range rs {
		sk, err := toSkill(r)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, nil
}

// lessBySlug orders skills by slug with an ID tiebreak, a total order even when a
// slug repeats across scopes.
func lessBySlug(a, b state.Skill) bool {
	if a.Slug != b.Slug {
		return a.Slug < b.Slug
	}
	return a.ID < b.ID
}

// matches reports whether a skill's name, body, or tags contain the lowercased
// query.
func matches(sk state.Skill, lowerQuery string) bool {
	return strings.Contains(strings.ToLower(sk.Name), lowerQuery) ||
		strings.Contains(strings.ToLower(sk.Body), lowerQuery) ||
		strings.Contains(strings.ToLower(strings.Join(sk.Tags, " ")), lowerQuery)
}

// translateErr maps the resource substrate's errors onto the state seam's, so a
// SkillStore caller sees state.ErrConflict / state.ErrNotFound regardless of the
// backing store.
func translateErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, resource.ErrConflict):
		return state.ErrConflict
	case errors.Is(err, resource.ErrNotFound):
		return state.ErrNotFound
	default:
		return err
	}
}
