package controlplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// faultyStore wraps a store and fails every read, modelling a broken or unreachable
// backend. Writes and any method not overridden delegate to the embedded store.
type faultyStore struct {
	resource.Store
	err error
}

func (f faultyStore) ListAll(context.Context, string, resource.Selector) ([]resource.Resource, error) {
	return nil, f.err
}

func (f faultyStore) Get(context.Context, string, resource.Scope, string) (resource.Resource, error) {
	return resource.Resource{}, f.err
}

func (f faultyStore) GetByID(context.Context, string) (resource.Resource, error) {
	return resource.Resource{}, f.err
}

func (f faultyStore) GetAnyScope(context.Context, string, string) (resource.Resource, bool, error) {
	return resource.Resource{}, false, f.err
}

// faultyAuthenticator fails every authentication, modelling an identity store that is
// unreachable rather than a credential that is merely wrong.
type faultyAuthenticator struct{ err error }

func (f faultyAuthenticator) Authenticate(*http.Request) (Principal, error) {
	return Principal{}, f.err
}

// readFailingLog fails every Read, modelling a spine whose backing store is down.
// testkit.FaultyLog deliberately faults only the append path, and the watch loop reads,
// so the read fault is injected here. Append delegates: the watch never appends.
type readFailingLog struct {
	spine.Log
	err error
}

func (l readFailingLog) Read(context.Context, spine.Query) ([]spine.Event, error) {
	return nil, l.err
}

// faultyFlushWriter is an http.ResponseWriter and http.Flusher whose body writes fail on
// a seeded schedule, modelling a client that disappears mid-stream: a dropped
// connection, a killed curl, a proxy that closes the socket. Writes go through
// testkit.FaultyWriter so the failure lands on a chosen write rather than at a random
// point, and the test replays identically.
type faultyFlushWriter struct {
	rec  *httptest.ResponseRecorder
	body io.Writer
}

func newFaultyFlushWriter(plan *testkit.FaultPlan) *faultyFlushWriter {
	rec := httptest.NewRecorder()
	return &faultyFlushWriter{rec: rec, body: testkit.FaultyWriter(rec, plan)}
}

func (f *faultyFlushWriter) Header() http.Header         { return f.rec.Header() }
func (f *faultyFlushWriter) Write(p []byte) (int, error) { return f.body.Write(p) }
func (f *faultyFlushWriter) WriteHeader(code int)        { f.rec.WriteHeader(code) }
func (f *faultyFlushWriter) Flush()                      {}

// serveUntilReturn runs the handler on its own goroutine and reports whether it returned
// before the deadline. A wedged handler is the failure this whole file is about: the
// request goroutine and its poll ticker must be released, not left spinning against a
// dependency that will never answer.
func serveUntilReturn(t *testing.T, h http.Handler, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(w, r)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not return; the request is wedged")
	}
}

func watchRequest(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "/v1/Widget/watch", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer readtok")
	return r
}

func readAuth() Authenticator {
	return NewTokenAuthenticator(map[string]Principal{"readtok": {ID: "r", Scope: ScopeRead}})
}

// TestServerDegradesWhenStoreFails verifies the read handlers surface a store
// failure as an error response rather than panicking or hanging: a broken backend
// must not take the control-plane process down. A panic in a handler would fail this
// test, since ServeHTTP is called directly with no recover.
func TestServerDegradesWhenStoreFails(t *testing.T) {
	store, log := newStore(t)
	faulty := faultyStore{Store: store, err: errors.New("backing store unavailable")}
	h := NewServer(faulty, log, readAuth()).Handler()

	for _, path := range []string{"/v1/Widget", "/v1/Widget/w1"} {
		rec := do(t, h, path, "readtok")
		if rec.Code < 400 {
			t.Fatalf("%s under a failing store returned %d, want an error status", path, rec.Code)
		}
	}
}

// TestFailingIdentityStoreDeniesClosed verifies authentication fails closed. When the
// identity store cannot answer, the request must be refused, never admitted: "I do not
// know who you are" is not "you are allowed". A fail-open here would turn an identity
// outage into an unauthenticated control plane.
func TestFailingIdentityStoreDeniesClosed(t *testing.T) {
	store, log := newStore(t)
	putWidget(t, store, "w1")
	auth := faultyAuthenticator{err: errors.New("identity store unavailable")}
	h := NewServer(store, log, auth).Handler()

	for _, path := range []string{"/v1/Widget", "/v1/Widget/w1", "/v1/Widget/watch"} {
		rec := do(t, h, path, "anytoken")
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Fatalf("%s under a failing identity store returned %d, want 401 or 403", path, rec.Code)
		}
		if rec.Code == http.StatusOK {
			t.Fatalf("%s failed open under a failing identity store", path)
		}
	}
}

// TestWatchReturnsWhenClientDropsMidStream verifies a client that disappears part way
// through an SSE stream unwinds the handler instead of wedging it. The status line and
// headers are not body writes, so the first Write is the first event frame: failing it
// models the socket dying exactly when the stream starts flowing. writeSSE reports the
// error and handleWatch must return on it, releasing the goroutine and the poll ticker.
func TestWatchReturnsWhenClientDropsMidStream(t *testing.T) {
	store, log := newStore(t)
	putWidget(t, store, "w1") // an event is already on the stream, so the first poll writes

	srv := NewServer(store, log, readAuth(), WithWatchPoll(5*time.Millisecond))
	w := newFaultyFlushWriter(testkit.FailOnCall(1, errors.New("connection reset by peer")))

	serveUntilReturn(t, srv.Handler(), w, watchRequest(t))
}

// TestWatchGivesUpOnPersistentStoreFailure verifies a watch against a store that never
// recovers terminates rather than polling and logging forever. handleWatch bounds itself
// to maxWatchReadErrors consecutive read failures, so a broken backend costs one bounded
// request per client, not an unbounded error stream.
func TestWatchGivesUpOnPersistentStoreFailure(t *testing.T) {
	store, log := newStore(t)
	failing := readFailingLog{Log: log, err: errors.New("spine unavailable")}

	srv := NewServer(store, failing, readAuth(), WithWatchPoll(time.Millisecond))
	serveUntilReturn(t, srv.Handler(), httptest.NewRecorder(), watchRequest(t))
}
