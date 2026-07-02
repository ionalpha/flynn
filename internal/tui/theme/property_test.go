package theme_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/tui/theme"
)

var allRoles = []theme.Role{
	theme.UserText, theme.UserPrefix, theme.AssistantText, theme.Code,
	theme.Quote, theme.Link, theme.Emphasis, theme.Strong, theme.Heading,
	theme.ToolName, theme.ToolDetail, theme.ToolOutput, theme.Admitted,
	theme.Rejected, theme.Trust, theme.DiffAdded, theme.DiffRemoved,
	theme.DiffContext, theme.DiffLocation, theme.Status, theme.StatusBusy,
	theme.RecordRecording, theme.RecordSealed, theme.RecordVerified,
	theme.RecordFailed, theme.Success, theme.Warning, theme.Error,
	theme.Muted, theme.Border, theme.Overlay, theme.Selection,
	theme.Placeholder, theme.PasteChip, theme.QueuedChip,
}

// genColor draws a color across every accepted form plus garbage, so the
// properties cover the malformed-input path too.
func genColor(rt *rapid.T) theme.Color {
	switch rapid.IntRange(0, 4).Draw(rt, "form") {
	case 0:
		return ""
	case 1:
		return theme.Color(rapid.SampledFrom([]string{
			"red", "green", "blue", "cyan", "magenta", "yellow",
			"bright-red", "bright-white", "black",
		}).Draw(rt, "named"))
	case 2:
		return theme.Color(rapid.StringMatching(`(0|[1-9][0-9]?|1[0-9][0-9]|2[0-4][0-9]|25[0-5])`).Draw(rt, "indexed"))
	case 3:
		return theme.Color(rapid.StringMatching(`#[0-9a-f]{6}`).Draw(rt, "hex"))
	default:
		return theme.Color(rapid.StringMatching(`[a-z#!?]{0,8}`).Draw(rt, "garbage"))
	}
}

func genStyle(rt *rapid.T) theme.Style {
	return theme.Style{
		Foreground: genColor(rt),
		Background: genColor(rt),
		Bold:       rapid.Bool().Draw(rt, "bold"),
		Faint:      rapid.Bool().Draw(rt, "faint"),
		Italic:     rapid.Bool().Draw(rt, "italic"),
		Underline:  rapid.Bool().Draw(rt, "underline"),
		Reverse:    rapid.Bool().Draw(rt, "reverse"),
	}
}

// wellFormed matches a correctly assembled render: an optional single SGR
// prefix of semicolon-separated decimal parameters, the text, an optional
// reset. Anything else (a stray bracket, a non-numeric parameter from a bad
// color, a missing terminator) fails the match.
var wellFormed = regexp.MustCompile(`^(\x1b\[[0-9]+(;[0-9]+)*m)?(.*?)(\x1b\[0m)?$`)

// TestProp_RenderIsAlwaysWellFormed: for any style, however malformed its
// colors, Render emits either the bare text or one valid SGR prefix plus a
// reset around it. A theme file can therefore never corrupt the byte stream.
func TestProp_RenderIsAlwaysWellFormed(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		th := loadStyle(rt, genStyle(rt))
		text := rapid.StringMatching(`[ -~é日]{0,12}`).Draw(rt, "text")
		got := th.Render(theme.UserText, text)
		m := wellFormed.FindStringSubmatch(got)
		if m == nil {
			rt.Fatalf("malformed render: %q", got)
		}
		if m[3] != text {
			rt.Fatalf("render altered the text: got %q want %q (full %q)", m[3], text, got)
		}
		// The prefix and the reset come together or not at all.
		if (m[1] == "") != (m[4] == "") {
			rt.Fatalf("unbalanced styling: %q", got)
		}
	})
}

// TestProp_ThemeFilesRoundTrip: any style set over any role subset survives
// the JSON encode, Load, and Style read-back unchanged, layered over its
// base for every role it does not name.
func TestProp_ThemeFilesRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 6).Draw(rt, "roles")
		styles := map[theme.Role]theme.Style{}
		for range n {
			styles[rapid.SampledFrom(allRoles).Draw(rt, "role")] = genStyle(rt)
		}
		blob, err := json.Marshal(map[string]any{"name": "prop", "base": "mono", "styles": styles})
		if err != nil {
			rt.Fatalf("marshal: %v", err)
		}
		th, err := theme.Load(strings.NewReader(string(blob)))
		if err != nil {
			rt.Fatalf("load: %v", err)
		}
		base := theme.Mono()
		for _, role := range allRoles {
			want, overridden := styles[role]
			if !overridden {
				want = base.Style(role)
			}
			if got := th.Style(role); got != want {
				rt.Fatalf("role %q = %+v, want %+v", role, got, want)
			}
		}
	})
}

// loadStyle builds a theme carrying the style on UserText via the public
// Load path, so the property exercises the same route a user theme takes.
func loadStyle(rt *rapid.T, st theme.Style) *theme.Theme {
	blob, err := json.Marshal(map[string]any{
		"name": "prop", "base": "mono",
		"styles": map[theme.Role]theme.Style{theme.UserText: st},
	})
	if err != nil {
		rt.Fatalf("marshal: %v", err)
	}
	th, err := theme.Load(strings.NewReader(string(blob)))
	if err != nil {
		rt.Fatalf("load: %v", err)
	}
	return th
}
