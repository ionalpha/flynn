package selfupdate

import (
	"fmt"
	"os"

	"github.com/ionalpha/flynn/fault"
)

// replace swaps the staged binary in for the running one.
//
// Windows will not let a running executable be overwritten, but it will let one be
// renamed: the file is locked against writes, not against a change of name. So the
// outgoing binary is moved aside and the incoming one takes its place. The sequence
// is ordered so that no failure can leave the path without a working flynn on it:
//
//  1. rename the running binary aside. If this fails, nothing has changed.
//  2. rename the staged binary into place. If this fails, step 1 is undone, and the
//     original binary is back at its own path before this function returns.
//
// The moved-aside file cannot be deleted while this process is running it, so it is
// left for the next start to sweep. It is inert: nothing runs it, and nothing reads it.
func (t installTarget) replace(staged string) error {
	superseded := t.Path + supersededSuffix

	// A leftover from an earlier upgrade would make the move-aside fail, so it is
	// cleared first. If it cannot be cleared it is almost certainly still running, and
	// a unique name keeps this upgrade from being blocked by the last one.
	if err := os.Remove(superseded); err != nil && !os.IsNotExist(err) {
		superseded = fmt.Sprintf("%s.%d%s", t.Path, os.Getpid(), supersededSuffix)
		_ = os.Remove(superseded)
	}

	if err := os.Rename(t.Path, superseded); err != nil {
		_ = os.Remove(staged)
		return fault.Wrap(fault.Terminal, CodeInstall,
			fmt.Errorf("moving the running binary aside: %w", err))
	}

	if err := os.Rename(staged, t.Path); err != nil {
		// Put the original back. Leaving the path empty because an upgrade failed would
		// turn a failed upgrade into an uninstall.
		if restore := os.Rename(superseded, t.Path); restore != nil {
			return fault.Wrap(fault.Terminal, CodeInstall, fmt.Errorf(
				"the upgrade failed (%w) and the original binary could not be put back either (%v). "+
					"It is still on disk at %s: rename it to %s to restore it", err, restore, superseded, t.Path))
		}
		_ = os.Remove(staged)
		return fault.Wrap(fault.Terminal, CodeInstall,
			fmt.Errorf("installing the new binary: %w", err))
	}
	return nil
}
