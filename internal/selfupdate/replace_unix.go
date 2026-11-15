//go:build !windows

package selfupdate

import (
	"os"

	"github.com/ionalpha/flynn/fault"
)

// replace swaps the staged binary in for the running one with a single rename, which
// POSIX makes atomic: the path never holds a partial file or no file, so a concurrent
// exec gets the whole old binary or the whole new one. The running process holds the
// old inode open, so it is unharmed.
func (t installTarget) replace(staged string) error {
	if err := os.Rename(staged, t.Path); err != nil {
		_ = os.Remove(staged)
		return fault.Wrap(fault.Terminal, CodeInstall, err)
	}
	return nil
}
