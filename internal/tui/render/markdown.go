package render

import (
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"

	"github.com/ionalpha/flynn/internal/tui/theme"
)

// Markdown renders markdown source to themed terminal lines. It understands
// the GFM constructs the session actually meets in model output: headings,
// paragraphs, fenced and indented code (syntax-highlighted), blockquotes,
// nested ordered and unordered lists, task lists, tables, thematic breaks,
// links, images, emphasis, strikethrough, and inline code. Unknown or raw
// HTML degrades to muted plain text, never to dropped content.
type Markdown struct {
	th     *theme.Theme
	parser parser.Parser
}

// NewMarkdown returns a renderer bound to a theme. The parser is retained
// because goldmark parsers are expensive to build and safe to reuse.
func NewMarkdown(th *theme.Theme) *Markdown {
	return &Markdown{
		th:     th,
		parser: goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser(),
	}
}

// Render parses source and renders it to rows of at most width cells.
func (m *Markdown) Render(source string, width int) []string {
	if width < 1 {
		return nil
	}
	// Normalize to valid UTF-8 first: the cell accounting, the wrap points,
	// and the emitted bytes must all see the same string, and an invalid
	// byte would otherwise change width when the control guard re-encodes
	// it. The terminal cannot render the original byte anyway.
	src := []byte(strings.ToValidUTF8(source, "�"))
	doc := m.parser.Parse(text.NewReader(src))
	return clampRows(m.renderChildren(doc, src, width, theme.Style{}, false), width)
}

// style resolves a role's style layered over the inherited base.
func (m *Markdown) style(r theme.Role, base theme.Style) theme.Style {
	return m.th.Style(r).Over(base)
}

// renderChildren renders a container's block children. Sibling blocks are
// separated by one blank row except in tight mode (inside compact list
// items, where markdown semantics say no paragraph spacing).
func (m *Markdown) renderChildren(n ast.Node, src []byte, width int, base theme.Style, tight bool) []string {
	var out []string
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		lines := m.block(c, src, width, base)
		if len(lines) == 0 {
			continue
		}
		if len(out) > 0 && !tight {
			out = append(out, "")
		}
		out = append(out, lines...)
	}
	return out
}

// block renders one block-level node to rows.
func (m *Markdown) block(n ast.Node, src []byte, width int, base theme.Style) []string {
	switch t := n.(type) {
	case *ast.Heading:
		st := m.style(theme.Heading, base)
		spans := append([]span{{strings.Repeat("#", t.Level) + " ", st}}, m.inline(t, src, st)...)
		return wrapSpans(spans, width)
	case *ast.Paragraph, *ast.TextBlock:
		return wrapSpans(m.inline(n, src, base), width)
	case *ast.FencedCodeBlock:
		return m.codeBlock(string(t.Language(src)), blockText(t, src), width)
	case *ast.CodeBlock:
		return m.codeBlock("", blockText(t, src), width)
	case *ast.Blockquote:
		if width <= 2 {
			return nil
		}
		bar := span{"│ ", m.style(theme.Quote, theme.Style{})}
		inner := m.renderChildren(t, src, width-2, m.style(theme.Quote, base), false)
		return prefixLines(inner, bar, bar)
	case *ast.List:
		return m.list(t, src, width, base)
	case *ast.ThematicBreak:
		return []string{m.style(theme.Border, theme.Style{}).Render(strings.Repeat("─", width))}
	case *east.Table:
		return m.table(t, src, width, base)
	case *ast.HTMLBlock:
		spans := []span{{blockText(t, src), m.style(theme.Muted, base)}}
		return wrapSpans(spans, width)
	default:
		// A block this walker does not know renders its children rather than
		// vanishing; worst case the content appears unstyled.
		return m.renderChildren(n, src, width, base, false)
	}
}

// list renders ordered and unordered lists, including nesting and task
// items. Continuation rows and nested blocks indent past the marker so the
// item's text keeps one left edge.
func (m *Markdown) list(l *ast.List, src []byte, width int, base theme.Style) []string {
	var out []string
	num := l.Start
	for it := l.FirstChild(); it != nil; it = it.NextSibling() {
		marker := "• "
		if l.IsOrdered() {
			marker = strconv.Itoa(num) + ". "
			num++
		}
		indent := textWidth(marker)
		if width <= indent {
			return out
		}
		inner := m.renderChildren(it, src, width-indent, base, l.IsTight)
		first := span{marker, m.style(theme.Muted, theme.Style{})}
		rest := span{strings.Repeat(" ", indent), theme.Style{}}
		out = append(out, prefixLines(inner, first, rest)...)
	}
	return out
}

