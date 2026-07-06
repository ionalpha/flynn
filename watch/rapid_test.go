package watch

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// lineSpec is one generated source line: plain code, or code carrying a marker.
type lineSpec struct {
	isMarker bool
	kind     Kind
	text     string
	rendered string
}

// genLine draws a source line. A plain line is letters and spaces only, so it holds
// no comment leader and no ai! / ai? token; a marker line is such a code prefix
// followed by a comment leader, the token, and an instruction of letters and spaces.
// Keeping both alphabets marker-free except for the injected token means the only
// marker on a generated line is the one the spec records.
func genLine(rt *rapid.T) lineSpec {
	code := rapid.StringMatching(`[a-z]{0,8}( [a-z]{1,6})?`).Draw(rt, "code")
	if !rapid.Bool().Draw(rt, "isMarker") {
		return lineSpec{rendered: code}
	}
	kind := rapid.SampledFrom([]Kind{Act, Ask}).Draw(rt, "kind")
	leader := rapid.SampledFrom(commentLeaders).Draw(rt, "leader")
	text := rapid.StringMatching(`[a-z]{1,8}( [a-z]{1,8}){0,3}`).Draw(rt, "text")
	rendered := strings.TrimSpace(code + " " + leader + " " + string(kind) + " " + text)
	return lineSpec{isMarker: true, kind: kind, text: text, rendered: rendered}
}

// TestScanFindsExactlyMarkedLines is the core property: over a random file, Scan
// returns exactly the marked lines, in order, with the right 1-based line number,
// kind, and instruction, and nothing for the plain lines.
func TestScanFindsExactlyMarkedLines(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		specs := rapid.SliceOfN(rapid.Custom(genLine), 0, 12).Draw(rt, "lines")
		lines := make([]string, len(specs))
		var want []Marker
		for i, s := range specs {
			lines[i] = s.rendered
			if s.isMarker {
				want = append(want, Marker{Kind: s.kind, File: "f", Line: i + 1, Text: s.text})
			}
		}
		got := Scan("f", []byte(strings.Join(lines, "\n")))
		if len(got) != len(want) {
			rt.Fatalf("got %d markers, want %d\nlines: %q\ngot: %+v", len(got), len(want), lines, got)
		}
		for i, w := range want {
			if got[i].Kind != w.Kind || got[i].Line != w.Line || got[i].Text != w.Text {
				rt.Errorf("marker %d = %+v, want kind=%q line=%d text=%q", i, got[i], w.Kind, w.Line, w.Text)
			}
		}
	})
}

// TestClearRemovesOneMarker is the clearing property: clearing a marker's line always
// leaves one fewer marker in the file and never grows the line count, over any random
// file that has at least one marker.
func TestClearRemovesOneMarker(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		specs := rapid.SliceOfN(rapid.Custom(genLine), 1, 12).Draw(rt, "lines")
		lines := make([]string, len(specs))
		var markerLines []int
		for i, s := range specs {
			lines[i] = s.rendered
			if s.isMarker {
				markerLines = append(markerLines, i+1)
			}
		}
		if len(markerLines) == 0 {
			return // nothing to clear on this draw
		}
		content := []byte(strings.Join(lines, "\n"))
		before := len(Scan("f", content))
		pick := rapid.SampledFrom(markerLines).Draw(rt, "clearLine")

		next, changed := Clear(content, pick)
		if !changed {
			rt.Fatalf("Clear(line=%d) reported no change on a marker line\nlines: %q", pick, lines)
		}
		if after := len(Scan("f", next)); after != before-1 {
			rt.Fatalf("after clear got %d markers, want %d\nlines: %q\npick: %d", after, before-1, lines, pick)
		}
		if newLines := strings.Count(string(next), "\n"); newLines > strings.Count(string(content), "\n") {
			rt.Fatalf("clear grew the file's line count")
		}
	})
}
