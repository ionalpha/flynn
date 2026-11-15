// Package skill implements state.SkillStore over a resource.Store. A skill is a
// resource of kind "Skill" whose name is the slug and whose scope is the namespace,
// so skills share one envelope, admission path and sync model with every other kind.
//
// Search ranks live skills by a case-insensitive scan; a backend can add a full-text
// projection of the same events without changing the contract.
package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/envelope"
	"github.com/ionalpha/flynn/recallplan"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

// GroupVersion is the skill kind's API group and version. The suffix keeps it apart
// from Kubernetes built-ins.
const GroupVersion = "skill.ionagent.io/v1"

// Kind is the resource kind name skills are stored under.
const Kind = "Skill"

// specSchema is checked at admission. It requires no field, so a skill with only a
// slug is valid, and refuses unknown keys.
var specSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string"},
    "description": {"type": "string"},
    "body": {"type": "string"},
    "tags": {"type": "array", "items": {"type": "string"}},
    "uses": {"type": "integer", "minimum": 0},
    "wins": {"type": "integer", "minimum": 0},
    "reads": {"type": "integer", "minimum": 0},
    "read_wins": {"type": "integer", "minimum": 0},
    "check": {"type": "string"}
  },
  "additionalProperties": false
}`)

// KindDef is the Skill kind registered with a resource registry.
var KindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       Kind,
	Schema:     specSchema,
	Singular:   "skill",
	Plural:     "skills",
}

// RegisterKind registers [KindDef] with reg. Registering again replaces it.
func RegisterKind(reg *resource.Registry) error { return reg.Register(KindDef) }

// spec is the Spec [specSchema] validates. Every field is omitempty, so adding a
// field leaves the hash of a skill stored without it unchanged.
type spec struct {
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Body        string   `json:"body,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	// Offers is stored under `uses`, which only ever counted offers. The schema
	// still admits `wins` so older specs validate, and nothing decodes it because it
	// counted offers on successful runs.
	Offers int    `json:"uses,omitempty"`
	Reads  int    `json:"reads,omitempty"`
	Wins   int    `json:"read_wins,omitempty"`
	Check  string `json:"check,omitempty"`
}

// Store is the state.SkillStore the agent uses.
type Store struct {
	rs resource.Store
}

var _ state.SkillStore = (*Store)(nil)

// NewStore returns a Store over rs, whose registry must hold [KindDef].
func NewStore(rs resource.Store) *Store { return &Store{rs: rs} }

// Upsert creates or updates the skill keyed by (Scope, Slug). Versioning, origin and
// optimistic concurrency come from the resource store.
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
// across every scope.
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

// Search returns live skills in every scope whose name, description, body and tags
// hold the words of query, case insensitive, best match first, at most limit (<= 0
// is no cap). When no skill holds every word it widens to any content word, as
// [recallplan.Plan] lays out. An empty query returns every live skill in slug order.
// Ranking before the cut makes limit a top-k.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]state.Skill, error) {
	rs, err := s.rs.ListAll(ctx, Kind, nil)
	if err != nil {
		return nil, err
	}
	all, err := toSkills(rs)
	if err != nil {
		return nil, err
	}
	q := strings.TrimSpace(query)
	var out []state.Skill
	if q == "" {
		out = append(out, all...)
		sort.Slice(out, func(i, j int) bool { return lessBySlug(out[i], out[j]) })
	} else {
		out = searchPlanned(all, q)
	}
	if out == nil {
		out = []state.Skill{}
	}
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

// resolve tries the id lookup first, then scans every scope for the slug.
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

// toResource carries SyncVersion so the store enforces the skill contract's opt-in
// optimistic concurrency.
func toResource(sk state.Skill) (resource.Resource, error) {
	body, err := json.Marshal(spec{Name: sk.Name, Description: sk.Description, Body: sk.Body, Tags: sk.Tags, Offers: sk.Offers, Reads: sk.Reads, Wins: sk.Wins, Check: sk.Check})
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
			Envelope: envelope.Envelope{
				SyncVersion:      sk.SyncVersion,
				OriginInstanceID: sk.OriginInstanceID,
			},
		},
	}, nil
}

func toSkill(r resource.Resource) (state.Skill, error) {
	sp, err := resource.DecodeSpec[spec](r)
	if err != nil {
		return state.Skill{}, fmt.Errorf("skill: decode spec: %w", err)
	}
	return state.Skill{
		ID:          r.ID,
		Slug:        r.Name,
		Name:        sp.Name,
		Description: sp.Description,
		Body:        sp.Body,
		Tags:        sp.Tags,
		Offers:      sp.Offers,
		Reads:       sp.Reads,
		Wins:        sp.Wins,
		Check:       sp.Check,
		Scope:       state.Scope(r.Scope),
		Version:     int(r.Version),
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
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

// lessBySlug breaks ties on ID because a slug can repeat across scopes.
func lessBySlug(a, b state.Skill) bool {
	if a.Slug != b.Slug {
		return a.Slug < b.Slug
	}
	return a.ID < b.ID
}

// searchPlanned matches terms against all fields joined, because a question's words
// are rarely all in one field.
func searchPlanned(skills []state.Skill, query string) []state.Skill {
	hits := recallplan.Select(query, skills, searchText, 0)
	score := make(map[string]float64, len(hits))
	out := make([]state.Skill, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Item)
		score[h.Item.ID] = matchScore(h.Item, h.Terms)
	}
	sort.Slice(out, func(i, j int) bool {
		if score[out[i].ID] != score[out[j].ID] {
			return score[out[i].ID] > score[out[j].ID]
		}
		return lessBySlug(out[i], out[j])
	})
	return out
}

func searchText(sk state.Skill) string {
	return strings.Join([]string{sk.Name, sk.Description, sk.Body, strings.Join(sk.Tags, " ")}, "\n")
}

// matchScore weights a hit by field: description 10, name 5, tags 3, body 1, because
// a passing mention in a long body says little. Body hits are capped at
// [maxBodyHitsScored] so a long document cannot outscore a description about the
// query. Term scores add, so more matched terms rank higher.
func matchScore(sk state.Skill, terms []string) float64 {
	total := 0.0
	for _, term := range terms {
		count := func(field string) int { return strings.Count(strings.ToLower(field), term) }
		body := count(sk.Body)
		if body > maxBodyHitsScored {
			body = maxBodyHitsScored
		}
		total += 10*float64(count(sk.Description)) +
			5*float64(count(sk.Name)) +
			3*float64(count(strings.Join(sk.Tags, " "))) +
			float64(body)
	}
	return total
}

// maxBodyHitsScored bounds how far repeating a word in a body can lift a skill.
const maxBodyHitsScored = 3

// translateErr maps resource errors to state.ErrConflict and state.ErrNotFound.
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
