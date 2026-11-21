// Package selection decides which skills a run is offered and which core tools it is
// granted, so an operator can run Flynn as a plain loop or narrow it to a named set.
//
// Selection is a view over the stores, never a change to them. A denied skill stays on
// disk and is offered again the next time it is permitted; the filter in [Skills] sits
// between the store and both consumers (recall's offer and the skill_read tool), so a skill
// that is not offered cannot be loaded by guessing its name either.
package selection

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// None is the word that sets an allow list to the empty list in a flag or a file.
const None = "none"

// FileName is the selection file in a data directory.
const FileName = "selection.json"

// Config is one invocation's selection. The zero value offers every skill and grants
// every tool.
//
// Precedence, from the weakest statement to the strongest:
//
//  1. Bare is the baseline: no skill offered, no repository instruction file, no memory
//     recalled into the prompt. It changes nothing about tools, which stay the core set.
//  2. An allow list names the skills offered, and replaces the baseline's "none" or the
//     default "all". Naming a skill is the operator asking for it.
//  3. A deny list removes skills from whatever the first two offer. Deny beats allow
//     and beats bare, so a name on both lists is not offered.
//  4. A tool list narrows the grant. It is independent of the skill rules.
type Config struct {
	// Bare turns off what a run is otherwise given unasked: skills, repository
	// instruction files and recalled memory.
	Bare bool
	// BareSet records that Bare was stated, so a flag can switch off a file's bare.
	BareSet bool

	// AllowSet is true when an allow list was given, including the empty one.
	AllowSet bool
	Allow    []string
	// Deny lists skills that are never offered or readable.
	Deny []string

	// ToolsSet is true when a tool list was given, including the empty one.
	ToolsSet bool
	Tools    []string
}

// Active reports whether any rule differs from the default, which is when a run says so.
func (c Config) Active() bool {
	return c.Bare || c.AllowSet || len(c.Deny) > 0 || c.ToolsSet
}

// PermitsSkill reports whether slug may be offered and read.
func (c Config) PermitsSkill(slug string) bool {
	if slices.Contains(c.Deny, slug) {
		return false
	}
	if c.AllowSet {
		return slices.Contains(c.Allow, slug)
	}
	return !c.Bare
}

// OffersSkills reports whether any skill can be permitted at all, which is when a run
// needs the skill tools. A deny list alone never empties the offer, because a skill it
// does not name still passes.
func (c Config) OffersSkills() bool {
	if c.AllowSet {
		for _, s := range c.Allow {
			if !slices.Contains(c.Deny, s) {
				return true
			}
		}
		return false
	}
	return !c.Bare
}

