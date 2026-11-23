// Package skilltool offers the second and third stages of Agent Skills disclosure,
// the SKILL.md body and the resources it addresses, as tools the agent calls.
// Recall puts stage one, name and description, in the prompt.
//
// A tool call is an event on the spine, so a skill that was read can be told apart
// from one that was only offered, and a run's outcome can be credited to what it
// read. Text pasted into a prompt gives no such record.
//
// Every tool returns the whole result or an error naming what was wrong. Nothing
// truncates, because a model working from half a procedure is worse off than one
// told it failed.
package skilltool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/skill/bundled"
	"github.com/ionalpha/flynn/skill/skillmd"
	"github.com/ionalpha/flynn/state"
)

// MaxBodyRunes caps the body skill_read returns, at about twice the specification's
// 5,000-token recommendation, so only a malformed skill reaches it. A longer body is
// refused, never cut.
const MaxBodyRunes = 40_000

var (
	// ErrUnknownSkill reports a name no skill answers to, a wrong name rather than a
	// broken record.
	ErrUnknownSkill = errors.New("skilltool: no such skill")
	// ErrUnreadable reports a body that is empty or past [MaxBodyRunes].
	ErrUnreadable = errors.New("skilltool: skill body unreadable")
	// ErrNoResource reports a path outside the resources the pack loader recorded,
	// whether or not the file exists.
	ErrNoResource = errors.New("skilltool: no such skill resource")
)

// Set is the skill toolset for one run and that run's record of the skills it read.
// [Set.Reads] returns the record for grading, so the Set must outlive the tool calls.
type Set struct {
	skills state.SkillStore
	packs  fs.FS
	root   string
	notes  Notes

	mu    sync.Mutex
	reads []string
}

// Notes supplies text to attach to a skill read. The implementer decides what is
// selected and how it is framed; this package knows nothing about memory.
type Notes interface {
	// ForSkill returns text for a successful read of skillID, or "". It returns no
	// error, so a failing note source returns "" and the read still succeeds.
	ForSkill(ctx context.Context, skillID string) string
}

// New builds the toolset over skills, reading resources from the pack in the binary
// unless [WithPack] replaces it.
func New(skills state.SkillStore, opts ...Option) *Set {
	s := &Set{skills: skills, packs: bundled.FS(), root: bundled.Root}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Option configures a Set.
type Option func(*Set)

// WithPack reads resources from root in fsys. fs.FS refuses ".." and absolute paths,
// so the set cannot read outside that tree.
func WithPack(fsys fs.FS, root string) Option {
	return func(s *Set) { s.packs, s.root = fsys, root }
}

// WithNotes attaches n's text to every successful skill_read. A nil n adds nothing.
func WithNotes(n Notes) Option {
	return func(s *Set) { s.notes = n }
}

// Tools returns the toolset as [mission.Tool] values. It is empty without a store,
// so a run never offers a tool it cannot answer.
func (s *Set) Tools() []mission.Tool {
	if s == nil || s.skills == nil {
		return nil
	}
	return []mission.Tool{readTool{s}, resourceTool{s}}
}

type readTool struct{ s *Set }

func (readTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "skill_read",
		Description: "Load the full procedure for one of the skills offered to you. The offer gives each skill's name and what it is for; this returns the whole instruction body. Read a skill before acting on it rather than working from its description.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["skill"],
  "properties": {
    "skill": {"type": "string", "description": "The skill's name, exactly as it was offered."}
  },
  "additionalProperties": false
}`),
	}
}

func (t readTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Skill string `json:"skill"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	text, id, err := t.s.render(ctx, in.Skill)
	if err != nil {
		return "", err
	}
	// Recorded after every refusal, so only a body that reached the model counts.
	t.s.recordRead(id)
	return text, nil
}

// Load returns the text skill_read would return for name, for a host that puts the
// skill in front of the model without the model asking. It is not recorded in
// [Set.Reads]: a body the model never asked for says nothing about whether the model
// chose to use it, and Reads is what a run's outcome is credited to.
func (s *Set) Load(ctx context.Context, name string) (string, error) {
	text, _, err := s.render(ctx, name)
	return text, err
}

