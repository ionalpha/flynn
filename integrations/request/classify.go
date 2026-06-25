package request

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ionalpha/flynn/fault"
)

// classify maps an HTTP status to a fault class and whether it is worth retrying.
// Success (below 400) carries no fault. 408 (request timeout) and 429 (too many
// requests) and any 5xx are Transient and retryable; every other 4xx is Terminal
// (bad input or auth, not retryable). The split mirrors fault.Transient vs Terminal
// so callers branch on the class, never on the raw code.
func classify(status int) (cls fault.Class, retryable bool) {
	switch {
	case status < 400:
		return "", false
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests:
		return fault.Transient, true
	case status >= 500:
		return fault.Transient, true
	default:
		return fault.Terminal, false
	}
}

// StatusFault returns the classified fault for a status code, or nil for a success
// (below 400). The code is "http_<status>" so it stays stable across processes and
// in the event log. It is exported so an API surface can classify a response it read
// itself with the same rule the transport uses.
func StatusFault(status int) *fault.Error {
	cls, _ := classify(status)
	if cls == "" {
		return nil
	}
	return fault.New(cls, fmt.Sprintf("http_%d", status), http.StatusText(status))
}

// parseRetryAfter interprets a Retry-After header relative to now. It accepts both
// forms the RFC allows: a non-negative delta in seconds, or an HTTP date, returning
// the delay until that date. It reports false for an empty, malformed, or
// past-dated header, so the caller falls back to its own backoff. A delta is capped
// to a sane ceiling so a hostile or absurd value cannot park the agent for days.
func parseRetryAfter(h string, now time.Time) (time.Duration, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			return 0, false
		}
		// Clamp by seconds before converting: a large delta would overflow the
		// nanosecond-based Duration and wrap negative. Anything past the cap is the
		// cap anyway, so this both bounds the wait and avoids the overflow.
		if secs >= int(maxRetryAfter/time.Second) {
			return maxRetryAfter, true
		}
		return capRetryAfter(time.Duration(secs) * time.Second), true
	}
	if when, err := http.ParseTime(h); err == nil {
		if !when.After(now) {
			return 0, false
		}
		// Bound before subtracting: a far-future date (ParseTime accepts up to year
		// 9999) would overflow Duration. Only subtract within the capped window.
		if when.After(now.Add(maxRetryAfter)) {
			return maxRetryAfter, true
		}
		return when.Sub(now), true
	}
	return 0, false
}

// maxRetryAfter bounds how long a server's Retry-After may hold the agent, so a
// malformed or adversarial value degrades to a long-but-finite wait, never a hang.
const maxRetryAfter = 5 * time.Minute

func capRetryAfter(d time.Duration) time.Duration {
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}
