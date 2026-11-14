//go:build unix

package stdioguard

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRetryEINTRRepeatsOnlyInterruptedCalls(t *testing.T) {
	calls := 0
	err := retryEINTR(func() error {
		calls++
		if calls < 3 {
			return unix.EINTR
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("interrupted twice then ok: err=%v after %d calls, want nil after 3", err, calls)
	}

	calls = 0
	want := unix.EBADF
	err = retryEINTR(func() error { calls++; return want })
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("a real failure: err=%v after %d calls, want EBADF after 1", err, calls)
	}
}
