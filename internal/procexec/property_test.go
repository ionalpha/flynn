package procexec

import (
	"context"
	"os/exec"
	"slices"
	"testing"

	"pgregory.net/rapid"
)

// TestCommandKeepsWhatTheCallerAsked holds the helper to being os/exec.Command with one
// extra creation flag: for any name and arguments the argv is the caller's and the window
// is withheld where Windows would open one.
func TestCommandKeepsWhatTheCallerAsked(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := rapid.StringMatching(`[a-z][a-z0-9_-]{0,12}`).Draw(t, "name")
		args := rapid.SliceOfN(rapid.StringMatching(`[ -~]{0,16}`), 0, 6).Draw(t, "args")
		want := append([]string{name}, args...)

		plain := Command(name, args...)
		bound := CommandContext(context.Background(), name, args...)
		for label, c := range map[string]*exec.Cmd{"Command": plain, "CommandContext": bound} {
			if !slices.Equal(c.Args, want) {
				t.Fatalf("%s: argv = %q, want %q", label, c.Args, want)
			}
			if !windowHidden(c) {
				t.Fatalf("%s: the command would open a console window", label)
			}
		}
	})
}
