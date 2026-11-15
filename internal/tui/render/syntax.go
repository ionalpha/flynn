package render

import (
	"embed"
	"io/fs"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"

	"github.com/ionalpha/flynn/internal/tui/theme"
)

const lexerDir = "lexers"

// lexerFS holds the lexer definitions, produced from chroma's own by
// TestEmbeddedLexersMatchChroma (run with -update after a chroma upgrade).
// Chroma's lexers package parses all 279 definitions during init, which every
// linked binary would pay (5.2ms and 2.7MB for `flynn --version`), so parsing
// waits for the first code block and only the languages a session meets are
// carried.
//
//go:embed lexers/*.xml
var lexerFS embed.FS

// lexerRegistry builds the registry on first use, parsing only each config
// header; rules compile on the first Tokenise. A definition that fails to parse
// is skipped, so a corrupt file costs one language its colors instead of taking
// down the renderer mid-stream. TestEveryEmbeddedLexerRegisters keeps a shipped
// build off that path.
var lexerRegistry = sync.OnceValue(func() *chroma.LexerRegistry {
	reg := chroma.NewLexerRegistry()
	entries, err := fs.ReadDir(lexerFS, lexerDir)
	if err != nil {
		return reg
	}
	for _, e := range entries {
		lex, err := chroma.NewXMLLexer(lexerFS, lexerDir+"/"+e.Name())
		if err != nil {
			continue
		}
		reg.Register(lex)
	}
	return reg
})

// Highlight renders code as rows of at most width cells with the lexer for lang,
// or plain code styling when it is unknown. Code wraps hard because whitespace
// is content.
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

// highlightLines buckets the lexer's token types into the theme's syntax roles,
// one span list per source line. Tokenization failure or an unknown language
// degrades to unstyled code, because a model can emit any string as a fence tag.
func highlightLines(th *theme.Theme, lang, code string) [][]span {
	// Tabs are zero-width and stripped by the control-byte guard, so expand them to
	// keep indentation. Valid UTF-8 keeps widths from shifting after wrap points
	// are chosen (see Markdown.Render).
	code = strings.ToValidUTF8(strings.ReplaceAll(code, "\t", "    "), "�")
	base := th.Style(theme.Code)
	lexer := lexerRegistry().Get(lang)
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
	// Trim the empty lines opened by trailing newlines so both highlighter paths
	// treat trailing blank lines as noise.
	for len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	return lines
}

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

// syntaxStyle buckets a chroma token type into a syntax role; the rest is plain.
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
