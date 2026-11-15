// Package request is the shared HTTP transport for every integration surface (the
// API surface, the scraper, the search providers), so an unattended agent has one
// place for bounded timeouts, retry with backoff on the right failure classes,
// per-host rate limiting and fault classification.
//
// Every call runs against a Doer, so a test or chaos adapter substitutes the
// network without touching call sites. Every delay goes through a clock.Timing, so
// a clock.Manual drives backoff and rate-limit waits without sleeping and a run
// reproduces exactly.
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

// Doer performs one HTTP round trip; *http.Client satisfies it, so a test or a
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

// WithDoer sets the HTTP doer. The default dials through netguard, public addresses
// only. Tests and chaos adapters substitute the network with it, and a caller that
// must reach a private host injects a netguard.Client with a matching policy.
func WithDoer(d Doer) Option {
	return func(t *Transport) {
		if d != nil {
			t.doer = d
		}
	}
}

// WithClock sets the clock for backoff and rate-limit waits (default clock.System).
func WithClock(c clock.Timing) Option {
	return func(t *Transport) {
		if c != nil {
			t.clk = c
		}
	}
}

// WithMaxAttempts caps the total tries, the first included. Below 1 is clamped to 1.
func WithMaxAttempts(n int) Option {
	return func(t *Transport) {
		if n < 1 {
			n = 1
		}
		t.maxAttempts = n
	}
}

// WithBackoff sets the base and ceiling for exponential backoff. A base of 0 retries
// without waiting. Negative values count as 0.
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

// WithJitterEntropy enables full jitter on each backoff wait, drawn from src, so
// retries from many goroutines do not align. Entropy is injected because only
// package ids owns a rand source: pass crypto/rand in production or a deterministic
// reader for replay. Without it backoff follows the exact schedule, so a run replays
// bit for bit.
func WithJitterEntropy(src io.Reader) Option {
	return func(t *Transport) {
		if src != nil {
			t.jitter = newJitter(src)
		}
	}
}

// WithRateLimit spaces requests to one host to at most perSecond. A value <= 0
// disables rate limiting.
func WithRateLimit(perSecond float64) Option {
	return func(t *Transport) {
		var interval time.Duration
		if perSecond > 0 {
			interval = time.Duration(float64(time.Second) / perSecond)
		}
		t.limiters.interval = interval
	}
}

// New builds a Transport. Defaults: a public-only netguard client, so a new
// transport cannot dial a private, loopback or cloud-metadata address;
// clock.System; three attempts; 200ms base backoff capped at 10s; no jitter; no rate
// limit.
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

// Do executes req with rate limiting, retries and backoff. The error is nil or a
// classified *fault.Error:
//
//   - A 2xx or 3xx returns the response and nil.
//   - A non-retryable status (most 4xx) returns the response and a Terminal fault,
//     so the caller can read the API's error detail.
//   - A retryable failure (network error, 408, 429, 5xx) is retried up to the
//     attempt cap, then returns the last response, if any, and a Transient fault.
//   - A cancelled or expired context returns a Cancelled fault, never retried.
//
// Retries re-send the body through req.GetBody. A request with a body and no GetBody
// is sent once, because the transport cannot replay it.
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
				// A consumed body cannot be replayed; stop with the prior failure.
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
		// Honour Retry-After when present, else back off. The body is drained so the
		// connection is reused on the next attempt.
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

// Backoff returns the un-jittered wait before the 1-based attempt: base *
// 2^(attempt-2) from attempt 2, capped at the ceiling, and zero for attempt 1. It is
// exported so tests assert the schedule.
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

// backoffWithJitter returns the wait before attempt. With an entropy source it
// applies full jitter in [d/2, d], so concurrent retries spread out and never wait
// under half the schedule.
func (t *Transport) backoffWithJitter(attempt int) time.Duration {
	d := t.Backoff(attempt)
	if d <= 0 || t.jitter == nil {
		return d
	}
	return d/2 + t.jitter.upTo(d/2+1)
}

// sleep waits d on the clock, returning a Cancelled fault if the context ends first.
// A non-positive d returns at once.
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

// transportFault classifies a doer error. An explicit *fault.Error is kept, so a
// caller or chaos adapter can mark a failure Terminal to stop retries. A cancelled
// or expired context is Cancelled. Anything else is a Transient network failure,
// since raw transport errors are usually blips.
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

// rewind rebuilds the request body for a retry, reporting false for a non-nil body
// without GetBody so the caller never sends a truncated request. [http.NoBody]
// replays trivially: [http.NewRequest] leaves its GetBody nil, and refusing it would
// stop every retry of a bodiless request built the standard way.
func rewind(req *http.Request) (io.ReadCloser, bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return req.Body, true
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

// drain reads and closes a response body so its connection returns to the pool.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
