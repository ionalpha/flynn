// Package learn turns a finished run into skills and memories later runs can reuse.
//
// A [Curator] asks a [Distiller] what a run taught and stores each lesson stamped
// with the run's provenance. Only a converged run is distilled, so every stored
// lesson traces to a run that met its goal and failed work teaches nothing.
//
// With a [Verifier], a skill's check runs in a sandbox first. A check that runs and
// fails drops the skill; a pass tags it verified; no runnable check tags it
// unverified.
//
// The Curator has no model dependency, so gating, provenance and persistence are
// deterministic. [ModelDistiller] is the model-backed Distiller.
package learn

import (
	"context"
	"errors"
	"strings"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/skill/skillmd"
	"github.com/ionalpha/flynn/state"
)

// LessonKind selects the store a lesson goes to.
type LessonKind string

const (
	// LessonSkill is a procedure, keyed by slug so a repeat lesson updates the skill.
	LessonSkill LessonKind = "skill"
	// LessonMemory is a fact, stored as an append-only memory item.
	LessonMemory LessonKind = "memory"
)

// Lesson is one item a Distiller proposes. Title seeds a skill's slug; for a memory
// it is an optional label.
type Lesson struct {
	Kind  LessonKind
	Title string
	Body  string
	Tags  []string
	// Check is an optional shell command run in a sandbox; exit 0 means the skill
	// works. Ignored for memory items.
	Check string
}

// Outcome is the finished run a Curator learns from.
type Outcome struct {
	Objective  string
	Result     string
	Transcript []llm.Message
	Converged  bool
	Scope      state.Scope
	// Source is the session or stream id stamped on every captured item.
	Source string
	// SkillsRead are the ids of skills whose body the run loaded, in load order. A
	// memory lesson is anchored to each, so the next read of that skill surfaces it.
	// Offered skills are excluded because the run may have ignored them; loading is
	// the run's choice. A run that loads five anchors to all five, and the surfacing
	// cap bounds what a reader sees. Skill lessons take no anchors because a skill
	// carries its own evidence (Offers, Reads, Wins).
	SkillsRead []string
}

// Distiller turns a run outcome into zero or more lessons.
type Distiller interface {
	Distill(ctx context.Context, o Outcome) ([]Lesson, error)
}

// Tags on a captured skill, so the curator lifecycle can rank and decay skills by
// evidence.
const (
	// provenanceTag marks a skill learned from a run; a skill without it is authored.
	provenanceTag = "learned"
	verifiedTag   = "verified"
	// unverifiedTag marks a skill whose check was absent or could not run.
	unverifiedTag = "unverified"
)

const memoryKind = "lesson"

// ErrBundledScope reports a Curate call into state.BundledScope, which holds the
// skills shipped in the binary. It is a wiring mistake, so it is an error: capturing
// nothing would look like a run that taught nothing.
var ErrBundledScope = errors.New("learn: bundled scope is reserved for skills shipped in the binary")

// Curator distills a converged run into lessons and stores them.
type Curator struct {
	distiller Distiller
	skills    state.SkillStore
	memories  state.MemoryStore
	verifier  Verifier
}

// Option configures a Curator.
type Option func(*Curator)

// WithVerifier gates skill capture on the skill's check. Without one, skills carry
// no verification tag.
func WithVerifier(v Verifier) Option {
	return func(c *Curator) { c.verifier = v }
}

