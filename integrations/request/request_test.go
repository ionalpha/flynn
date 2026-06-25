package request_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/integrations/request"
)

// step is one scripted outcome of a doer call as plain data: either an error, or a
// status (with optional body and headers) the doer materialises into a response when
// called. Keeping fixtures as data rather than prebuilt *http.Response values means
// the only responses that exist are the ones the doer hands to the transport.
type step struct {
	status int
	body   string
	hdr    http.Header
	err    error
}

// scriptDoer replays a fixed sequence of outcomes and counts how many calls it saw,
// so a test can assert both the result and exactly how many attempts were spent.
type scriptDoer struct {
	steps []step
	calls int
}

func (d *scriptDoer) Do(*http.Request) (*http.Response, error) {
	i := d.calls
	d.calls++
	if i >= len(d.steps) {
		return nil, errors.New("scriptDoer: unexpected extra call")
	}
	s := d.steps[i]
	if s.err != nil {
		return nil, s.err
	}
	hdr := s.hdr
	if hdr == nil {
		hdr = http.Header{}
	}
	return &http.Response{StatusCode: s.status, Body: io.NopCloser(strings.NewReader(s.body)), Header: hdr}, nil
}

func newReq(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://example.test/path", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// waitTimers waits until at least n timers are armed on the manual clock, so a test
// advances the clock only once the transport has parked on its wait. It polls with a
// brief sleep, mirroring the project's existing timer-wait helpers; the bounded loop
// is a test-failure guard, not part of the behaviour under test.
func waitTimers(t *testing.T, clk *clock.Manual, n int) {
	t.Helper()
	for range 2000 {
		if clk.PendingTimers() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timer never registered (want %d, have %d)", n, clk.PendingTimers())
}

func TestDo_SuccessNoRetry(t *testing.T) {
	doer := &scriptDoer{steps: []step{{status: 200, body: "ok"}}}
	tr := request.New(request.WithDoer(doer), request.WithBackoff(0, 0))

	got, err := tr.Do(context.Background(), newReq(t))
	if got != nil {
		_ = got.Body.Close()
	}
	if err != nil {
		t.Fatalf("Do: unexpected error %v", err)
	}
	if got.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", got.StatusCode)
	}
	if doer.calls != 1 {
		t.Fatalf("calls = %d, want 1", doer.calls)
	}
}

func TestDo_TerminalNotRetried(t *testing.T) {
	doer := &scriptDoer{steps: []step{{status: 404, body: "nope"}}}
	tr := request.New(request.WithDoer(doer), request.WithMaxAttempts(3), request.WithBackoff(0, 0))

	got, err := tr.Do(context.Background(), newReq(t))
	if got != nil {
		_ = got.Body.Close()
	}
	if fault.Classify(err) != fault.Terminal {
		t.Fatalf("class = %q, want terminal", fault.Classify(err))
	}
	if got == nil || got.StatusCode != 404 {
		t.Fatalf("want the 404 response returned for inspection, got %v", got)
	}
	if doer.calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on terminal)", doer.calls)
	}
}

func TestDo_RetriesTransientThenSucceeds(t *testing.T) {
	doer := &scriptDoer{steps: []step{
		{status: 500},
		{status: 503},
		{status: 200, body: "ok"},
	}}
	tr := request.New(request.WithDoer(doer), request.WithMaxAttempts(3), request.WithBackoff(0, 0))

	got, err := tr.Do(context.Background(), newReq(t))
	if got != nil {
		_ = got.Body.Close()
	}
	if err != nil {
		t.Fatalf("Do: unexpected error %v", err)
	}
	if got.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", got.StatusCode)
	}
	if doer.calls != 3 {
		t.Fatalf("calls = %d, want 3", doer.calls)
	}
}

func TestDo_StopsAtCeiling(t *testing.T) {
	doer := &scriptDoer{steps: []step{
		{status: 500},
		{status: 500},
		{status: 500},
	}}
	tr := request.New(request.WithDoer(doer), request.WithMaxAttempts(3), request.WithBackoff(0, 0))

	got, err := tr.Do(context.Background(), newReq(t))
	if got != nil {
		_ = got.Body.Close()
	}
	if fault.Classify(err) != fault.Transient {
		t.Fatalf("class = %q, want transient", fault.Classify(err))
	}
	if got == nil || got.StatusCode != 500 {
		t.Fatalf("want last 500 response returned, got %v", got)
	}
	if doer.calls != 3 {
		t.Fatalf("calls = %d, want 3 (ceiling)", doer.calls)
	}
}

func TestDo_NetworkErrorRetries(t *testing.T) {
	doer := &scriptDoer{steps: []step{
		{err: errors.New("connection refused")},
		{err: errors.New("connection reset")},
		{status: 200, body: "ok"},
	}}
	tr := request.New(request.WithDoer(doer), request.WithMaxAttempts(3), request.WithBackoff(0, 0))

	got, err := tr.Do(context.Background(), newReq(t))
	if got != nil {
		_ = got.Body.Close()
	}
	if err != nil {
		t.Fatalf("Do: unexpected error %v", err)
	}
	if got.StatusCode != 200 || doer.calls != 3 {
		t.Fatalf("status=%d calls=%d, want 200 and 3", got.StatusCode, doer.calls)
	}
}

// A 429 with Retry-After waits exactly that long on the clock, then retries. Driven
// by a manual clock so the wait is deterministic with no real sleeping.
func TestDo_RetryAfterHonoured(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0).UTC())
	doer := &scriptDoer{steps: []step{
		{status: 429, hdr: http.Header{"Retry-After": {"2"}}},
		{status: 200, body: "ok"},
	}}
	tr := request.New(request.WithDoer(doer), request.WithClock(clk),
		request.WithMaxAttempts(3), request.WithBackoff(time.Minute, time.Minute))

	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		r, e := tr.Do(context.Background(), newReq(t))
		if r != nil {
			_ = r.Body.Close()
		}
		done <- result{r, e}
	}()

	waitTimers(t, clk, 1) // the Retry-After wait is armed
	clk.Advance(2 * time.Second)

	got := <-done
	if got.err != nil {
		t.Fatalf("Do: unexpected error %v", got.err)
	}
	if got.resp.StatusCode != 200 || doer.calls != 2 {
		t.Fatalf("status=%d calls=%d, want 200 and 2", got.resp.StatusCode, doer.calls)
	}
}

// A context cancelled during a backoff wait stops with a Cancelled fault.
func TestDo_ContextCancelDuringBackoff(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0).UTC())
	doer := &scriptDoer{steps: []step{{status: 500}, {status: 200, body: "ok"}}}
	tr := request.New(request.WithDoer(doer), request.WithClock(clk),
		request.WithMaxAttempts(3), request.WithBackoff(time.Minute, time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		r, e := tr.Do(ctx, newReq(t))
		if r != nil {
			_ = r.Body.Close()
		}
		errc <- e
	}()

	waitTimers(t, clk, 1) // backoff wait is armed
	cancel()

	if got := fault.Classify(<-errc); got != fault.Cancelled {
		t.Fatalf("class = %q, want cancelled", got)
	}
}
