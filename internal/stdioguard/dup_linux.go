//go:build linux

package stdioguard

import "golang.org/x/sys/unix"

// dupOnto makes newfd refer to what oldfd refers to, inheritable across exec. Linux on
// arm64 has no dup2, so dup3 is used.
func dupOnto(oldfd, newfd int) error {
	return retryEINTR(func() error { return unix.Dup3(oldfd, newfd, 0) })
}
