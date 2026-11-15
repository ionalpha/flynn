package selfupdate

import (
	"fmt"
	"os"

	"github.com/ionalpha/flynn/fault"
)

// replace swaps the staged binary in for the running one. Windows locks a running
// executable against writes but allows a rename, so the outgoing binary is moved aside
// and the incoming one takes its place, in an order that never leaves the path without
// a working flynn:
//
//  1. rename the running binary aside. If this fails, nothing has changed.
//  2. rename the staged binary into place. If this fails, step 1 is undone before
//     this function returns.
//
// The moved-aside file cannot be deleted while it runs, so the next start sweeps it.
func (t installTarget) replace(staged string) error {
	superseded := t.Path + supersededSuffix

	// A leftover from an earlier upgrade would fail the move aside, so it is cleared
	// first. If it cannot be, it is probably still running, and a unique name keeps
	// the last upgrade from blocking this one.
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
		// Restore the original, or a failed upgrade becomes an uninstall.
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
