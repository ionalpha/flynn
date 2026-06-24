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
// A skill can also carry an executable check. When a Verifier is supplied, the
// Curator runs that check in a sandbox before crystallizing the skill: a skill
// whose check runs and fails is proven broken and dropped, never stored; one that
// passes is tagged verified; one with no runnable check is kept but tagged
// unverified. A captured procedure is thus trusted in proportion to evidence that
// it actually works, rather than on the model's say-so alone.
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
	// Check is an optional shell command that verifies a skill works: it is run in
	// a sandbox and a zero exit code means the skill is sound. Empty means the skill
	// has no executable check. Ignored for memory items, which are not executable.
	Check string
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

// Tags stamped onto a captured skill to record its provenance and how much its
// soundness was verified, so the curator lifecycle (and a human) can rank and decay
// skills by evidence rather than treating every capture as equally trustworthy.
const (
	// provenanceTag marks a skill as machine-learned from a run, distinct from one
	// a human authored.
	provenanceTag = "learned"
	// verifiedTag marks a skill whose check ran and passed in a sandbox.
	verifiedTag = "verified"
	// unverifiedTag marks a skill kept without passing a check (it had none, or the
	// check could not be run).
	unverifiedTag = "unverified"
)

// memoryKind is the memory item kind captured lessons are stored under.
const memoryKind = "lesson"

// Curator is the capture half of the learning loop: it distills a converged run
// into lessons and persists them, stamped with provenance. It holds no model
// dependency of its own; the Distiller supplies that. With a Verifier set, a
// skill's executable check gates whether it is crystallized.
type Curator struct {
	distiller Distiller
	skills    state.SkillStore
	memories  state.MemoryStore
	verifier  Verifier
}

// Option configures a Curator.
type Option func(*Curator)

// WithVerifier gates skill capture on an executable check: a skill whose check
// runs and fails is dropped, one that passes is tagged verified, and one with no
// runnable check is kept tagged unverified. Without a verifier, skills are kept as
// captured (untagged by verification).
func WithVerifier(v Verifier) Option {
	return func(c *Curator) { c.verifier = v }
}

// NewCurator builds a Curator over a distiller and the skill and memory stores it
// writes to.
func NewCurator(d Distiller, skills state.SkillStore, memories state.MemoryStore, opts ...Option) *Curator {
	c := &Curator{distiller: d, skills: skills, memories: memories}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Captured is what a Curate call persisted, returned for audit and for the caller
// to surface ("learned 2 skills, 1 memory from this run"). Dropped holds skill
// lessons rejected because their check ran and failed, so a caller can report what
// was discarded as broken rather than silently losing it.
type Captured struct {
	Skills   []state.Skill
	Memories []state.MemoryItem
	Dropped  []Lesson
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
			tags := withProvenance(l.Tags)
			if c.verifier != nil {
				v, err := c.verifier.Verify(ctx, l, o.Scope)
				if err != nil {
					return captured, err // verification infrastructure failed (e.g. cancelled)
				}
				switch {
				case v.Verified:
					tags = append(tags, verifiedTag)
				case v.Ran:
					// The check ran and failed: the skill is proven broken, so it is
					// dropped rather than crystallized.
					captured.Dropped = append(captured.Dropped, l)
					continue
				default:
					tags = append(tags, unverifiedTag) // no check, or it could not be run
				}
			}
			skill := state.Skill{
				Slug:  slugify(l.Title),
				Name:  strings.TrimSpace(l.Title),
				Body:  l.Body,
				Tags:  tags,
				Check: l.Check,
				Scope: o.Scope,
			}
			// Re-capturing a skill keeps the outcome evidence it has already earned,
			// so reinforcement is not reset every time the same lesson is learned again.
			if prev, err := c.skills.Get(ctx, skill.Slug); err == nil && prev.Scope == o.Scope {
				skill.Uses, skill.Wins = prev.Uses, prev.Wins
			}
			sk, err := c.skills.Upsert(ctx, skill)
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
