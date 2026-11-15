package learn

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
)

// defaultDistillSystem asks for strict JSON so the reply parses, and for few lessons
// so the skill set stays small.
const defaultDistillSystem = `You distill durable, reusable lessons from a finished agent run.
Return ONLY a JSON array (no prose) of lessons, each: {"kind":"skill"|"memory","title":string,"body":string,"tags":[string],"check":string}.
Use "skill" for a reusable procedure worth applying again, "memory" for a fact or observation worth recalling.
For a skill, "check" is an OPTIONAL shell command that verifies the skill works, exiting 0 on success (e.g. running a build or test). Provide one only when it is safe and self-contained; omit or leave it empty otherwise. Memory items have no check.
Be selective: capture only what would genuinely help a future run, and return [] if nothing is worth keeping.`

// ModelDistiller is the production [Distiller]: it asks a model for a JSON array of
// lessons.
type ModelDistiller struct {
	model     llm.Model
	system    string
	maxTokens int
}

// DistillerOption configures a ModelDistiller.
type DistillerOption func(*ModelDistiller)

// WithSystem replaces the system prompt; a blank s is ignored.
func WithSystem(s string) DistillerOption {
	return func(d *ModelDistiller) {
		if strings.TrimSpace(s) != "" {
			d.system = s
		}
	}
}

// WithMaxTokens caps the reply length; the default is 1024 and n <= 0 is ignored.
func WithMaxTokens(n int) DistillerOption {
	return func(d *ModelDistiller) {
		if n > 0 {
			d.maxTokens = n
		}
	}
}

// NewModelDistiller returns a distiller over m.
func NewModelDistiller(m llm.Model, opts ...DistillerOption) *ModelDistiller {
	d := &ModelDistiller{model: m, system: defaultDistillSystem, maxTokens: 1024}
	for _, o := range opts {
		o(d)
	}
	return d
}

var _ Distiller = (*ModelDistiller)(nil)

// Distill parses the model's reply. A reply with no JSON array yields no lessons; a
// malformed array is a terminal fault, so a broken distiller is visible.
func (d *ModelDistiller) Distill(ctx context.Context, o Outcome) ([]Lesson, error) {
	resp, err := d.model.Generate(ctx, llm.Request{
		System:    d.system,
		Messages:  []llm.Message{llm.Text(llm.RoleUser, d.prompt(o))},
		MaxTokens: d.maxTokens,
	})
	if err != nil {
		return nil, err
	}
	return parseLessons(resp.Message.TextContent())
}

// prompt includes the transcript, with tool calls by name only, so the model sees
// how the goal was reached.
func (d *ModelDistiller) prompt(o Outcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Objective:\n%s\n\nResult:\n%s\n", o.Objective, o.Result)
	if len(o.Transcript) > 0 {
		b.WriteString("\nTranscript:\n")
		for _, m := range o.Transcript {
			if text := strings.TrimSpace(m.TextContent()); text != "" {
				fmt.Fprintf(&b, "%s: %s\n", m.Role, text)
			}
			for _, tu := range m.ToolUses() {
				fmt.Fprintf(&b, "%s: [tool %s]\n", m.Role, tu.Name)
			}
		}
	}
	return b.String()
}

type lessonJSON struct {
	Kind  string   `json:"kind"`
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Tags  []string `json:"tags"`
	Check string   `json:"check"`
}

func parseLessons(text string) ([]Lesson, error) {
	raw := extractArray(text)
	if raw == "" {
		return nil, nil
	}
	var items []lessonJSON
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fault.Wrap(fault.Terminal, "distill_parse", err)
	}
	out := make([]Lesson, 0, len(items))
	for _, it := range items {
		out = append(out, Lesson{
			Kind:  lessonKind(it.Kind),
			Title: it.Title,
			Body:  it.Body,
			Tags:  it.Tags,
			Check: it.Check,
		})
	}
	return out, nil
}

// lessonKind maps any kind but "skill" to [LessonMemory], so an unknown kind is kept
// as a memory.
func lessonKind(s string) LessonKind {
	if strings.EqualFold(strings.TrimSpace(s), string(LessonSkill)) {
		return LessonSkill
	}
	return LessonMemory
}

// extractArray returns text from the first "[" to the last "]", or "", because
// models wrap JSON in prose or code fences.
func extractArray(text string) string {
	start := strings.IndexByte(text, '[')
	end := strings.LastIndexByte(text, ']')
	if start < 0 || end <= start {
		return ""
	}
	return text[start : end+1]
}
