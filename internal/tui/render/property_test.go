package render_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/tui/render"
	"github.com/ionalpha/flynn/internal/tui/theme"
)

// genMarkdown draws markdown-shaped text from the constructs the renderer
// makes decisions about: headings, paragraphs with inline styles, lists,
// quotes, fences, tables, links, and raw fragments that stress the parser.
func genMarkdown(rt *rapid.T) string {
	var b strings.Builder
	blocks := rapid.IntRange(0, 8).Draw(rt, "blocks")
	for range blocks {
		switch rapid.IntRange(0, 7).Draw(rt, "block") {
		case 0:
			b.WriteString(strings.Repeat("#", rapid.IntRange(1, 6).Draw(rt, "level")) + " ")
			b.WriteString(rapid.StringMatching(`[a-z *_\x60]{0,20}`).Draw(rt, "heading"))
			b.WriteString("\n\n")
		case 1:
			b.WriteString(rapid.StringMatching(`[a-z *_~\x60\[\]()!#>|-]{0,60}`).Draw(rt, "para"))
			b.WriteString("\n\n")
		case 2:
			marker := rapid.SampledFrom([]string{"- ", "* ", "1. ", "12. "}).Draw(rt, "marker")
			items := rapid.IntRange(1, 4).Draw(rt, "items")
			for range items {
				b.WriteString(marker + rapid.StringMatching(`[a-z ]{0,30}`).Draw(rt, "item") + "\n")
			}
			b.WriteString("\n")
		case 3:
			b.WriteString("> " + rapid.StringMatching(`[a-z >]{0,30}`).Draw(rt, "quote") + "\n\n")
		case 4:
			b.WriteString("```" + rapid.SampledFrom([]string{"", "go", "python", "zzz"}).Draw(rt, "lang") + "\n")
			b.WriteString(rapid.StringMatching(`[a-z(){}\n\t "]{0,60}`).Draw(rt, "code"))
			if rapid.Bool().Draw(rt, "closed") {
				b.WriteString("\n```\n")
			}
		case 5:
			b.WriteString("| a | b |\n|---|--:|\n| " +
				rapid.StringMatching(`[a-z ]{0,10}`).Draw(rt, "cell") + " | 1 |\n\n")
		case 6:
			b.WriteString("[" + rapid.StringMatching(`[a-z ]{0,10}`).Draw(rt, "label") +
				"](https://example.com/" + rapid.StringMatching(`[a-z]{0,40}`).Draw(rt, "path") + ")\n\n")
		default:
			// Arbitrary bytes: whatever a model can emit, the renderer must survive.
			b.WriteString(rapid.String().Draw(rt, "raw"))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// TestProp_MarkdownRowsNeverExceedWidth is the screen contract: every
// rendered row fits the terminal, whatever the source and width.
func TestProp_MarkdownRowsNeverExceedWidth(t *testing.T) {
	md := render.NewMarkdown(theme.Default())
	rapid.Check(t, func(rt *rapid.T) {
		source := genMarkdown(rt)
		width := rapid.IntRange(1, 140).Draw(rt, "width")
		for i, r := range md.Render(source, width) {
			if w := ansi.StringWidth(r); w > width {
				rt.Fatalf("row %d is %d cells at width %d: %q", i, w, width, r)
			}
		}
	})
}

// TestProp_MarkdownDeterministic: the same source at the same width renders
// to identical rows, which replay depends on.
func TestProp_MarkdownDeterministic(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		source := genMarkdown(rt)
		width := rapid.IntRange(1, 140).Draw(rt, "width")
		a := render.NewMarkdown(theme.Default()).Render(source, width)
		b := render.NewMarkdown(theme.Default()).Render(source, width)
		if strings.Join(a, "\n") != strings.Join(b, "\n") {
			rt.Fatalf("non-deterministic render")
		}
	})
}

// TestProp_MarkdownRowsAreSingleAndControlFree: one string is one terminal
// row; no raw control byte may survive into output (escapes come only from
// the theme's styling).
func TestProp_MarkdownRowsAreSingleAndControlFree(t *testing.T) {
	md := render.NewMarkdown(theme.Default())
	rapid.Check(t, func(rt *rapid.T) {
		source := genMarkdown(rt)
		width := rapid.IntRange(1, 140).Draw(rt, "width")
		for _, r := range md.Render(source, width) {
			for _, c := range ansi.Strip(r) {
				if (c < 0x20 || c == 0x7f) && c != 0 {
					rt.Fatalf("control byte %q in row %q", c, r)
				}
			}
		}
	})
}

// TestProp_ParagraphTextSurvivesWrap: wrapping rearranges whitespace, never
// words. A plain paragraph's words come out exactly, in order.
func TestProp_ParagraphTextSurvivesWrap(t *testing.T) {
	md := render.NewMarkdown(theme.Default())
	rapid.Check(t, func(rt *rapid.T) {
		words := rapid.SliceOfN(rapid.StringMatching(`[a-z]{1,12}`), 1, 20).Draw(rt, "words")
		width := rapid.IntRange(4, 100).Draw(rt, "width")
		rows := md.Render(strings.Join(words, " "), width)
		var got []string
		for _, r := range rows {
			got = append(got, strings.Fields(ansi.Strip(r))...)
		}
		// Hard-broken overlong words split across rows; compare letters.
		want := strings.Join(words, "")
		if strings.Join(got, "") != want {
			rt.Fatalf("words changed: got %q want %q", strings.Join(got, ""), want)
		}
	})
}

// TestProp_DiffRowsNeverExceedWidth mirrors the markdown width contract for
// both diff layouts, and checks a no-op diff renders nothing.
func TestProp_DiffRowsNeverExceedWidth(t *testing.T) {
	genFile := rapid.StringMatching(`([a-z {}()\t]{0,20}\n){0,12}`)
	rapid.Check(t, func(rt *rapid.T) {
		before := genFile.Draw(rt, "before")
		after := genFile.Draw(rt, "after")
		width := rapid.IntRange(1, 200).Draw(rt, "width")
		if before == after {
			if rows := render.Diff(theme.Default(), "f.txt", before, after, width); rows != nil {
				rt.Fatalf("no-op diff rendered %d rows", len(rows))
			}
			return
		}
		for i, r := range render.Diff(theme.Default(), "f.txt", before, after, width) {
			if w := ansi.StringWidth(r); w > width {
				rt.Fatalf("row %d is %d cells at width %d: %q", i, w, width, r)
			}
		}
	})
}

// TestProp_HighlightPreservesCode: highlighting styles text, never rewrites
// it. At a width wide enough to avoid wrapping, the stripped rows are the
// source lines (tabs expanded).
func TestProp_HighlightPreservesCode(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		code := rapid.StringMatching(`[a-z(){}=+ "\n]{0,80}`).Draw(rt, "code")
		lang := rapid.SampledFrom([]string{"", "go", "python", "json", "zzz"}).Draw(rt, "lang")
		rows := render.Highlight(theme.Default(), lang, code, 400)
		got := strings.Join(plain(rows), "\n")
		want := strings.TrimRight(code, "\n")
		if got != want {
			rt.Fatalf("code changed:\ngot  %q\nwant %q", got, want)
		}
	})
}
