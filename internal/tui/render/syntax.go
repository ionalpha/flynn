package render

import (
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"

	"github.com/ionalpha/flynn/internal/tui/theme"
)

// Highlight renders source code as themed rows of at most width cells, using
// the lexer registered for lang (or plain code styling when the language is
// unknown). Code wraps hard rather than word-wraps: whitespace is content.
func Highlight(th *theme.Theme, lang, code string, width int) []string {
	if width < 1 {
		return nil
	}
	var out []string
	for _, l := range highlightLines(th, lang, code) {
		out = append(out, hardWrapLine(l, width)...)
	}
	return clampRows(out, width)
}

// highlightLines tokenizes code and buckets the lexer's token types into the
// theme's syntax roles, one span list per source line. Tokenization failure
// (or an unknown language) degrades to unstyled code, never to an error: a
// model can emit any string as a fence info tag.
func highlightLines(th *theme.Theme, lang, code string) [][]span {
	// Tabs are zero-width to the cell accounting and stripped by the
	// control-byte guard, so expand them up front to keep indentation; and
	// normalize to valid UTF-8 so widths cannot shift after wrap points are
	// chosen (see Markdown.Render).
	code = strings.ToValidUTF8(strings.ReplaceAll(code, "\t", "    "), "�")
	base := th.Style(theme.Code)
	lexer := lexers.Get(lang)
	if lexer == nil {
		return plainCodeLines(code, base)
	}
	it, err := lexer.Tokenise(nil, code)
	if err != nil {
		return plainCodeLines(code, base)
	}
	lines := [][]span{nil}
	for tok := it(); tok != chroma.EOF; tok = it() {
		st := syntaxStyle(th, tok.Type).Over(base)
		val := tok.Value
		for {
			nl := strings.IndexByte(val, '\n')
			if nl < 0 {
				break
			}
			if nl > 0 {
				lines[len(lines)-1] = append(lines[len(lines)-1], span{val[:nl], st})
			}
			lines = append(lines, nil)
			val = val[nl+1:]
		}
		if val != "" {
			lines[len(lines)-1] = append(lines[len(lines)-1], span{val, st})
		}
	}
	// Tokenizers keep the source's trailing newlines; trim the empty lines
	// they open so both highlighter paths agree that trailing blank lines in
	// a code block are noise.
	for len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// plainCodeLines is the no-lexer fallback: every line one Code-styled span.
func plainCodeLines(code string, base theme.Style) [][]span {
	split := strings.Split(strings.TrimRight(code, "\n"), "\n")
	out := make([][]span, len(split))
	for i, l := range split {
		if l != "" {
			out[i] = []span{{l, base}}
		}
	}
	return out
}

// syntaxStyle buckets a chroma token type into the theme's small syntax role
// set. Anything unbucketed renders as plain code.
func syntaxStyle(th *theme.Theme, t chroma.TokenType) theme.Style {
	switch {
	case t == chroma.KeywordType || t == chroma.NameClass:
		return th.Style(theme.SyntaxType)
	case t.InCategory(chroma.Keyword):
		return th.Style(theme.SyntaxKeyword)
	case t.InSubCategory(chroma.LiteralString):
		return th.Style(theme.SyntaxString)
	case t.InSubCategory(chroma.LiteralNumber):
		return th.Style(theme.SyntaxNumber)
	case t.InCategory(chroma.Comment):
		return th.Style(theme.SyntaxComment)
	case t == chroma.NameFunction || t == chroma.NameBuiltin:
		return th.Style(theme.SyntaxFunction)
	default:
		return theme.Style{}
	}
}
