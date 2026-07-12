//go:build !windows

package selfupdate

import (
	"os"

	"github.com/ionalpha/flynn/fault"
)

// replace swaps the staged binary in for the running one.
//
// On a POSIX system this is a single rename, which the kernel performs atomically:
// there is no instant at which the binary's path holds a partial file or no file at
// all. A concurrent exec either gets the whole old binary or the whole new one. The
// running process is unharmed, because it holds the old inode open and the rename
// only moves the name.
func (t installTarget) replace(staged string) error {
	if err := os.Rename(staged, t.Path); err != nil {
		_ = os.Remove(staged)
		return fault.Wrap(fault.Terminal, CodeInstall, err)
	}
	return nil
}
