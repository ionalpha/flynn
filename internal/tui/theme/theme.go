// Package theme is the semantic styling layer for the terminal session.
// Components never emit colors: they style text by role (what the text IS,
// not how it looks), and a Theme maps each role to a concrete style. Swapping
// the theme restyles the whole session without touching a component, which is
// what makes user theme files and the accessibility variants possible at all.
package theme

import (
	"strconv"
	"strings"
)

// Role names one kind of content the session renders. The set is the UI's
// styling vocabulary: a new visual element gets a role here, never an inline
// color at the call site.
type Role string

// The transcript roles: the conversation's own text and its markdown
// constructs.
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

// The tool and governance roles: action blocks, admission outcomes, and
// diffs.
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

// The status and record roles: the status line, the record badge states,
// and the outcome accents.
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

// The chrome roles: borders, overlays, selection, and the composer's
// affordances.
const (
	Border      Role = "border"
	Overlay     Role = "overlay"
	Selection   Role = "selection"
	Placeholder Role = "placeholder"
	PasteChip   Role = "paste.chip"
	QueuedChip  Role = "queued.chip"
)

// Color names a terminal color: empty for the terminal default, one of the
// sixteen base names ("red", "bright-blue"), a 256-palette index ("213"), or
// a hex value ("#ff5f87"). Named and indexed colors respect the user's
// terminal palette; hex is exact where supported.
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
}

// IsZero reports whether the style changes nothing.
func (s Style) IsZero() bool { return s == Style{} }

// Theme maps roles to styles. A role a theme does not define renders
// unstyled, never as an error: a sparse user theme degrades to plain text,
// not a broken session.
type Theme struct {
	name   string
	styles map[Role]Style
}

// Name identifies the theme (shown by the theme picker).
func (t *Theme) Name() string { return t.name }

// Style returns the style for a role (the zero style when undefined).
func (t *Theme) Style(r Role) Style { return t.styles[r] }

// Render styles s for the role: the role's SGR attributes, the text, then a
// reset. Text whose style contributes no attributes passes through
// untouched. That covers the zero style (an unthemed role costs nothing and
// adds no escape noise) and a style whose only content is an invalid color:
// the color is muted, so there is no prefix, and there must be no dangling
// reset either.
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

// sgr builds the style's escape sequence.
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

// baseColors are the sixteen standard names and their SGR foreground codes.
var baseColors = map[string]int{
	"black": 30, "red": 31, "green": 32, "yellow": 33,
	"blue": 34, "magenta": 35, "cyan": 36, "white": 37,
	"bright-black": 90, "bright-red": 91, "bright-green": 92, "bright-yellow": 93,
	"bright-blue": 94, "bright-magenta": 95, "bright-cyan": 96, "bright-white": 97,
}

// colorAttrs translates a Color into SGR parameters. An unparseable color is
// reported invalid and skipped: a typo in a theme file mutes that color
// rather than corrupting the escape stream.
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
