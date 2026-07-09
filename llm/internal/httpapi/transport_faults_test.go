package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/testkit"
)

// errTransport fails every round trip, modelling a network-level failure (a refused
// connection or DNS error) that yields no HTTP response at all, rather than a status.
type errTransport struct{ calls int }

func (e *errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	e.calls++
	return nil, errors.New("dial tcp 203.0.113.1:443: connect: connection refused")
}

// statusTransport answers every round trip with the same status and a freshly built
// body, so a retried request reads the same response the first attempt did rather than
// an exhausted reader. It counts attempts, which is how a bounded retry is observed.
type statusTransport struct {
	status int
	body   string
	calls  int
}

func (s *statusTransport) RoundTrip(*http.Request) (*http.Response, error) {
	s.calls++
	return &http.Response{
		StatusCode: s.status,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     make(http.Header),
	}, nil
}

// bodyTransport answers once with a caller-supplied body, so a test can hand the
// client a response whose body fails part way through being read.
type bodyTransport struct{ body io.ReadCloser }

func (b *bodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: b.body, Header: make(http.Header)}, nil
}

// nopCloser adapts a reader into a response body.
type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }

// TestPostJSONTransportErrorIsSurfaced verifies a round trip that returns no
// response surfaces a typed fault under the provider's code (never a raw error, a
// nil-dereference, or a panic), so the caller can classify and act on it.
func TestPostJSONTransportErrorIsSurfaced(t *testing.T) {
	rt := &errTransport{}
	err := post(t, clientWith(rt))
	if err == nil {
		t.Fatal("a failed round trip must surface an error")
	}
	if rt.calls == 0 {
		t.Fatal("the transport was never called")
	}
	if fault.Classify(err) == "" {
		t.Fatalf("a transport failure must carry a fault class: %v", err)
	}
}

// TestPostJSONContextCancelled verifies a cancelled context fails the call promptly
// with a Cancelled-classed fault instead of hanging or panicking.
func TestPostJSONContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out struct{}
	c := clientWith(&seqTransport{responses: []*http.Response{resp(200, `{}`)}}) //nolint:bodyclose // PostJSON closes the body
	err := c.PostJSON(ctx, "/v1/x", map[string]string{}, &out)
	if err == nil {
		t.Fatal("a cancelled context must fail the call")
	}
	if got := fault.Classify(err); got != fault.Cancelled {
		t.Fatalf("cancelled context classified %v, want Cancelled: %v", got, err)
	}
}

// TestPostJSONBodyFailsMidStream verifies a response body that dies part way through
// being read fails as a transient fault rather than panicking or, worse, decoding the
// truncated prefix as if it were the whole response. This is the decode loop's real
// failure mode: the status said 200 and the frame started arriving before the
// connection dropped. The fault is seeded through testkit.FaultyReader so the read
// fails on a chosen call and the run replays exactly.
func TestPostJSONBodyFailsMidStream(t *testing.T) {
	// A body long enough that io.ReadAll needs more than one Read: the first read
	// succeeds, the second dies, exactly as a connection dropped mid-response.
	full := `{"content":[{"type":"text","text":"` + strings.Repeat("a", 64<<10) + `"}]}`
	faulty := testkit.FaultyReader(strings.NewReader(full), testkit.FailOnCall(2, errors.New("unexpected EOF")))

	var out map[string]any
	c := clientWith(&bodyTransport{body: nopCloser{faulty}})
	err := c.PostJSON(context.Background(), "/v1/x", map[string]string{}, &out)
	if err == nil {
		t.Fatal("a body that fails mid-read must fail the call, not decode a truncated prefix")
	}
	if got := fault.Classify(err); got != fault.Transient {
		t.Fatalf("mid-stream read failure classified %v, want Transient: %v", got, err)
	}
	if len(out) != 0 {
		t.Fatalf("a failed read populated the output: %v", out)
	}
}

// TestPostJSONGivesUpOnPersistent5xx verifies retry is bounded. A server error is
// transient, so the transport retries it with backoff; against a provider that is down
// for good, that retry must terminate and surface the fault rather than hammering the
// endpoint forever. TestPostJSONRetriesTransientThenSucceeds covers the recovering
// case; this covers the one that never recovers.
func TestPostJSONGivesUpOnPersistent5xx(t *testing.T) {
	rt := &statusTransport{status: 503, body: `{"error":{"message":"unavailable"}}`}
	var out map[string]any
	c := clientWith(rt)

	err := c.PostJSON(context.Background(), "/v1/x", map[string]string{}, &out)
	if err == nil {
		t.Fatal("a permanently failing provider must surface an error")
	}
	if got := fault.Classify(err); got != fault.Transient {
		t.Fatalf("a persistent 5xx classified %v, want Transient: %v", got, err)
	}
	if rt.calls < 2 {
		t.Fatalf("a 5xx was attempted %d time(s); a transient status must be retried", rt.calls)
	}
	if rt.calls > 8 {
		t.Fatalf("a 5xx was attempted %d times; retry must be bounded", rt.calls)
	}
	if len(out) != 0 {
		t.Fatalf("a failed call populated the output: %v", out)
	}
}
