package theme_test

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/theme"
)

func TestRenderWrapsWithSGRAndReset(t *testing.T) {
	th := theme.Default()
	got := th.Render(theme.Error, "boom")
	if !strings.HasPrefix(got, "\x1b[") || !strings.HasSuffix(got, "\x1b[0m") {
		t.Fatalf("styled render = %q, want SGR-wrapped", got)
	}
	if !strings.Contains(got, "boom") {
		t.Fatalf("styled render lost the text: %q", got)
	}
}

func TestRenderUnknownRolePassesThrough(t *testing.T) {
	th := theme.Default()
	if got := th.Render(theme.Role("no.such.role"), "plain"); got != "plain" {
		t.Fatalf("unstyled render = %q, want the text untouched", got)
	}
}

func TestRenderEmptyTextAddsNothing(t *testing.T) {
	th := theme.Default()
	if got := th.Render(theme.Error, ""); got != "" {
		t.Fatalf("empty render = %q, want empty", got)
	}
}

func TestColorForms(t *testing.T) {
	cases := []struct {
		color theme.Color
		want  string // a substring the SGR must contain
	}{
		{"red", "31"},
		{"bright-cyan", "96"},
		{"213", "38;5;213"},
		{"#ff5f87", "38;2;255;95;135"},
	}
	for _, tc := range cases {
		st := theme.Style{Foreground: tc.color}
		th := themeWith(t, st)
		got := th.Render(theme.UserText, "x")
		if !strings.Contains(got, tc.want) {
			t.Fatalf("color %q rendered %q, want it to contain %q", tc.color, got, tc.want)
		}
	}
}

// TestInvalidColorOnlyStyleRendersBare pins a regression the property test
// found: a style whose only content is an invalid color is not the zero
// style, but it contributes no attributes, so the render must be the bare
// text, never a dangling reset with no prefix.
func TestInvalidColorOnlyStyleRendersBare(t *testing.T) {
	th := themeWith(t, theme.Style{Foreground: "!"})
	if got := th.Render(theme.UserText, " "); got != " " {
		t.Fatalf("attribute-free style rendered %q, want the bare text", got)
	}
}

func TestInvalidColorMutesRatherThanCorrupts(t *testing.T) {
	th := themeWith(t, theme.Style{Foreground: "#zzzzzz", Bold: true})
	got := th.Render(theme.UserText, "x")
	if strings.Contains(got, "zz") {
		t.Fatalf("invalid color leaked into the escape stream: %q", got)
	}
	if !strings.Contains(got, "1m") && !strings.Contains(got, "[1;") {
		t.Fatalf("valid attributes were lost with the bad color: %q", got)
	}
}

// themeWith builds a one-role custom theme through the public Load path.
func themeWith(t *testing.T, st theme.Style) *theme.Theme {
	t.Helper()
	fg := string(st.Foreground)
	bold := "false"
	if st.Bold {
		bold = "true"
	}
	src := `{"name":"t","base":"mono","styles":{"user.text":{"fg":"` + fg + `","bold":` + bold + `}}}`
	th, err := theme.Load(strings.NewReader(src))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return th
}

func TestLoadLayersOverBase(t *testing.T) {
	src := `{"name":"my", "base":"default", "styles":{"error":{"fg":"magenta"}}}`
	th, err := theme.Load(strings.NewReader(src))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if th.Name() != "my" {
		t.Fatalf("name = %q, want my", th.Name())
	}
	if got := th.Style(theme.Error).Foreground; got != "magenta" {
		t.Fatalf("override lost: error fg = %q", got)
	}
	// A role the file does not name keeps the base style.
	if got := th.Style(theme.Success); got.IsZero() {
		t.Fatal("base styles were not inherited")
	}
}

func TestLoadRefusesUnknownRoleAndBase(t *testing.T) {
	if _, err := theme.Load(strings.NewReader(`{"styles":{"errr":{"bold":true}}}`)); err == nil {
		t.Fatal("a misspelled role loaded silently")
	}
	if _, err := theme.Load(strings.NewReader(`{"base":"solarized","styles":{}}`)); err == nil {
		t.Fatal("an unknown base loaded silently")
	}
}

func TestBuiltinLookup(t *testing.T) {
	for _, name := range []string{"default", "high-contrast", "mono"} {
		if theme.Builtin(name) == nil {
			t.Fatalf("built-in %q missing", name)
		}
	}
	if theme.Builtin("nope") != nil {
		t.Fatal("unknown built-in resolved")
	}
	if theme.Builtin("") == nil {
		t.Fatal("empty name should resolve to the default")
	}
}

// TestHighContrastCarriesStateOffColor pins the accessibility invariant: in
// the high-contrast theme, every state-bearing role distinguishes itself with
// a non-color attribute, never hue alone.
func TestHighContrastCarriesStateOffColor(t *testing.T) {
	th := theme.HighContrast()
	for _, role := range []theme.Role{
		theme.Rejected, theme.RecordFailed, theme.Error,
		theme.DiffAdded, theme.DiffRemoved, theme.Warning, theme.Success,
	} {
		st := th.Style(role)
		if !st.Bold && !st.Underline && !st.Reverse && !st.Italic {
			t.Fatalf("high-contrast role %q relies on color alone: %+v", role, st)
		}
	}
}
