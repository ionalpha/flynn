package request

import (
	"net/http"
	"testing"
	"time"

	"github.com/ionalpha/flynn/fault"
)

// FuzzParseRetryAfter throws arbitrary header strings and clock offsets at the
// Retry-After parser, which reads a value an upstream server (possibly hostile)
// controls. The invariants: it never panics, and any duration it accepts is
// non-negative and bounded by the cap, so a malformed or absurd value can never
// park the agent indefinitely.
func FuzzParseRetryAfter(f *testing.F) {
	for _, s := range []string{
		"", "0", "120", "-5", "  30 ", "not-a-number", "999999999999999999999",
		"Wed, 21 Oct 2099 07:28:00 GMT", "Mon, 02 Jan 2006 15:04:05 MST", "\x00\n", "12.5",
	} {
		f.Add(s, int64(0))
	}

	f.Fuzz(func(t *testing.T, h string, nowUnix int64) {
		now := time.Unix(nowUnix%1_000_000_000, 0).UTC()
		d, ok := parseRetryAfter(h, now)
		if !ok {
			return
		}
		if d < 0 {
			t.Fatalf("parseRetryAfter(%q) = %v, negative duration", h, d)
		}
		if d > maxRetryAfter {
			t.Fatalf("parseRetryAfter(%q) = %v, exceeds cap %v", h, d, maxRetryAfter)
		}
	})
}

// FuzzStatusFault throws arbitrary status codes at the classifier and asserts the
// invariants hold for every integer: it never panics, a fault is produced exactly
// for non-success codes, and only the documented codes are marked retryable.
func FuzzStatusFault(f *testing.F) {
	for _, s := range []int{-1, 0, 100, 200, 204, 301, 399, 400, 401, 404, 408, 418, 429, 500, 503, 599, 700} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, status int) {
		fe := StatusFault(status)
		cls, retryable := classify(status)

		if (fe == nil) != (status < 400) {
			t.Fatalf("status %d: StatusFault nil=%v but success=%v", status, fe == nil, status < 400)
		}
		if fe != nil && fe.Class != cls {
			t.Fatalf("status %d: StatusFault class %q != classify %q", status, fe.Class, cls)
		}
		wantRetryable := status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
		if retryable != wantRetryable {
			t.Fatalf("status %d: retryable=%v, want %v", status, retryable, wantRetryable)
		}
		if retryable && cls != fault.Transient {
			t.Fatalf("status %d retryable but class %q != transient", status, cls)
		}
	})
}
