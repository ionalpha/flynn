package request_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/integrations/request"
	"github.com/ionalpha/flynn/internal/testkit"
)

// countDoer counts the round trips that reach it, so a chaos test can prove how many
// attempts a failure pattern actually cost.
type countDoer struct {
	inner request.Doer
	calls int
}

func (c *countDoer) Do(r *http.Request) (*http.Response, error) {
	c.calls++
	return c.inner.Do(r)
}

// Chaos: a network that fails the first two calls then recovers. Driven through the
// shared testkit fault layer (FaultyDoer + FaultPlan), the transport retries and
// ultimately succeeds. Deterministic: the same plan reproduces the same outcome.
func TestChaos_TransientThenRecover(t *testing.T) {
	plan := testkit.FailFirst(2, fault.New(fault.Transient, "net_blip", "temporary"))
	doer := &countDoer{inner: testkit.FaultyDoer(nil, plan)}
	tr := request.New(request.WithDoer(doer), request.WithMaxAttempts(3), request.WithBackoff(0, 0))

	got, err := tr.Do(context.Background(), newReq(t))
	if got != nil {
		_ = got.Body.Close()
	}
	if err != nil {
		t.Fatalf("Do: unexpected error %v", err)
	}
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", got.StatusCode)
	}
	if doer.calls != 3 {
		t.Fatalf("calls = %d, want 3 (two failures, one success)", doer.calls)
	}
}

// Chaos: a hard outage that never recovers. The transport spends its full attempt
// budget and surfaces a Transient fault, never a panic or a hang.
func TestChaos_HardOutageStopsAtCeiling(t *testing.T) {
	plan := testkit.Always(fault.New(fault.Transient, "net_down", "outage"))
	doer := &countDoer{inner: testkit.FaultyDoer(nil, plan)}
	tr := request.New(request.WithDoer(doer), request.WithMaxAttempts(4), request.WithBackoff(0, 0))

	got, err := tr.Do(context.Background(), newReq(t))
	if got != nil {
		_ = got.Body.Close()
	}
	if fault.Classify(err) != fault.Transient {
		t.Fatalf("class = %q, want transient", fault.Classify(err))
	}
	if doer.calls != 4 {
		t.Fatalf("calls = %d, want 4 (ceiling)", doer.calls)
	}
}

// Chaos: an injected Terminal transport error is honoured as non-retryable, so a
// classified hard failure is never replayed even with attempts to spare.
func TestChaos_TerminalTransportNotRetried(t *testing.T) {
	plan := testkit.Always(fault.New(fault.Terminal, "tls_bad_cert", "untrusted"))
	doer := &countDoer{inner: testkit.FaultyDoer(nil, plan)}
	tr := request.New(request.WithDoer(doer), request.WithMaxAttempts(5), request.WithBackoff(0, 0))

	got, err := tr.Do(context.Background(), newReq(t))
	if got != nil {
		_ = got.Body.Close()
	}
	if fault.Classify(err) != fault.Terminal {
		t.Fatalf("class = %q, want terminal", fault.Classify(err))
	}
	if doer.calls != 1 {
		t.Fatalf("calls = %d, want 1 (terminal not retried)", doer.calls)
	}
}
