package term

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// EditorCommand returns the editor argument vector: VISUAL, then EDITOR (either
// may carry arguments), then a platform default present on a stock system.
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

// RunAttached runs argv on this process's standard streams until it exits: the
// editor handoff. The caller hands the terminal back (cooked mode, modes off,
// input reader parked) before calling and reclaims it after. The child inherits
// the full environment and spawns outside the sandbox, because it is the user's
// own program started at their request, not an agent action.
func RunAttached(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the user's configured editor, run at their explicit request
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
