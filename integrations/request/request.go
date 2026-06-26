// Package request is the shared HTTP transport every integration surface runs on:
// the API surface, the scraper, and the search providers all make their outbound
// calls through one Transport rather than each holding its own *http.Client. That
// keeps a single home for the cross-cutting concerns a remote, unattended agent
// must get right: bounded timeouts, retry with backoff on the right failure
// classes only, per-host rate limiting, and typed error classification through the
// fault model.
//
// Two design choices make the transport testable and deterministic. It runs every
// call against a Doer interface (*http.Client satisfies it), so a test or a chaos
// adapter substitutes the network without touching call sites. And every delay is
// taken through a clock.Timing rather than time.Sleep, so a clock.Manual drives
// backoff and rate-limit waits with no real sleeping and a run reproduces exactly.
package request

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/netguard"
)

// Doer performs a single HTTP round trip. *http.Client satisfies it. The transport
// is defined against this interface, not a concrete client, so a test or a
// fault-injecting adapter can stand in for the network.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Transport executes HTTP requests with bounded retries, exponential backoff, and
// per-host rate limiting. It is safe for concurrent use.
type Transport struct {
	doer        Doer
	clk         clock.Timing
	maxAttempts int
	baseBackoff time.Duration
	maxBackoff  time.Duration
	jitter      *jitter
	limiters    *hostLimiters
}

// Option configures a Transport.
type Option func(*Transport)

// WithDoer sets the underlying HTTP doer. The default dials through netguard
// (anti-SSRF: public addresses only, private/loopback/metadata denied). Tests and
// chaos adapters use this to substitute the network, and a caller that must reach a
// specific private host injects its own netguard.Client with a matching policy.
func WithDoer(d Doer) Option {
	return func(t *Transport) {
		if d != nil {
			t.doer = d
		}
	}
}

// WithClock sets the clock backing backoff and rate-limit waits. The default is
// clock.System; tests pass a clock.Manual for deterministic, sleepless delays.
func WithClock(c clock.Timing) Option {
	return func(t *Transport) {
		if c != nil {
			t.clk = c
		}
	}
}

// WithMaxAttempts caps the total number of tries, including the first. A value
// below 1 is clamped to 1 (no retry).
func WithMaxAttempts(n int) Option {
	return func(t *Transport) {
		if n < 1 {
			n = 1
		}
		t.maxAttempts = n
	}
}

// WithBackoff sets the base and ceiling for exponential backoff between retries.
// A base of 0 disables waiting entirely (retries fire immediately), which keeps
// deterministic tests simple. Negative values are treated as 0.
func WithBackoff(base, ceiling time.Duration) Option {
	return func(t *Transport) {
		if base < 0 {
			base = 0
		}
		if ceiling < 0 {
			ceiling = 0
		}
		t.baseBackoff, t.maxBackoff = base, ceiling
	}
}

// WithJitterEntropy enables full jitter on each backoff wait, drawing from src so
// retries from many goroutines do not align into a thundering herd. Entropy is
// injected (the project reserves direct rand sources to package ids): pass
// crypto/rand from the composition root in production, or a deterministic reader for
// reproducible replay. Without this option backoff stays the exact deterministic
// schedule, so a run replays bit-for-bit by default.
func WithJitterEntropy(src io.Reader) Option {
	return func(t *Transport) {
		if src != nil {
			t.jitter = newJitter(src)
		}
	}
}

// WithRateLimit enforces a minimum interval between requests to the same host,
// expressed as a maximum requests-per-second. A value <= 0 disables rate limiting.
func WithRateLimit(perSecond float64) Option {
	return func(t *Transport) {
		var interval time.Duration
		if perSecond > 0 {
			interval = time.Duration(float64(time.Second) / perSecond)
		}
		t.limiters.interval = interval
	}
}

// New builds a Transport. Defaults: a netguard client (public addresses only,
// anti-SSRF) so a freshly-built transport cannot be steered into dialing a private,
// loopback, or cloud-metadata address; clock.System; three attempts; 200ms base
// backoff capped at 10s; no jitter (deterministic schedule); no rate limit. Enable
// jitter with WithJitterEntropy.
func New(opts ...Option) *Transport {
	t := &Transport{
		doer:        netguard.Client(netguard.PublicOnly()),
		clk:         clock.System{},
		maxAttempts: 3,
		baseBackoff: 200 * time.Millisecond,
		maxBackoff:  10 * time.Second,
		limiters:    &hostLimiters{next: map[string]time.Time{}},
	}
	t.limiters.clk = t.clk
	for _, o := range opts {
		o(t)
	}
	// Options may have replaced the clock; keep the limiter reading the same one.
	t.limiters.clk = t.clk
	return t
}

