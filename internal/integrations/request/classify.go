package request

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ionalpha/flynn/fault"
)

// classify maps an HTTP status to a fault class and retryability. Below 400 carries
// no fault. 408, 429 and every 5xx are Transient and retryable; every other 4xx is
// Terminal. Callers branch on the class, never the raw code.
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

// StatusFault returns the classified fault for a status code, or nil below 400. The
// code is "http_<status>" so it is stable across processes and in the event log. An
// API surface uses it to classify a response it read with the transport's rule.
func StatusFault(status int) *fault.Error {
	cls, _ := classify(status)
	if cls == "" {
		return nil
	}
	return fault.New(cls, fmt.Sprintf("http_%d", status), http.StatusText(status))
}

// parseRetryAfter reads a Retry-After header relative to now, as delta seconds or an
// HTTP date. It reports false for an empty, malformed or past header, so the caller
// uses its own backoff. The delay is capped so a hostile value cannot park the agent.
func parseRetryAfter(h string, now time.Time) (time.Duration, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			return 0, false
		}
		// Clamp in seconds before converting, because a large delta would overflow
		// the nanosecond Duration and wrap negative.
		if secs >= int(maxRetryAfter/time.Second) {
			return maxRetryAfter, true
		}
		return capRetryAfter(time.Duration(secs) * time.Second), true
	}
	if when, err := http.ParseTime(h); err == nil {
		if !when.After(now) {
			return 0, false
		}
		// Bound before subtracting, because a far-future date (ParseTime accepts year
		// 9999) would overflow Duration.
		if when.After(now.Add(maxRetryAfter)) {
			return maxRetryAfter, true
		}
		return when.Sub(now), true
	}
	return 0, false
}

// maxRetryAfter bounds how long a server's Retry-After may hold the agent, so an
// adversarial value is a finite wait.
const maxRetryAfter = 5 * time.Minute

func capRetryAfter(d time.Duration) time.Duration {
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}