// NewCurator returns a Curator writing to skills and memories.
func NewCurator(d Distiller, skills state.SkillStore, memories state.MemoryStore, opts ...Option) *Curator {
	c := &Curator{distiller: d, skills: skills, memories: memories}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Captured is what a Curate call stored. Dropped holds skill lessons whose check
// failed. Skipped holds skill lessons whose slug an authored or bundled skill owns,
// so a caller can tell them apart from a run that taught nothing.
type Captured struct {
	Skills   []state.Skill
	Memories []state.MemoryItem
	Dropped  []Lesson
	Skipped  []Lesson
}

// Curate distills o and stores the lessons. An unconverged run yields nothing and a
// lesson with no body is skipped. The first store error is returned with what was
// stored before it.
func (c *Curator) Curate(ctx context.Context, o Outcome) (Captured, error) {
	var captured Captured
	if o.Scope == state.BundledScope {
		return captured, ErrBundledScope
	}
	if !o.Converged {
		return captured, nil
	}
	lessons, err := c.distiller.Distill(ctx, o)
	if err != nil {
		return captured, err
	}
	bundled, err := c.bundledSlugs(ctx)
	if err != nil {
		return captured, err
	}
	for _, l := range lessons {
		if strings.TrimSpace(l.Body) == "" {
			continue
		}
		switch l.Kind {
		case LessonSkill:
			err = c.captureSkill(ctx, o, l, bundled, &captured)
		case LessonMemory:
			err = c.captureMemory(ctx, o, l, &captured)
		}
		if err != nil {
			return captured, err
		}
	}
	return captured, nil
}

// captureSkill stores l or records in captured why it did not.
//
// A skill in the scope without [provenanceTag] is authored or from a pack, and a
// lesson never replaces it. A bundled slug is skipped in every scope because Get
// resolves a slug by earliest created row and bundled skills are seeded first, so
// the learned record would be unreachable. Both are checked before the sandbox runs.
func (c *Curator) captureSkill(ctx context.Context, o Outcome, l Lesson, bundled map[string]bool, captured *Captured) error {
	slug := slugify(l.Title)
	prev, prevErr := c.skills.Get(ctx, slug)
	existsInScope := prevErr == nil && prev.Scope == o.Scope
	if (existsInScope && !isLearned(prev.Tags)) || bundled[slug] {
		captured.Skipped = append(captured.Skipped, l)
		return nil
	}

	tags, keep, err := c.verifiedTags(ctx, l, o.Scope)
	if err != nil {
		return err
	}
	if !keep {
		captured.Dropped = append(captured.Dropped, l)
		return nil
	}

	skill := state.Skill{
		Slug:  slug,
		Name:  strings.TrimSpace(l.Title),
		Body:  l.Body,
		Tags:  tags,
		Check: l.Check,
		Scope: o.Scope,
	}
	// Carry evidence over so re-learning a lesson does not reset reinforcement.
	if existsInScope {
		skill.Offers, skill.Reads, skill.Wins = prev.Offers, prev.Reads, prev.Wins
	}
	sk, err := c.skills.Upsert(ctx, skill)
	if err != nil {
		return err
	}
	captured.Skills = append(captured.Skills, sk)
	return nil
}

// verifiedTags returns the tags to store l with, and false when its check ran and
// failed. An error is a verifier failure, such as a cancelled context, and says
// nothing about the skill.
func (c *Curator) verifiedTags(ctx context.Context, l Lesson, scope state.Scope) (tags []string, keep bool, err error) {
	tags = withProvenance(l.Tags)
	if c.verifier == nil {
		return tags, true, nil
	}
	v, err := c.verifier.Verify(ctx, l, scope)
	switch {
	case err != nil:
		return nil, false, err
	case v.Verified:
		return append(tags, verifiedTag), true, nil
	case v.Ran:
		return nil, false, nil
	default:
		return append(tags, unverifiedTag), true, nil
	}
}

func (c *Curator) captureMemory(ctx context.Context, o Outcome, l Lesson, captured *Captured) error {
	mi, err := c.memories.Write(ctx, state.MemoryItem{
		Kind:    memoryKind,
		Content: l.Body,
		Sources: sourcesOf(o.Source),
		Scope:   o.Scope,
		Anchors: state.SkillAnchors(o.SkillsRead),
	})
	if err != nil {
		return err
	}
	captured.Memories = append(captured.Memories, mi)
	return nil
}

// bundledSlugs lists state.BundledScope once per Curate call. Get would resolve a
// colliding slug across scopes and could return the learned record instead.
func (c *Curator) bundledSlugs(ctx context.Context) (map[string]bool, error) {
	all, err := c.skills.List(ctx, state.BundledScope)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(all))
	for _, sk := range all {
		out[sk.Slug] = true
	}
	return out, nil
}

// sourcesOf returns nil for an empty source so a memory never carries a blank one.
func sourcesOf(source string) []string {
	if source == "" {
		return nil
	}
	return []string{source}
}

// withProvenance adds [provenanceTag] once without mutating tags.
func withProvenance(tags []string) []string {
	for _, t := range tags {
		if t == provenanceTag {
			return tags
		}
	}
	return append(append([]string(nil), tags...), provenanceTag)
}

func isLearned(tags []string) bool {
	for _, t := range tags {
		if t == provenanceTag {
			return true
		}
	}
	return false
}

// slugify lowercases s and collapses each run of other characters to one hyphen.
// An empty result becomes "skill".
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "skill"
	}
	// SKILL.md caps a name at skillmd.MaxNameLen, so bound it where it is minted.
	// The slug is ASCII, so a byte cut is a rune cut.
	if len(out) > skillmd.MaxNameLen {
		out = strings.Trim(out[:skillmd.MaxNameLen], "-")
	}
	return out
}