// Do executes req with rate limiting, retries, and backoff, and returns the final
// response. The returned error is always a classified *fault.Error or nil:
//
//   - A 2xx/3xx response returns the response and a nil error.
//   - A non-retryable status (most 4xx) returns the response and a Terminal fault,
//     so the caller may still read the body to surface the API's error detail.
//   - A retryable failure (network error, 408, 429, 5xx) is retried up to the
//     attempt ceiling; if it never succeeds the last response (if any) and a
//     Transient fault are returned.
//   - A cancelled or expired context returns a Cancelled fault and is never retried.
//
// Retries re-send the body via req.GetBody; a retryable request without GetBody is
// sent once and its failure returned, since the transport cannot safely replay it.
func (t *Transport) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	var lastResp *http.Response
	var lastErr *fault.Error

	for attempt := 1; attempt <= t.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return lastResp, fault.Wrap(fault.Cancelled, "http_cancelled", err)
		}
		// Rate-limit this host before spending the attempt.
		if err := t.sleep(ctx, t.limiters.reserve(host)); err != nil {
			return lastResp, err
		}
		if attempt > 1 {
			body, ok := rewind(req)
			if !ok {
				// Cannot replay a consumed body safely; stop with the prior failure.
				return lastResp, lastErr
			}
			req.Body = body
		}

		resp, err := t.doer.Do(req.WithContext(ctx))
		if err != nil {
			lastResp, lastErr = nil, transportFault(err)
			if lastErr.Class != fault.Transient || attempt == t.maxAttempts {
				return nil, lastErr
			}
			if werr := t.sleep(ctx, t.backoffWithJitter(attempt+1)); werr != nil {
				return nil, werr
			}
			continue
		}

		statusErr := StatusFault(resp.StatusCode)
		if statusErr == nil {
			return resp, nil // success
		}
		_, retryable := classify(resp.StatusCode)
		if !retryable || attempt == t.maxAttempts {
			return resp, statusErr
		}
		// Retryable status: honour Retry-After when present, else back off. Drain and
		// close the body so the connection can be reused on the next attempt.
		wait := t.backoffWithJitter(attempt + 1)
		if ra, ok := parseRetryAfter(resp.Header.Get("Retry-After"), t.clk.Now()); ok {
			wait = ra
		}
		drain(resp)
		lastResp, lastErr = resp, statusErr
		if werr := t.sleep(ctx, wait); werr != nil {
			return nil, werr
		}
	}
	return lastResp, lastErr
}

// Backoff returns the deterministic, un-jittered wait before the given 1-based
// attempt: base * 2^(attempt-2) for attempt >= 2, capped at the ceiling, and zero
// for the first attempt (which has no preceding wait). It is exported so the schedule
// can be asserted directly in tests.
func (t *Transport) Backoff(attempt int) time.Duration {
	if attempt <= 1 || t.baseBackoff <= 0 {
		return 0
	}
	d := t.baseBackoff
	for i := 2; i < attempt; i++ {
		d *= 2
		if d >= t.maxBackoff {
			return t.maxBackoff
		}
	}
	if t.maxBackoff > 0 && d > t.maxBackoff {
		return t.maxBackoff
	}
	return d
}

// backoffWithJitter returns the wait before the given attempt. Without an entropy
// source it is the exact deterministic schedule; with one it applies full jitter, a
// value in [d/2, d], so concurrent retries spread out without ever waiting less than
// half the schedule.
func (t *Transport) backoffWithJitter(attempt int) time.Duration {
	d := t.Backoff(attempt)
	if d <= 0 || t.jitter == nil {
		return d
	}
	return d/2 + t.jitter.upTo(d/2+1)
}

// sleep waits for d through the clock, returning a Cancelled fault if the context
// ends first. A non-positive d returns immediately.
func (t *Transport) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := t.clk.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fault.Wrap(fault.Cancelled, "http_wait_cancelled", ctx.Err())
	case <-timer.C():
		return nil
	}
}

// transportFault classifies a doer error. An error already carrying an explicit
// *fault.Error is respected as-is, so a caller (or a chaos adapter) can mark a
// failure Terminal and have it not be retried. A cancelled/expired context maps to
// Cancelled (never retried). Anything else is treated as a Transient network
// failure worth a retry, since raw transport errors are usually blips.
func transportFault(err error) *fault.Error {
	var fe *fault.Error
	if errors.As(err, &fe) {
		return fe
	}
	if fault.Classify(err) == fault.Cancelled {
		return fault.Wrap(fault.Cancelled, "http_cancelled", err)
	}
	return fault.Wrap(fault.Transient, "http_transport", err)
}

// rewind rebuilds the request body for a retry. It reports false when the body
// cannot be replayed (a non-nil body with no GetBody), so the caller stops rather
// than sending a truncated request.
func rewind(req *http.Request) (io.ReadCloser, bool) {
	if req.Body == nil {
		return nil, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	return body, true
}

// drain reads and closes a response body so its connection returns to the pool
// before the next attempt.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