// codeBlock highlights a code block and indents it two cells. Code wraps
// hard (spaces are content), so nothing is ever cut.
func (m *Markdown) codeBlock(lang, code string, width int) []string {
	if width <= 2 {
		return nil
	}
	lines := highlightLines(m.th, lang, code)
	var out []string
	for _, l := range lines {
		out = append(out, prefixLines(hardWrapLine(l, width-2), span{"  ", theme.Style{}}, span{"  ", theme.Style{}})...)
	}
	return out
}

// inline renders a node's inline children to spans under the inherited
// style.
func (m *Markdown) inline(n ast.Node, src []byte, base theme.Style) []span {
	var out []span
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch t := c.(type) {
		case *ast.Text:
			out = append(out, span{string(t.Segment.Value(src)), base})
			switch {
			case t.HardLineBreak():
				out = append(out, span{"\n", base})
			case t.SoftLineBreak():
				out = append(out, span{" ", base})
			}
		case *ast.String:
			out = append(out, span{string(t.Value), base})
		case *ast.CodeSpan:
			out = append(out, m.inline(t, src, m.style(theme.Code, base))...)
		case *ast.Emphasis:
			role := theme.Emphasis
			if t.Level >= 2 {
				role = theme.Strong
			}
			out = append(out, m.inline(t, src, m.style(role, base))...)
		case *ast.Link:
			out = append(out, m.linkSpans(t, src, base)...)
		case *ast.AutoLink:
			out = append(out, span{string(t.URL(src)), m.style(theme.Link, base)})
		case *ast.Image:
			out = append(out, span{"[image: ", m.style(theme.Muted, base)})
			out = append(out, m.inline(t, src, m.style(theme.Muted, base))...)
			out = append(out, span{"]", m.style(theme.Muted, base)})
			if len(t.Destination) > 0 {
				out = append(out, span{" (" + string(t.Destination) + ")", m.style(theme.Muted, base)})
			}
		case *ast.RawHTML:
			out = append(out, span{segmentsText(t.Segments, src), m.style(theme.Muted, base)})
		case *east.Strikethrough:
			out = append(out, m.inline(t, src, (theme.Style{Strike: true}).Over(base))...)
		case *east.TaskCheckBox:
			box := "[ ] "
			if t.IsChecked {
				box = "[x] "
			}
			out = append(out, span{box, m.style(theme.Muted, base)})
		default:
			out = append(out, m.inline(c, src, base)...)
		}
	}
	return out
}

// linkSpans renders a link as its styled label followed by the destination
// in muted parentheses, unless the label already is the destination.
func (m *Markdown) linkSpans(l *ast.Link, src []byte, base theme.Style) []span {
	label := m.inline(l, src, m.style(theme.Link, base))
	dest := string(l.Destination)
	if dest == "" || dest == plainText(label) {
		return label
	}
	return append(label, span{" (" + dest + ")", m.style(theme.Muted, base)})
}

// plainText flattens spans back to their unstyled text.
func plainText(spans []span) string {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.text)
	}
	return b.String()
}

// blockText reads a block node's raw source lines as one string.
func blockText(n ast.Node, src []byte) string {
	var b strings.Builder
	lines := n.Lines()
	for i := range lines.Len() {
		b.Write(segValue(lines.At(i), src))
	}
	return strings.TrimRight(b.String(), "\n")
}

// segmentsText reads inline raw segments as one string.
func segmentsText(segs *text.Segments, src []byte) string {
	var b strings.Builder
	for i := range segs.Len() {
		b.Write(segValue(segs.At(i), src))
	}
	return b.String()
}

// segValue reads one segment's bytes (text.Segments.At returns a value, and
// Segment.Value has a pointer receiver).
func segValue(seg text.Segment, src []byte) []byte {
	return seg.Value(src)
}
