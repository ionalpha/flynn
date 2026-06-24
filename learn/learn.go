// Package learn is the agent's learning loop: it turns a finished run into durable
// knowledge the agent can reuse, so experience compounds across runs instead of
// being discarded when a conversation ends.
//
// This package is the capture half. A Curator takes the outcome of a run, asks a
// Distiller what reusable lesson it taught, and writes the result to the skill and
// memory stores, stamped with the run's provenance. Capture is outcome-gated: only
// a run that actually converged is distilled, so the agent never crystallizes a
// lesson from work that failed or stalled. That gate, plus the provenance stamp,
// is what keeps a self-curating skill set from degrading into noise: every stored
// lesson is traceable to a run that met its goal.
//
// The Distiller is a port. The model-backed implementation (ModelDistiller) asks a
// language model to summarize the run; a test supplies a scripted one. The Curator
// itself contains no model dependency, so its gating, provenance, and persistence
// are deterministic and fully testable.
package learn

import (
	"context"
	"strings"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/state"
)

// LessonKind selects where a distilled lesson is stored: a reusable Skill (a
// procedure the agent can apply again) or a Memory item (a fact or observation to
// recall).
type LessonKind string

const (
	// LessonSkill is a reusable procedure, stored as a skill keyed by its slug so a
	// later run with the same lesson updates rather than duplicates it.
	LessonSkill LessonKind = "skill"
	// LessonMemory is a fact or observation, stored as an append-only memory item.
	LessonMemory LessonKind = "memory"
)

// Lesson is one piece of durable knowledge a Distiller proposes from a run. Title
// names a skill (and seeds its slug); for a memory item it is an optional label.
// Body is the lesson itself.
type Lesson struct {
	Kind  LessonKind
	Title string
	Body  string
	Tags  []string
}

// Outcome is the finished run a Curator learns from: what it set out to do, what it
// produced, the conversation it took to get there, whether it converged, the scope
// the knowledge belongs to, and a provenance string identifying the run.
type Outcome struct {
	Objective  string
	Result     string
	Transcript []llm.Message
	Converged  bool
	Scope      state.Scope
	// Source identifies the run this knowledge came from (a session or stream id),
	// stamped onto every captured item so a lesson is always traceable to its run.
	Source string
}

// Distiller turns a run outcome into zero or more lessons. Returning none is a
// valid result: not every run teaches something worth keeping.
type Distiller interface {
	Distill(ctx context.Context, o Outcome) ([]Lesson, error)
}

// provenanceTag marks a skill as machine-learned from a run, so the curator
// lifecycle (and a human) can tell captured skills from authored ones.
const provenanceTag = "learned"

// memoryKind is the memory item kind captured lessons are stored under.
const memoryKind = "lesson"

// Curator is the capture half of the learning loop: it distills a converged run
// into lessons and persists them, stamped with provenance. It holds no model
// dependency of its own; the Distiller supplies that.
type Curator struct {
	distiller Distiller
	skills    state.SkillStore
	memories  state.MemoryStore
}

// NewCurator builds a Curator over a distiller and the skill and memory stores it
// writes to.
func NewCurator(d Distiller, skills state.SkillStore, memories state.MemoryStore) *Curator {
	return &Curator{distiller: d, skills: skills, memories: memories}
}

// Captured is what a Curate call persisted, returned for audit and for the caller
// to surface ("learned 2 skills, 1 memory from this run").
type Captured struct {
	Skills   []state.Skill
	Memories []state.MemoryItem
}

// Curate distills o and persists the resulting lessons, returning what it stored.
// It is outcome-gated: a run that did not converge yields nothing, so a failed or
// stalled run never crystallizes a lesson. A lesson missing a body is skipped. The
// first store error aborts and is returned, with whatever was stored before it.
func (c *Curator) Curate(ctx context.Context, o Outcome) (Captured, error) {
	var captured Captured
	if !o.Converged {
		return captured, nil // gate: capture only from runs that met their goal
	}
	lessons, err := c.distiller.Distill(ctx, o)
	if err != nil {
		return captured, err
	}
	for _, l := range lessons {
		if strings.TrimSpace(l.Body) == "" {
			continue
		}
		switch l.Kind {
		case LessonSkill:
			sk, err := c.skills.Upsert(ctx, state.Skill{
				Slug:  slugify(l.Title),
				Name:  strings.TrimSpace(l.Title),
				Body:  l.Body,
				Tags:  withProvenance(l.Tags),
				Scope: o.Scope,
			})
			if err != nil {
				return captured, err
			}
			captured.Skills = append(captured.Skills, sk)
		case LessonMemory:
			mi, err := c.memories.Write(ctx, state.MemoryItem{
				Kind:    memoryKind,
				Content: l.Body,
				Source:  o.Source,
				Scope:   o.Scope,
			})
			if err != nil {
				return captured, err
			}
			captured.Memories = append(captured.Memories, mi)
		}
	}
	return captured, nil
}

// withProvenance ensures the learned-provenance tag is present exactly once,
// preserving the distiller's own tags.
func withProvenance(tags []string) []string {
	for _, t := range tags {
		if t == provenanceTag {
			return tags
		}
	}
	return append(append([]string(nil), tags...), provenanceTag)
}

// slugify renders a title as a stable skill slug: lowercased, with each run of
// non-alphanumeric characters collapsed to a single hyphen and the ends trimmed.
// An empty result falls back to "skill" so a slug is always non-empty.
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
	return out
}
