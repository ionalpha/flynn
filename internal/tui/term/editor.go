package term

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// EditorCommand returns the user's editor as an argument vector: VISUAL,
// then EDITOR (either may carry arguments, split on whitespace), then a
// platform default that is present on a stock system.
func EditorCommand() []string {
	for _, k := range []string{"VISUAL", "EDITOR"} {
		if fields := strings.Fields(os.Getenv(k)); len(fields) > 0 {
			return fields
		}
	}
	if runtime.GOOS == "windows" {
		return []string{"notepad"}
	}
	return []string{"vi"}
}

// RunAttached runs one command attached to this process's own standard
// streams and blocks until it exits: the editor handoff, where the user's
// configured editor takes over the terminal. The caller owns the terminal
// state around the call: hand the terminal back (cooked mode, shell modes
// off, input reader parked) before calling, and reclaim it after. The child
// inherits the full environment, because it is the user's own program on the
// user's own terminal at their explicit request, not an agent action; that
// is also why it may spawn directly instead of through the sandbox.
func RunAttached(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the user's configured editor, run at their explicit request
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
