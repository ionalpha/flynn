// Package theme maps semantic roles to terminal styles. Components style text by
// role (what it is), never by color, so swapping the theme restyles the session
// and user theme files and accessibility variants need no component changes.
package theme

import (
	"strconv"
	"strings"
)

// Role names one kind of content the session renders. A new visual element gets
// a role here, never an inline color.
type Role string

// The transcript roles.
const (
	UserText      Role = "user.text"
	UserPrefix    Role = "user.prefix"
	AssistantText Role = "assistant.text"
	Code          Role = "code"
	Quote         Role = "quote"
	Link          Role = "link"
	Emphasis      Role = "emphasis"
	Strong        Role = "strong"
	Heading       Role = "heading"
)

// The tool, governance and diff roles.
const (
	ToolName     Role = "tool.name"
	ToolDetail   Role = "tool.detail"
	ToolOutput   Role = "tool.output"
	Admitted     Role = "governance.admitted"
	Rejected     Role = "governance.rejected"
	Trust        Role = "governance.trust"
	DiffAdded    Role = "diff.added"
	DiffRemoved  Role = "diff.removed"
	DiffContext  Role = "diff.context"
	DiffLocation Role = "diff.location"
)

// The status, record and outcome roles.
const (
	Status          Role = "status"
	StatusBusy      Role = "status.busy"
	RecordRecording Role = "record.recording"
	RecordSealed    Role = "record.sealed"
	RecordVerified  Role = "record.verified"
	RecordFailed    Role = "record.failed"
	Success         Role = "success"
	Warning         Role = "warning"
	Error           Role = "error"
	Muted           Role = "muted"
)

// The syntax roles: token classes in highlighted code. A highlighter buckets its
// fine-grained token types into these; the rest falls back to Code.
const (
	SyntaxKeyword  Role = "syntax.keyword"
	SyntaxString   Role = "syntax.string"
	SyntaxNumber   Role = "syntax.number"
	SyntaxComment  Role = "syntax.comment"
	SyntaxFunction Role = "syntax.function"
	SyntaxType     Role = "syntax.type"
)

// The chrome roles.
const (
	Border      Role = "border"
	Overlay     Role = "overlay"
	Selection   Role = "selection"
	Placeholder Role = "placeholder"
	PasteChip   Role = "paste.chip"
	QueuedChip  Role = "queued.chip"
)

// Color is empty for the terminal default, one of the sixteen base names ("red",
// "bright-blue"), a 256-palette index ("213") or hex ("#ff5f87"). Named and
// indexed colors follow the user's palette; hex is exact where supported.
type Color string

// Style is one role's appearance.
type Style struct {
	Foreground Color `json:"fg,omitempty"`
	Background Color `json:"bg,omitempty"`
	Bold       bool  `json:"bold,omitempty"`
	Faint      bool  `json:"faint,omitempty"`
	Italic     bool  `json:"italic,omitempty"`
	Underline  bool  `json:"underline,omitempty"`
	Reverse    bool  `json:"reverse,omitempty"`
	Strike     bool  `json:"strike,omitempty"`
}

// IsZero reports whether the style changes nothing.
func (s Style) IsZero() bool { return s == Style{} }

// Over layers s onto base: s's colors win where set and attribute flags
// accumulate, so emphasis inside a quote keeps the quote's color and gains
// italics with no escape state.
func (s Style) Over(base Style) Style {
	out := base
	if s.Foreground != "" {
		out.Foreground = s.Foreground
	}
	if s.Background != "" {
		out.Background = s.Background
	}
	out.Bold = out.Bold || s.Bold
	out.Faint = out.Faint || s.Faint
	out.Italic = out.Italic || s.Italic
	out.Underline = out.Underline || s.Underline
	out.Reverse = out.Reverse || s.Reverse
	out.Strike = out.Strike || s.Strike
	return out
}

// Render styles text with s, for callers that merge roles themselves. Like
// Theme.Render, an attribute-free style passes text through untouched, and every
// styled span is self-contained (prefix, text, reset) so lines leak no escape
// state.
func (s Style) Render(text string) string {
	if text == "" {
		return text
	}
	prefix := s.sgr()
	if prefix == "" {
		return text
	}
	return prefix + text + "\x1b[0m"
}

// Theme maps roles to styles. An undefined role renders unstyled, so a sparse
// user theme degrades to plain text.
type Theme struct {
	name   string
	styles map[Role]Style
}

// Name returns the theme name shown by the theme picker.
func (t *Theme) Name() string { return t.name }

// Style returns the style for role r, the zero style when undefined.
func (t *Theme) Style(r Role) Style { return t.styles[r] }

// Render wraps s in the role's SGR attributes and a reset. A style with no
// attributes passes s through untouched: the zero style, and one whose only
// content is an invalid color, which has no prefix and so must have no dangling
// reset.
func (t *Theme) Render(r Role, s string) string {
	if s == "" {
		return s
	}
	prefix := t.styles[r].sgr()
	if prefix == "" {
		return s
	}
	return prefix + s + "\x1b[0m"
}

func (s Style) sgr() string {
	var attrs []string
	if s.Bold {
		attrs = append(attrs, "1")
	}
	if s.Faint {
		attrs = append(attrs, "2")
	}
	if s.Italic {
		attrs = append(attrs, "3")
	}
	if s.Underline {
		attrs = append(attrs, "4")
	}
	if s.Reverse {
		attrs = append(attrs, "7")
	}
	if s.Strike {
		attrs = append(attrs, "9")
	}
	if a, valid := colorAttrs(s.Foreground, false); valid {
		attrs = append(attrs, a...)
	}
	if a, valid := colorAttrs(s.Background, true); valid {
		attrs = append(attrs, a...)
	}
	if len(attrs) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(attrs, ";") + "m"
}

var baseColors = map[string]int{
	"black": 30, "red": 31, "green": 32, "yellow": 33,
	"blue": 34, "magenta": 35, "cyan": 36, "white": 37,
	"bright-black": 90, "bright-red": 91, "bright-green": 92, "bright-yellow": 93,
	"bright-blue": 94, "bright-magenta": 95, "bright-cyan": 96, "bright-white": 97,
}

// colorAttrs translates a Color into SGR parameters. An unparseable color is
// skipped, so a typo in a theme file mutes it instead of corrupting the escape
// stream.
func colorAttrs(c Color, background bool) ([]string, bool) {
	if c == "" {
		return nil, false
	}
	name := strings.ToLower(string(c))
	if code, isNamed := baseColors[name]; isNamed {
		if background {
			code += 10
		}
		return []string{strconv.Itoa(code)}, true
	}
	if strings.HasPrefix(name, "#") && len(name) == 7 {
		r, errR := strconv.ParseUint(name[1:3], 16, 8)
		g, errG := strconv.ParseUint(name[3:5], 16, 8)
		b, errB := strconv.ParseUint(name[5:7], 16, 8)
		if errR != nil || errG != nil || errB != nil {
			return nil, false
		}
		lead := "38"
		if background {
			lead = "48"
		}
		return []string{lead, "2", strconv.FormatUint(r, 10), strconv.FormatUint(g, 10), strconv.FormatUint(b, 10)}, true
	}
	if idx, err := strconv.Atoi(name); err == nil && idx >= 0 && idx <= 255 {
		lead := "38"
		if background {
			lead = "48"
		}
		return []string{lead, "5", strconv.Itoa(idx)}, true
	}
	return nil, false
}
