package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// TestReceiveReconnectBackoffUsesClock proves the reconnect wait routes through the
// injected clock: after a failed getUpdates the bot does not re-poll until the
// Manual clock advances past the backoff, so retry timing is deterministic under
// test rather than a real 2s sleep.
func TestReceiveReconnectBackoffUsesClock(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0))
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&polls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError) // first poll fails -> backoff
			return
		}
		writeOK(w, `[{"update_id":10,"message":{"text":"hi","chat":{"id":1},"from":{"id":2,"username":"z"}}}]`)
	}))
	defer srv.Close()

	c, err := New("tok", WithBaseURL(srv.URL), WithPollTimeout(time.Second), WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, err := c.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The first poll fails and the loop parks on a backoff timer; no retry yet.
	waitFor(t, func() bool { return atomic.LoadInt32(&polls) == 1 && clk.PendingTimers() == 1 })
	select {
	case <-in:
		t.Fatal("delivered before the reconnect backoff elapsed on the clock")
	default:
	}

	// Advancing past the backoff releases the retry, which delivers the message.
	clk.Advance(retryBackoff)
	select {
	case spec := <-in:
		if spec.Content != "hi" {
			t.Fatalf("spec.Content = %q, want \"hi\"", spec.Content)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the message after the backoff advance")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 2000 {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}
