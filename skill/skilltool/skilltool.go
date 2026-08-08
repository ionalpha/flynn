// Package skilltool exposes the second and third stages of the Agent Skills
// disclosure model as tools the agent calls, rather than as text the runtime
// injects.
//
// The specification describes three stages. The first is metadata: a skill's name
// and description, the only text a runtime loads before anything has decided the
// skill is relevant. The second is the instructions: the whole SKILL.md body, loaded
// once the skill is activated. The third is the resources it addresses, read only
// when the body points at one. Recall carries stage one into the prompt; this
// package is stages two and three.
//
// # Why these are tools and not more injection
//
// A tool call is an event on the spine. Offered-and-ignored, read-and-the-run-failed
// and read-and-the-run-won are three different facts about a skill, and only the
// first is knowable when a skill's text is pasted into a system prompt: nothing
// distinguishes text the model used from text it read past. Grading a skill by what
// it did for a run needs that distinction, so the read has to be an act rather than
// an ambient condition.
//
// # Failure is loud
//
// Every path here either returns the whole thing or returns an error naming what was
// wrong. Nothing truncates. A model working from the first half of a procedure is
// the failure this package exists to remove, and a silently clipped body would
// reintroduce it one layer further in.
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
	"unicode/utf8"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/skill/bundled"
	"github.com/ionalpha/flynn/skill/skillmd"
	"github.com/ionalpha/flynn/state"
)

// MaxBodyRunes caps the body skill_read will hand back. The specification
// recommends an instruction body under 5,000 tokens and a SKILL.md under 500 lines;
// this is roughly twice that, so an authored skill never meets it and a skill that
// does is malformed rather than merely long. Refusing is deliberate: the alternative
// is returning a procedure with its ending cut off, which is worse than saying so.
const MaxBodyRunes = 40_000

var (
	// ErrUnknownSkill reports a name no skill answers to. It is distinct from a skill
	// that exists and cannot be read, because the two call for different repair: one
	// is a wrong name, the other is a broken record.
	ErrUnknownSkill = errors.New("skilltool: no such skill")
	// ErrUnreadable reports a skill whose body cannot be handed over as it stands:
	// empty, or past MaxBodyRunes.
	ErrUnreadable = errors.New("skilltool: skill body unreadable")
	// ErrNoResource reports a path the skill does not address. Only the resources the
	// pack loader recorded are readable, so this covers both a file that is not there
	// and a file that is there and is not part of the skill.
	ErrNoResource = errors.New("skilltool: no such skill resource")
)

// Set is the skill toolset over a skill store and the pack tree whose resources it
// reads. Construct it with New and hand its tools to a mission executor alongside
// the working-tree tools.
type Set struct {
	skills state.SkillStore
	packs  fs.FS
	root   string
}

// New builds the skill toolset over skills, reading resources from the pack in the
// binary. Options replace that pack, which is how a test points at a tree with
// resources in it and how an installed pack on disk will be reached.
func New(skills state.SkillStore, opts ...Option) *Set {
	s := &Set{skills: skills, packs: bundled.FS(), root: bundled.Root}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Option configures a Set.
type Option func(*Set)

// WithPack reads resources from the skill tree rooted at root in fsys instead of the
// pack in the binary. The filesystem is the boundary: fs.FS refuses "..", absolute
// and rooted paths by contract, so a set built over one tree cannot read out of it.
func WithPack(fsys fs.FS, root string) Option {
	return func(s *Set) { s.packs, s.root = fsys, root }
}

// Tools returns the skill toolset as mission.Tools, ready to register with an
// executor. It is empty when there is no store behind it, so a run assembled without
// durable skills offers no tool it cannot answer.
func (s *Set) Tools() []mission.Tool {
	if s == nil || s.skills == nil {
		return nil
	}
	return []mission.Tool{readTool{s}, resourceTool{s}}
}

// --- skill_read ---------------------------------------------------------------

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
	sk, err := t.s.get(ctx, in.Skill)
	if err != nil {
		return "", err
	}
	body := strings.TrimSpace(sk.Body)
	if body == "" {
		return "", fmt.Errorf("%w: %s has no body", ErrUnreadable, sk.Slug)
	}
	if n := utf8.RuneCountInString(body); n > MaxBodyRunes {
		return "", fmt.Errorf("%w: %s is %d characters, over the %d limit", ErrUnreadable, sk.Slug, n, MaxBodyRunes)
	}
	// The resources are listed rather than read. A body that points at a script names
	// it in prose, and the model needs the addressable set to know that path is one it
	// may ask for; the bytes stay where they are until it does.
	res, note := t.s.resources(sk)
	switch {
	case note != "":
		// The procedure is what was asked for, and a pack that will not load is not a
		// reason to withhold it. Said out loud rather than passed over: a bundled skill
		// whose tree this binary cannot read is a real fault, and the model is better
		// off knowing that a list exists and it is not being given one.
		return body + "\n\n(" + note + ")", nil
	case len(res) == 0:
		return body, nil
	}
	return body + "\n\nResources for this skill, readable with skill_resource:\n- " + strings.Join(res, "\n- "), nil
}

// --- skill_resource -----------------------------------------------------------

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
	// Membership in the addressed set is the whole access check, compared exactly. The
	// loader built that set by walking the skill's own directory, so a path it holds
	// cannot leave the skill and a path it does not hold is refused without any
	// reasoning about what the string might mean.
	if !slices.Contains(res, in.Path) {
		return "", fmt.Errorf("%w: %s does not address %q", ErrNoResource, sk.Slug, in.Path)
	}
	b, err := fs.ReadFile(t.s.packs, path.Join(t.s.root, sk.Slug, in.Path))
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrNoResource, sk.Slug, err)
	}
	return string(b), nil
}

// --- shared -------------------------------------------------------------------

// get resolves the name the model used to a live skill. The store takes an id or a
// slug, and the slug is what recall offers, so the model's own vocabulary resolves
// without a lookup table.
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

// resources returns the paths a skill addresses, or none when it addresses nothing
// this process can reach. Only the bundled scope is reachable today: a stored skill
// records its content, not where the tree it came from lives, so a skill learned
// here or imported from a directory has no resources to offer until installed packs
// have a home on disk.
//
// A pack that should be readable and is not comes back as a note rather than an
// error, because there is nothing a caller can do differently about it. It is the
// case where the store holds a bundled skill this binary no longer ships, which a
// downgrade produces and the next seed resolves; the skill's own content is in the
// store either way.
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