// ParseList splits a flag or file value into names. The word [None], alone, is the empty
// list. A blank value is an error, because it is far more likely a shell variable that
// expanded to nothing than a request for no skills.
func ParseList(flagName, v string) ([]string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, fmt.Errorf("%s needs a comma-separated list of names, or %q for none", flagName, None)
	}
	if v == None {
		return []string{}, nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("%s has an empty name in %q", flagName, v)
		}
		if p == None {
			return nil, fmt.Errorf("%s: %q means no names and cannot be mixed with others", flagName, None)
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// Over returns c laid over base: a field c states replaces base's, and the deny lists
// are joined, because a skill either side forbids stays forbidden.
func (c Config) Over(base Config) Config {
	out := base
	if c.BareSet {
		out.Bare, out.BareSet = c.Bare, true
	}
	if c.AllowSet {
		out.Allow, out.AllowSet = c.Allow, true
	}
	if c.ToolsSet {
		out.Tools, out.ToolsSet = c.Tools, true
	}
	out.Deny = joinNames(base.Deny, c.Deny)
	return out
}

func joinNames(a, b []string) []string {
	var out []string
	for _, s := range slices.Concat(a, b) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// CheckSkills refuses a slug on the allow or deny list that no stored skill has, naming
// the slugs that exist. A misspelt deny would otherwise leave the skill offered while the
// operator believes it is gone.
func (c Config) CheckSkills(known []string) error {
	for _, list := range []struct {
		flag  string
		names []string
	}{{"--skills", c.Allow}, {"--no-skill", c.Deny}} {
		for _, n := range list.names {
			if !slices.Contains(known, n) {
				return fmt.Errorf("%s: no skill %q; skills available: %s", list.flag, n, nameList(known))
			}
		}
	}
	return nil
}

// CheckTools refuses a tool name outside valid, naming the valid ones.
func (c Config) CheckTools(valid []string) error {
	for _, n := range c.Tools {
		if !slices.Contains(valid, n) {
			return fmt.Errorf("--tools: no tool %q; tools available: %s", n, nameList(valid))
		}
	}
	return nil
}

func nameList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	s := slices.Clone(names)
	sort.Strings(s)
	return strings.Join(s, ", ")
}

// file is the on-disk form. A field that is absent leaves the default; an empty list is
// stated and means none.
type file struct {
	Bare     *bool    `json:"bare"`
	Skills   []string `json:"skills"`
	NoSkills []string `json:"no_skills"`
	Tools    []string `json:"tools"`
}

// Load reads the selection file in dir. A missing file is the zero [Config]. A file that
// does not parse is an error, because an ignored deny list would offer what the operator
// had taken away, and an unknown key is most likely a misspelt one.
func Load(dir string) (Config, error) {
	path := filepath.Join(dir, FileName)
	b, err := os.ReadFile(path) //nolint:gosec // G304: fixed filename under the operator-chosen data dir
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	return parseFile(path, b)
}

func parseFile(path string, b []byte) (Config, error) {
	var f file
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	var c Config
	if f.Bare != nil {
		c.Bare, c.BareSet = *f.Bare, true
	}
	// encoding/json leaves an absent array nil and decodes [] to an empty non-nil slice.
	if f.Skills != nil {
		c.Allow, c.AllowSet = f.Skills, true
	}
	if f.Tools != nil {
		c.Tools, c.ToolsSet = f.Tools, true
	}
	c.Deny = f.NoSkills
	return c, nil
}

// Skills returns a view of inner that shows only the skills c permits. Writes pass
// through, so learning is unaffected; reads and searches never return a denied skill,
// including a read by id, which the store accepts in place of a slug.
func Skills(inner state.SkillStore, c Config) state.SkillStore {
	if !c.Active() {
		return inner
	}
	return filtered{inner: inner, c: c}
}

type filtered struct {
	inner state.SkillStore
	c     Config
}

func (f filtered) Upsert(ctx context.Context, s state.Skill) (state.Skill, error) {
	return f.inner.Upsert(ctx, s)
}

func (f filtered) Delete(ctx context.Context, idOrSlug string) error {
	return f.inner.Delete(ctx, idOrSlug)
}

// Get checks the slug of what the store returned, not the name asked for: an id reaches
// the same skill as its slug does.
func (f filtered) Get(ctx context.Context, idOrSlug string) (state.Skill, error) {
	s, err := f.inner.Get(ctx, idOrSlug)
	if err != nil {
		return state.Skill{}, err
	}
	if !f.c.PermitsSkill(s.Slug) {
		return state.Skill{}, state.ErrNotFound
	}
	return s, nil
}

func (f filtered) List(ctx context.Context, scope state.Scope) ([]state.Skill, error) {
	all, err := f.inner.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	return f.keep(all, 0), nil
}

// Search asks the store for every match and applies limit after the filter, so a denied
// skill does not use up one of the slots a permitted one would have filled.
func (f filtered) Search(ctx context.Context, query string, limit int) ([]state.Skill, error) {
	all, err := f.inner.Search(ctx, query, 0)
	if err != nil {
		return nil, err
	}
	return f.keep(all, limit), nil
}

func (f filtered) keep(in []state.Skill, limit int) []state.Skill {
	out := make([]state.Skill, 0, len(in))
	for _, s := range in {
		if limit > 0 && len(out) >= limit {
			break
		}
		if f.c.PermitsSkill(s.Slug) {
			out = append(out, s)
		}
	}
	return out
}

// EventType is the type of the event [Append] writes. The session package projects it
// into its own vocabulary, and a test pins the two names equal.
const EventType = "run.selection"

// Summary is what a run was told about its selection, as the payload keeps it.
type Summary struct {
	// Offered is how many stored skills the run could be offered.
	Offered int
	// Tools is the granted core tool names, or nil when the grant is not narrowed.
	Tools []string
}

// Append records the selection on a run's stream. The payload holds only strings and
// integers, as the chain's canonical encoding requires.
func Append(ctx context.Context, log spine.Log, stream string, c Config, sum Summary) error {
	mode := "custom"
	if c.Bare {
		mode = "bare"
	}
	tools := "all"
	if c.ToolsSet {
		tools = nameList(sum.Tools)
		if len(sum.Tools) == 0 {
			tools = None
		}
	}
	_, err := log.Append(ctx, spine.AppendInput{
		Stream: stream,
		Type:   EventType,
		Actor:  spine.ActorHuman,
		Payload: map[string]any{
			"mode":    mode,
			"offered": int64(sum.Offered),
			"allowed": allowText(c),
			"denied":  nameList(c.Deny),
			"tools":   tools,
		},
	})
	return err
}

func allowText(c Config) string {
	switch {
	case c.AllowSet:
		return nameList(c.Allow)
	case c.Bare:
		return None
	}
	return "all"
}
