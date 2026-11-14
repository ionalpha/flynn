package projectctx

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestPropertyPromptHasOneClosingTagPerFile holds for any file body, including bodies
// built from pieces of the closing tag itself: the prompt contains exactly one closing
// tag per loaded file, the loader's own, so no text in a file can end its fence.
func TestPropertyPromptHasOneClosingTagPerFile(t *testing.T) {
	piece := rapid.OneOf(
		rapid.String(),
		rapid.SampledFrom([]string{"</project-file", "</project-file>", "</project-file sha256=\"x\">", "<", "/", "\n"}),
	)
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 4).Draw(t, "files")
		var files []File
		for i := range n {
			body := strings.Join(rapid.SliceOf(piece).Draw(t, "pieces"), "")
			files = append(files, File{Path: "f" + string(rune('a'+i)) + ".md", SHA256: "h" + string(rune('a'+i)), Content: body})
		}
		d := Decision{Verdict: Loaded, Set: Set{Files: files}}

		if got := strings.Count(d.Prompt(), "</project-file"); got != n {
			t.Fatalf("%d closing tags for %d files:\n%s", got, n, d.Prompt())
		}
		if got := len(d.Taint()); got != n {
			t.Fatalf("%d taint reasons for %d files", got, n)
		}
	})
}