// render is the body, its resource list and its notes, in that order, so a loaded
// skill and a read one put the same text in front of the model.
func (s *Set) render(ctx context.Context, name string) (text, id string, err error) {
	sk, err := s.get(ctx, name)
	if err != nil {
		return "", "", err
	}
	body := strings.TrimSpace(sk.Body)
	if body == "" {
		return "", "", fmt.Errorf("%w: %s has no body", ErrUnreadable, sk.Slug)
	}
	if n := utf8.RuneCountInString(body); n > MaxBodyRunes {
		return "", "", fmt.Errorf("%w: %s is %d characters, over the %d limit", ErrUnreadable, sk.Slug, n, MaxBodyRunes)
	}
	// Resources are listed, not read, so the model knows which paths it may ask for.
	out := []string{body}
	res, note := s.resources(sk)
	switch {
	case note != "":
		// The body is still returned, with a note that the resource list is missing.
		out = append(out, "("+note+")")
	case len(res) > 0:
		out = append(out, "Resources for this skill, readable with skill_resource:\n- "+strings.Join(res, "\n- "))
	}
	// Notes go last, so the procedure is read before anything said about it.
	if s.notes != nil {
		if n := strings.TrimSpace(s.notes.ForSkill(ctx, sk.ID)); n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, "\n\n"), sk.ID, nil
}

type resourceTool struct{ s *Set }

func (resourceTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "skill_resource",
		Description: "Read one file a skill addresses: a script, a reference document, an asset. Only the paths the skill ships are readable, and skill_read lists them.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["skill", "path"],
  "properties": {
    "skill": {"type": "string", "description": "The skill's name, exactly as it was offered."},
    "path": {"type": "string", "description": "The resource path relative to the skill, as skill_read listed it (for example \"references/api.md\")."}
  },
  "additionalProperties": false
}`),
	}
}

func (t resourceTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Skill string `json:"skill"`
		Path  string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	sk, err := t.s.get(ctx, in.Skill)
	if err != nil {
		return "", err
	}
	res, note := t.s.resources(sk)
	if note != "" {
		return "", fmt.Errorf("%w: %s", ErrNoResource, note)
	}
	// Exact membership in the loader's list is the whole access check, so no path
	// string is interpreted.
	if !slices.Contains(res, in.Path) {
		return "", fmt.Errorf("%w: %s does not address %q", ErrNoResource, sk.Slug, in.Path)
	}
	b, err := fs.ReadFile(t.s.packs, path.Join(t.s.root, sk.Slug, in.Path))
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrNoResource, sk.Slug, err)
	}
	return string(b), nil
}

// recordRead stores ids, because reinforcement credits ids and two scopes can share
// a slug. It only appends under the lock; [Set.Reads] dedupes.
func (s *Set) recordRead(id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, id)
}

// Reads returns the ids of the skills this run read, deduped, in first-read order,
// as learn.Reinforce expects. A run that read nothing returns nothing; the offers do
// not count.
func (s *Set) Reads() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]bool, len(s.reads))
	out := make([]string, 0, len(s.reads))
	for _, id := range s.reads {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// get passes the model's name to the store, which accepts the slug recall offered.
func (s *Set) get(ctx context.Context, name string) (state.Skill, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return state.Skill{}, fmt.Errorf("%w: no name given", ErrUnknownSkill)
	}
	sk, err := s.skills.Get(ctx, name)
	if errors.Is(err, state.ErrNotFound) {
		return state.Skill{}, fmt.Errorf("%w: %q", ErrUnknownSkill, name)
	}
	if err != nil {
		return state.Skill{}, err
	}
	return sk, nil
}

// resources lists a bundled skill's resource paths. Other scopes have none, because
// a stored skill does not record where its tree lives. A bundled skill this binary
// does not ship, as after a downgrade, returns a note instead of an error, because
// the caller cannot act on it and the next seed fixes it.
func (s *Set) resources(sk state.Skill) (paths []string, note string) {
	if sk.Scope != state.BundledScope {
		return nil, ""
	}
	pack, err := skillmd.Load(s.packs, path.Join(s.root, sk.Slug))
	if err != nil {
		return nil, fmt.Sprintf("this binary ships no pack directory for %s, so its resources cannot be listed", sk.Slug)
	}
	return pack.Resources, ""
}
