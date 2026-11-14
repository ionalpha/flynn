//go:build unix

package stdioguard

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// redirectStdout keeps the original stdout as a private descriptor and makes descriptor 1
// the write end of a pipe. Order matters. The private copy is taken first, at or above
// descriptor 3 so it cannot land on a standard stream, and with close-on-exec so no child
// inherits it. Only then is descriptor 1 replaced, and the replacement clears close-on-exec
// as dup3 and dup2 always do, so a child that inherits descriptor 1 inherits the pipe.
//
// The pipe is made with the raw syscall and not [os.Pipe], which marks its descriptors
// non-blocking. Non-blocking mode belongs to the open file and not the descriptor, so it
// would follow the write end onto descriptor 1 and make every write to stdout, from this
// process or a child, fail with EAGAIN when the pipe is full.
//
// [os.Stdout] needs no reset on Unix: it is descriptor 1, and descriptor 1 now leads to
// the pipe.
func redirectStdout() (frames, strays *os.File, err error) {
	orig, err := unix.FcntlInt(1, unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrNoStdout, err)
	}
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		_ = unix.Close(orig)
		return nil, nil, fmt.Errorf("stdioguard: create stray pipe: %w", err)
	}
	unix.CloseOnExec(p[0])
	unix.CloseOnExec(p[1])
	if err := dupOnto(p[1], 1); err != nil {
		_ = unix.Close(orig)
		_ = unix.Close(p[0])
		_ = unix.Close(p[1])
		return nil, nil, fmt.Errorf("stdioguard: repoint descriptor 1: %w", err)
	}
	_ = unix.Close(p[1])
	return os.NewFile(uintptr(orig), "stdio-frames"), os.NewFile(uintptr(p[0]), "stdio-strays"), nil
}

// retryEINTR runs f until it stops failing with a signal interruption.
func retryEINTR(f func() error) error {
	for {
		if err := f(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
