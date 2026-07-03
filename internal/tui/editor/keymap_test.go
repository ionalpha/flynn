package editor_test

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/input"
)

func TestParseChordNormalizes(t *testing.T) {
	cases := map[string]string{
		"enter":           "enter",
		"Ctrl+A":          "ctrl+a",
		"shift+ctrl+Left": "ctrl+shift+left",
		"ALT+Enter":       "alt+enter",
		"ctrl+_":          "ctrl+_",
		"ctrl++":          "ctrl++",
		"space":           "space",
	}
	for in, want := range cases {
		got, err := editor.ParseChord(in)
		if err != nil {
			t.Fatalf("ParseChord(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseChord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseChordRefusesUnknown(t *testing.T) {
	for _, in := range []string{"ctrl+foo", "hyper+a", "ctrl+ctrl+a", "pgup", ""} {
		if got, err := editor.ParseChord(in); err == nil {
			t.Errorf("ParseChord(%q) = %q, want error", in, got)
		}
	}
}

func TestLoadKeymapOverridesAndUnbinds(t *testing.T) {
	km, err := editor.LoadKeymap(strings.NewReader(
		`{"bindings": {"Ctrl+G": "editor.kill-to-start", "ctrl+u": "none"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var e editor.Editor
	e.SetKeymap(km)
	e.Insert("hello")

	// The new binding fires.
	if got := e.Handle(input.Key{Code: 'g', Mods: input.ModCtrl}); got != editor.ActionRedraw {
		t.Fatalf("ctrl+g = %v, want ActionRedraw", got)
	}
	if e.Content() != "" {
		t.Fatalf("ctrl+g left %q, want empty (kill to start)", e.Content())
	}

	// The unbound default no longer fires; an unclaimed ctrl chord is ActionNone.
	e.Insert("hello")
	if got := e.Handle(input.Key{Code: 'u', Mods: input.ModCtrl}); got != editor.ActionNone {
		t.Fatalf("unbound ctrl+u = %v, want ActionNone", got)
	}
	if e.Content() != "hello" {
		t.Fatalf("unbound ctrl+u changed the buffer to %q", e.Content())
	}

	// Untouched defaults survive the layering.
	if got := e.Handle(input.Key{Code: input.KeyEnter}); got != editor.ActionSubmit {
		t.Fatalf("enter = %v, want ActionSubmit", got)
	}
}

func TestLoadKeymapRefusesBadInput(t *testing.T) {
	cases := map[string]string{
		"unknown command": `{"bindings": {"ctrl+g": "editor.explode"}}`,
		"bad chord":       `{"bindings": {"ctrl+pgdn": "editor.left"}}`,
		"unknown field":   `{"binds": {"ctrl+g": "editor.left"}}`,
		"not json":        `bindings = "toml"`,
	}
	for name, in := range cases {
		if _, err := editor.LoadKeymap(strings.NewReader(in)); err == nil {
			t.Errorf("%s: LoadKeymap accepted %q", name, in)
		}
	}
}

func TestShiftedFunctionalKeyFallsBack(t *testing.T) {
	var e editor.Editor
	e.Insert("ab")
	// shift+left has no binding of its own; it falls back to left.
	if got := e.Handle(input.Key{Code: input.KeyLeft, Mods: input.ModShift}); got != editor.ActionRedraw {
		t.Fatalf("shift+left = %v, want ActionRedraw", got)
	}
	e.Insert("x")
	if e.Content() != "axb" {
		t.Fatalf("content = %q, want axb", e.Content())
	}
}

func TestShiftedRuneStillInserts(t *testing.T) {
	var e editor.Editor
	if got := e.Handle(input.Key{Code: 'A', Mods: input.ModShift}); got != editor.ActionRedraw {
		t.Fatalf("shift+a = %v, want ActionRedraw", got)
	}
	if e.Content() != "A" {
		t.Fatalf("content = %q, want A", e.Content())
	}
}
