package learn

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
)

// defaultDistillSystem frames the model as a curator extracting reusable knowledge
// from a finished run. It asks for strict JSON so the output parses deterministically,
// and for restraint so the skill set stays signal, not noise.
const defaultDistillSystem = `You distill durable, reusable lessons from a finished agent run.
Return ONLY a JSON array (no prose) of lessons, each: {"kind":"skill"|"memory","title":string,"body":string,"tags":[string]}.
Use "skill" for a reusable procedure worth applying again, "memory" for a fact or observation worth recalling.
Be selective: capture only what would genuinely help a future run, and return [] if nothing is worth keeping.`

// ModelDistiller is a Distiller backed by a language model: it summarizes a run
// into lessons by asking the model for a strict JSON array, then parsing it. It is
// the production distiller; the Curator stays model-free and testable behind the
// Distiller port.
type ModelDistiller struct {
	model     llm.Model
	system    string
	maxTokens int
}

// DistillerOption configures a ModelDistiller.
type DistillerOption func(*ModelDistiller)

// WithSystem overrides the standing instruction framing the distillation.
func WithSystem(s string) DistillerOption {
	return func(d *ModelDistiller) {
		if strings.TrimSpace(s) != "" {
			d.system = s
		}
	}
}

// WithMaxTokens caps the output length requested of the model.
func WithMaxTokens(n int) DistillerOption {
	return func(d *ModelDistiller) {
		if n > 0 {
			d.maxTokens = n
		}
	}
}

// NewModelDistiller builds a model-backed distiller over m.
func NewModelDistiller(m llm.Model, opts ...DistillerOption) *ModelDistiller {
	d := &ModelDistiller{model: m, system: defaultDistillSystem, maxTokens: 1024}
	for _, o := range opts {
		o(d)
	}
	return d
}

var _ Distiller = (*ModelDistiller)(nil)

// Distill asks the model for lessons learned from o and parses its JSON reply. A
// reply with no JSON array is treated as "nothing to capture" (no error); a reply
// whose array is malformed is a terminal fault, so a broken distiller is visible
// rather than silently dropping knowledge.
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

// prompt renders the run into the distillation request: the objective, the result,
// and a compact transcript so the model can see how the goal was reached.
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

// lessonJSON is the wire shape the model returns; it is mapped onto Lesson with a
// safe default kind so an unspecified or unknown kind becomes a memory item rather
// than being dropped.
type lessonJSON struct {
	Kind  string   `json:"kind"`
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Tags  []string `json:"tags"`
}

// parseLessons extracts the JSON array from text and maps it to lessons. Text with
// no array yields no lessons; a present-but-invalid array is an error.
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
		})
	}
	return out, nil
}

// lessonKind maps the wire kind to a LessonKind, defaulting anything other than an
// explicit "skill" to a memory item.
func lessonKind(s string) LessonKind {
	if strings.EqualFold(strings.TrimSpace(s), string(LessonSkill)) {
		return LessonSkill
	}
	return LessonMemory
}

// extractArray returns the outermost JSON array in text (from the first "[" to the
// last "]"), or "" if there is none. Models often wrap JSON in prose or code
// fences despite instructions; this tolerates that without a full parser.
func extractArray(text string) string {
	start := strings.IndexByte(text, '[')
	end := strings.LastIndexByte(text, ']')
	if start < 0 || end <= start {
		return ""
	}
	return text[start : end+1]
}
