//go:build unix && !linux

package stdioguard

import "golang.org/x/sys/unix"

// dupOnto makes newfd refer to what oldfd refers to, inheritable across exec.
func dupOnto(oldfd, newfd int) error {
	return retryEINTR(func() error { return unix.Dup2(oldfd, newfd) })
}
