package request_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/integrations/request"
)

// outcome is a generated per-call result: a status code, or a network error.
type outcome struct {
	status  int  // 0 means "network error"
	success bool // a 2xx
	stop    bool // success or a non-retryable terminal: the loop must stop here
}

func drawOutcome(rt *rapid.T, label string) outcome {
	kind := rapid.SampledFrom([]string{"ok", "terminal", "transient", "neterr"}).Draw(rt, label)
	switch kind {
	case "ok":
		return outcome{status: 200, success: true, stop: true}
	case "terminal":
		return outcome{status: 404, stop: true}
	case "transient":
		return outcome{status: 503}
	default:
		return outcome{status: 0}
	}
}

func (o outcome) step() step {
	if o.status == 0 {
		return step{err: errors.New("network blip")}
	}
	return step{status: o.status}
}

// Property: Do spends at most maxAttempts calls, stops at the first success or
// non-retryable terminal, and the returned error is nil exactly when it stopped on
// a success. Retries fire immediately (zero backoff) so the property is about the
// control flow, not timing.
func TestProp_RetryStopsCorrectly(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		maxAtt := rapid.IntRange(1, 5).Draw(rt, "maxAttempts")
		n := rapid.IntRange(1, 8).Draw(rt, "n")
		outs := make([]outcome, n)
		steps := make([]step, 0, n+maxAtt)
		for i := range outs {
			outs[i] = drawOutcome(rt, fmt.Sprintf("o%d", i))
			steps = append(steps, outs[i].step())
		}
		// Pad with retryable failures so the doer never runs out and the padding never
		// causes an earlier stop than the generated outcomes dictate.
		for range maxAtt {
			steps = append(steps, step{status: 503})
		}

		doer := &scriptDoer{steps: steps}
		tr := request.New(request.WithDoer(doer), request.WithMaxAttempts(maxAtt), request.WithBackoff(0, 0))
		got, err := tr.Do(context.Background(), newReq(t))
		if got != nil {
			_ = got.Body.Close()
		}

		// Expected number of calls: index (1-based) of the first stopping outcome
		// within the first maxAtt outcomes, else maxAtt.
		wantCalls, stoppedOnSuccess := maxAtt, false
		for i := 0; i < maxAtt && i < len(outs); i++ {
			if outs[i].stop {
				wantCalls, stoppedOnSuccess = i+1, outs[i].success
				break
			}
		}
		if doer.calls != wantCalls {
			rt.Fatalf("calls = %d, want %d (outcomes=%v, max=%d)", doer.calls, wantCalls, outs, maxAtt)
		}
		if (err == nil) != stoppedOnSuccess {
			rt.Fatalf("err=%v but stoppedOnSuccess=%v", err, stoppedOnSuccess)
		}
		if err != nil && fault.Classify(err) == fault.Cancelled {
			rt.Fatalf("no context was cancelled, got cancelled fault")
		}
	})
}

// Property: a non-retryable terminal status is never retried, whatever the attempt
// ceiling: it always costs exactly one call.
func TestProp_TerminalNeverRetried(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		maxAtt := rapid.IntRange(1, 6).Draw(rt, "maxAttempts")
		// Any 4xx that is not 408/429 is terminal.
		status := rapid.SampledFrom([]int{400, 401, 403, 404, 422}).Draw(rt, "status")

		doer := &scriptDoer{steps: []step{{status: status}}}
		tr := request.New(request.WithDoer(doer), request.WithMaxAttempts(maxAtt), request.WithBackoff(0, 0))
		got, err := tr.Do(context.Background(), newReq(t))
		if got != nil {
			_ = got.Body.Close()
		}

		if fault.Classify(err) != fault.Terminal {
			rt.Fatalf("status %d: class = %q, want terminal", status, fault.Classify(err))
		}
		if doer.calls != 1 {
			rt.Fatalf("status %d: calls = %d, want 1", status, doer.calls)
		}
	})
}

// Property: the un-jittered backoff schedule starts at zero (the first attempt has
// no preceding wait), never decreases as the attempt grows, and never exceeds the
// ceiling.
func TestProp_BackoffMonotonicAndCapped(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		base := time.Duration(rapid.IntRange(1, 1000).Draw(rt, "baseMs")) * time.Millisecond
		ceiling := time.Duration(rapid.IntRange(1, 60000).Draw(rt, "maxMs")) * time.Millisecond
		tr := request.New(request.WithBackoff(base, ceiling))

		if got := tr.Backoff(1); got != 0 {
			rt.Fatalf("Backoff(1) = %v, want 0", got)
		}
		prev := time.Duration(0)
		for attempt := 2; attempt <= 12; attempt++ {
			b := tr.Backoff(attempt)
			if b < prev {
				rt.Fatalf("Backoff(%d)=%v < Backoff(prev)=%v (not monotonic)", attempt, b, prev)
			}
			if ceiling > 0 && b > ceiling {
				rt.Fatalf("Backoff(%d)=%v exceeds cap %v", attempt, b, ceiling)
			}
			prev = b
		}
	})
}
