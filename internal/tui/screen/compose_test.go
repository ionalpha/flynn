package screen_test

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/screen"
)

func stripSGR(s string) string {
	return strings.ReplaceAll(s, "\x1b[0m", "")
}

func TestOverlaySplicesOverTheBase(t *testing.T) {
	base := []string{"aaaaaaaaaa", "bbbbbbbbbb", "cccccccccc"}
	got := screen.Overlay(base, []string{"XX", "YY"}, 3, 1)
	want := []string{"aaaaaaaaaa", "bbbXXbbbbb", "cccYYccccc"}
	for i := range want {
		if stripSGR(got[i]) != want[i] {
			t.Fatalf("row %d = %q, want %q", i, stripSGR(got[i]), want[i])
		}
	}
}

func TestOverlayExtendsPastTheBase(t *testing.T) {
	base := []string{"aa"}
	got := screen.Overlay(base, []string{"XX", "YY"}, 4, 1)
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3 (overlay extends the frame)", len(got))
	}
	if stripSGR(got[1]) != "    XX" {
		t.Fatalf("row 1 = %q, want blank-padded overlay", stripSGR(got[1]))
	}
	if stripSGR(got[2]) != "    YY" {
		t.Fatalf("row 2 = %q, want blank-padded overlay", stripSGR(got[2]))
	}
}

func TestOverlayIsWidthAwareOverWideGlyphs(t *testing.T) {
	// The base row contains double-width CJK cells; the overlay must land at
	// the requested column measured in cells, not runes or bytes.
	base := []string{"日本語テスト"}
	got := screen.Overlay(base, []string{"XX"}, 4, 0)
	if w := screen.Width(stripSGR(got[0])); w != screen.Width(base[0]) {
		t.Fatalf("composited width = %d, want %d", w, screen.Width(base[0]))
	}
	if !strings.Contains(got[0], "XX") {
		t.Fatalf("overlay content missing from %q", got[0])
	}
}

func TestOverlayEmptyIsIdentity(t *testing.T) {
	base := []string{"one", "two"}
	got := screen.Overlay(base, nil, 2, 1)
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("empty overlay changed the base: %q", got)
	}
}

func TestCenterClampsToOrigin(t *testing.T) {
	x, y := screen.Center(80, 24, 40, 10)
	if x != 20 || y != 7 {
		t.Fatalf("center = (%d,%d), want (20,7)", x, y)
	}
	x, y = screen.Center(10, 5, 40, 10)
	if x != 0 || y != 0 {
		t.Fatalf("oversized box center = (%d,%d), want clamped (0,0)", x, y)
	}
}
