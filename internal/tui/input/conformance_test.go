package input_test

import (
	"reflect"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/input"
)

// This is the cross-emulator conformance matrix: for each user action, the
// distinct byte sequences real terminals emit for it must all decode to the
// same canonical event. A terminal is only as good as its worst emulator, so
// every encoding a key can arrive in is pinned here, and a regression that
// silently drops one emulator's form fails the build.
//
// The encodings are grouped by the mode or protocol that produces them, not by
// product name, because one product emits several depending on its settings:
//   - legacy: the default xterm CSI/SS3 forms (Windows Terminal and ConPTY VT
//     input, Terminal.app, most emulators out of the box)
//   - app: SS3 application-cursor-key mode (DEC mode 1), which tmux and screen
//     pass through and many shells enable
//   - tilde: the CSI number ~ function-key forms
//   - modifyOtherKeys: xterm's CSI 27 ; mods ; code ~ form for modified keys
//     (xterm level 2, iTerm2)
//   - kitty: the kitty keyboard protocol CSI u form (kitty, ghostty, wezterm,
//     foot, and Windows Terminal with the protocol negotiated)
//   - raw: the bare control byte a modifier folds a key down to
var conformanceMatrix = []struct {
	action    string
	want      input.Event
	encodings map[string]string
}{
	{
		action: "up arrow",
		want:   input.Key{Code: input.KeyUp},
		encodings: map[string]string{
			"legacy": "\x1b[A",
			"app":    "\x1bOA",
		},
	},
	{
		action: "ctrl+right (word forward)",
		want:   input.Key{Code: input.KeyRight, Mods: input.ModCtrl},
		encodings: map[string]string{
			"legacy": "\x1b[1;5C",
			"kitty":  "\x1b[1;5C",
		},
	},
	{
		action: "alt+left (word back)",
		want:   input.Key{Code: input.KeyLeft, Mods: input.ModAlt},
		encodings: map[string]string{
			"legacy": "\x1b[1;3D",
		},
	},
	{
		action: "home",
		want:   input.Key{Code: input.KeyHome},
		encodings: map[string]string{
			"legacy": "\x1b[H",
			"tilde":  "\x1b[1~",
			"app":    "\x1bOH",
		},
	},
	{
		action: "end",
		want:   input.Key{Code: input.KeyEnd},
		encodings: map[string]string{
			"legacy": "\x1b[F",
			"tilde":  "\x1b[4~",
			"app":    "\x1bOF",
		},
	},
	{
		action: "delete",
		want:   input.Key{Code: input.KeyDelete},
		encodings: map[string]string{
			"tilde": "\x1b[3~",
		},
	},
	{
		action: "page up",
		want:   input.Key{Code: input.KeyPageUp},
		encodings: map[string]string{
			"tilde": "\x1b[5~",
		},
	},
	{
		action: "enter",
		want:   input.Key{Code: input.KeyEnter},
		encodings: map[string]string{
			"raw":   "\r",
			"kitty": "\x1b[13u",
		},
	},
	{
		action: "shift+tab",
		want:   input.Key{Code: input.KeyTab, Mods: input.ModShift},
		encodings: map[string]string{
			"legacy": "\x1b[Z",
			"kitty":  "\x1b[9;2u",
		},
	},
	{
		action: "backspace",
		want:   input.Key{Code: input.KeyBackspace},
		encodings: map[string]string{
			"raw":   "\x7f",
			"kitty": "\x1b[127u",
		},
	},
	{
		action: "ctrl+a",
		want:   input.Key{Code: 'a', Mods: input.ModCtrl},
		encodings: map[string]string{
			"raw":             "\x01",
			"kitty":           "\x1b[97;5u",
			"modifyOtherKeys": "\x1b[27;5;97~",
		},
	},
	{
		action: "ctrl+enter",
		want:   input.Key{Code: input.KeyEnter, Mods: input.ModCtrl},
		encodings: map[string]string{
			"kitty":           "\x1b[13;5u",
			"modifyOtherKeys": "\x1b[27;5;13~",
		},
	},
	{
		action: "f1",
		want:   input.Key{Code: input.KeyF1},
		encodings: map[string]string{
			"app":   "\x1bOP",
			"tilde": "\x1b[11~",
		},
	},
	{
		action: "shift+f1",
		want:   input.Key{Code: input.KeyF1, Mods: input.ModShift},
		encodings: map[string]string{
			"legacy": "\x1b[1;2P",
		},
	},
	{
		action: "f5",
		want:   input.Key{Code: input.KeyF5},
		encodings: map[string]string{
			"tilde": "\x1b[15~",
		},
	},
	{
		action: "ctrl+f5",
		want:   input.Key{Code: input.KeyF5, Mods: input.ModCtrl},
		encodings: map[string]string{
			"tilde": "\x1b[15;5~",
		},
	},
	{
		action: "focus in",
		want:   input.Focus{Gained: true},
		encodings: map[string]string{
			"legacy": "\x1b[I",
		},
	},
}

// TestConformanceMatrix decodes every emulator encoding of every action and
// requires it to produce exactly the action's canonical event, whole and in
// one read.
func TestConformanceMatrix(t *testing.T) {
	for _, row := range conformanceMatrix {
		for emu, seq := range row.encodings {
			t.Run(row.action+"/"+emu, func(t *testing.T) {
				got := decode(t, seq)
				want := []input.Event{row.want}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s via %s: decode(%q)\n got %#v\nwant %#v", row.action, emu, seq, got, want)
				}
			})
		}
	}
}

// TestConformanceEncodingsAgree is the property the matrix exists to prove:
// within an action, no two emulator encodings decode to different events, so
// the composer behaves identically no matter which terminal the user runs.
func TestConformanceEncodingsAgree(t *testing.T) {
	for _, row := range conformanceMatrix {
		for emu, seq := range row.encodings {
			got := decode(t, seq)
			if len(got) != 1 || !reflect.DeepEqual(got[0], row.want) {
				t.Errorf("%s via %s decodes to %#v, but the action's canonical event is %#v",
					row.action, emu, got, row.want)
			}
		}
	}
}
