package main

import (
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/projectctx"
	"github.com/ionalpha/flynn/internal/tui/screen"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/session"
)

// TestTranscriptBlocksReflow renders the same blocks through random sequences of
// window widths, as resizes would: no row is ever wider than the window, and a
// word that fits beside the gutter is never split.
func TestTranscriptBlocksReflow(t *testing.T) {
	th := theme.Default()
	rapid.Check(t, func(rt *rapid.T) {
		words := rapid.SliceOfN(rapid.StringMatching(`[a-z]{1,8}(-[a-z]{1,8}){0,2}`), 1, 40).Draw(rt, "words")
		src := strings.Join(words, " ")
		v := newTranscriptView(th)
		blocks := map[string]screen.Component{
			"answer": v.block(session.Event{Kind: session.KindAssistant, Text: "ok " + src}),
			"notice": textBlock(th, blockNotice, src),
			"prompt": textBlock(th, blockUser, src),
			"report": lineBlock(th, theme.Muted, src),
		}
		for _, width := range rapid.SliceOfN(rapid.IntRange(12, 160), 1, 10).Draw(rt, "widths") {
			for name, b := range blocks {
				rows := b.Render(width)
				var got []string
				for _, row := range rows {
					if screen.Width(row) > width {
						rt.Fatalf("%s at %d: row too wide: %q", name, width, row)
					}
					got = append(got, strings.Fields(sgr.ReplaceAllString(row, ""))...)
				}
				for _, w := range words {
					if len(w) <= width-contentGutter && !slices.Contains(got, w) {
						rt.Fatalf("%s at %d split %q: %q", name, width, w, rows)
					}
				}
			}
		}
	})
}

// Prose uses every column right of the gutter.
func TestProseReachesTheLastColumn(t *testing.T) {
	b := newTranscriptView(theme.Default()).block(session.Event{Kind: session.KindAssistant, Text: strings.Repeat("abc ", 40)})
	// Gutter 2, then five words and four spaces: 19 cells, 21 in all.
	if got := screen.Width(b.Render(21)[0]); got != 21 {
		t.Errorf("first row is %d cells in a 21 column window", got)
	}
}

func TestBannerWrapsInANarrowWindow(t *testing.T) {
	b := bannerBlock(theme.Default(), "provider/a-model-with-a-long-name", "/srv/work", projectctx.Decision{})
	for _, width := range []int{12, 30, 200} {
		for _, row := range b.Render(width) {
			if screen.Width(row) > width {
				t.Errorf("banner row too wide at %d: %q", width, row)
			}
		}
	}
}
