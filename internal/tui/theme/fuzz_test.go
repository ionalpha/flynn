package theme_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/internal/tui/theme"
)

// FuzzThemeLoad throws arbitrary bytes at the theme file parser, the one
// place user-authored content enters the styling layer. Whatever the file
// contains, Load either returns an error or a theme whose every role renders
// well-formed output: a hostile or corrupted theme file can reject, but it
// can never panic the session or corrupt the escape stream.
func FuzzThemeLoad(f *testing.F) {
	f.Add(`{"name":"t","styles":{"error":{"fg":"red","bold":true}}}`)
	f.Add(`{"base":"mono","styles":{}}`)
	f.Add(`{"base":"nope"}`)
	f.Add(`{"styles":{"error":{"fg":"#zzzzzz"}}}`)
	f.Add(`{"styles":{"not.a.role":{}}}`)
	f.Add(`not json at all`)
	f.Add("\x00\xff{}")

	f.Fuzz(func(t *testing.T, blob string) {
		th, err := theme.Load(strings.NewReader(blob))
		if err != nil {
			return
		}
		for _, role := range allRoles {
			got := th.Render(role, "sample")
			m := wellFormed.FindStringSubmatch(got)
			if m == nil {
				t.Fatalf("role %q rendered malformed output %q from theme %q", role, got, blob)
			}
			if m[3] != "sample" {
				t.Fatalf("role %q altered the text: %q", role, got)
			}
		}
	})
}

var errDisk = errors.New("disk failure")

// TestChaosLoadSurfacesReadFailures: a theme file whose read fails mid-way
// (a vanished config volume) surfaces the failure as an error, never a
// partial or default-silently theme.
func TestChaosLoadSurfacesReadFailures(t *testing.T) {
	blob := `{"name":"t","styles":{"error":{"bold":true}}}`
	src := testkit.FaultyReader(bytes.NewReader([]byte(blob)), testkit.FailOnCall(1, errDisk))
	if _, err := theme.Load(src); !errors.Is(err, errDisk) {
		t.Fatalf("Load error = %v, want the read failure surfaced", err)
	}
}
