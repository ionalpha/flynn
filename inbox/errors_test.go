package inbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/resource"
)

// The failure paths of the inbound boundary. Every one of them is a path a live
// deployment reaches: a source whose platform is unreachable, a store that will not
// write, work that will not start, a sink that will not deliver. What they pin is
// that no inbound entry is silently lost: a failure is either reported to the
// caller (so the reconcile loop retries the entry) or handed to the error handler,
// and the entry is never marked settled on the strength of an action that failed.

// errBackend is the injected store failure the tests below assert on.
var errBackend = errors.New("store unreachable")

// faultyStore wraps a resource.Store and fails, or corrupts, the reads and writes a
// test names. The shared testkit injectors wrap no resource.Store, so this is the
// smallest thing that models a backend that is down or a record that came back
// unreadable.
type faultyStore struct {
	resource.Store

	getErr       error // Get fails with this
	putErr       error // Put fails with this
	putAfter     int   // ... but only from this Put onward (0 = every Put)
	mangleSpec   bool  // Get returns a resource whose spec cannot be decoded
	mangleStatus bool  // ... or whose status cannot be decoded

	mu   sync.Mutex
	puts int
}

func (s *faultyStore) Get(ctx context.Context, kind string, scope resource.Scope, name string) (resource.Resource, error) {
	if s.getErr != nil {
		return resource.Resource{}, s.getErr
	}
	r, err := s.Store.Get(ctx, kind, scope, name)
	if err != nil {
		return r, err
	}
	if s.mangleSpec {
		r.Spec = json.RawMessage(`"not an object"`)
	}
	if s.mangleStatus {
		r.Status = json.RawMessage(`"not an object"`)
	}
	return r, nil
}

func (s *faultyStore) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	s.mu.Lock()
	s.puts++
	n := s.puts
	s.mu.Unlock()
	if s.putErr != nil && n > s.putAfter {
		return resource.Resource{}, s.putErr
	}
	return s.Store.Put(ctx, r)
}

// flakyWorker is a Worker whose Start or Poll fails on demand, modelling a goal
// engine that is down or work whose state cannot be read this reconcile.
type flakyWorker struct {
	mu       sync.Mutex
	starts   int
	startErr error
	pollErr  error
	done     bool
	answer   string
	failed   bool
}

func (w *flakyWorker) Start(context.Context, string, string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.startErr != nil {
		return "", w.startErr
	}
	w.starts++
	return "g1", nil
}

func (w *flakyWorker) Poll(context.Context, string) (bool, string, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pollErr != nil {
		return false, "", false, w.pollErr
	}
	return w.done, w.answer, w.failed, nil
}

func (w *flakyWorker) complete(answer string, failed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done, w.answer, w.failed = true, answer, failed
}

// started reports how many times work was started for an entry.
func (w *flakyWorker) started() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.starts
}

// setStatus writes st onto a stored entry, so a test can start a reconcile from any
// phase rather than driving the whole lifecycle to reach it.
func setStatus(t *testing.T, store resource.Store, key resource.Key, st inbox.Status) {
	t.Helper()
	ctx := context.Background()
	r, err := store.Get(ctx, key.Kind, key.Scope, key.Name)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := st.Encode()
	if err != nil {
		t.Fatal(err)
	}
	r.Status = raw
	if _, err := store.Put(ctx, r); err != nil {
		t.Fatal(err)
	}
}

// newTriage builds a triage reconciler over store with a manual clock.
func newTriage(store resource.Store, worker inbox.Worker, sinks *inbox.Sinks, opts ...inbox.TriageOption) *inbox.Triage {
	return inbox.NewTriage(store, worker, sinks, clock.NewManual(time.Unix(1, 0)), opts...)
}

// TestReconcileOfADeletedEntryIsANoOp: a change hint can outlive the entry it names,
// and an entry that is already gone is settled, not an error to retry forever.
func TestReconcileOfADeletedEntryIsANoOp(t *testing.T) {
	store := newStore(t)
	worker := &flakyWorker{}
	tri := newTriage(store, worker, inbox.NewSinks())

	res, err := tri.Reconcile(context.Background(), resource.Key{Kind: inbox.Kind, Name: "entry-gone"})
	if err != nil {
		t.Fatalf("reconciling a deleted entry = %v, want nil", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("a deleted entry was requeued after %v", res.RequeueAfter)
	}
	if worker.started() != 0 {
		t.Error("a deleted entry started work")
	}
}

// TestReconcileSurfacesAnUnreadableEntry: a store that is down, or a record whose
// spec or status will not decode, must fail the reconcile so the entry is retried
// rather than quietly dropped from the inbound path.
func TestReconcileSurfacesAnUnreadableEntry(t *testing.T) {
	cases := []struct {
		name  string
		store func(inner resource.Store) *faultyStore
		want  string
	}{
		{
			name:  "the store cannot be read",
			store: func(inner resource.Store) *faultyStore { return &faultyStore{Store: inner, getErr: errBackend} },
			want:  errBackend.Error(),
		},
		{
			name:  "the entry's spec will not decode",
			store: func(inner resource.Store) *faultyStore { return &faultyStore{Store: inner, mangleSpec: true} },
			want:  "json",
		},
		{
			name:  "the entry's status will not decode",
			store: func(inner resource.Store) *faultyStore { return &faultyStore{Store: inner, mangleStatus: true} },
			want:  "json",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := newStore(t)
			key := putEntry(t, inner, inbox.Spec{Source: "telegram", Conversation: "c1", Content: "hi"})
			worker := &flakyWorker{}
			tri := newTriage(tc.store(inner), worker, inbox.NewSinks(&fakeSink{name: "telegram"}))

			_, err := tri.Reconcile(context.Background(), key)
			if err == nil {
				t.Fatal("Reconcile succeeded over an entry it could not read")
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Errorf("error %q does not report %s", err, tc.want)
			}
			if worker.started() != 0 {
				t.Error("work was started for an entry that could not be read")
			}
		})
	}
}

// TestReconcileIgnoresAPhaseItDoesNotKnow: a phase written by a newer build (or a
// hand-edited entry) is left alone rather than re-triaged, which would act on an
// entry twice.
func TestReconcileIgnoresAPhaseItDoesNotKnow(t *testing.T) {
	store := newStore(t)
	key := putEntry(t, store, inbox.Spec{Source: "telegram", Conversation: "c1", Content: "hi"})
	setStatus(t, store, key, inbox.Status{Phase: inbox.Phase("Quarantined")})

	worker := &flakyWorker{}
	res, err := newTriage(store, worker, inbox.NewSinks()).Reconcile(context.Background(), key)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("an unknown phase was requeued after %v", res.RequeueAfter)
	}
	if worker.started() != 0 {
		t.Error("an entry in an unknown phase started work")
	}
	if got := statusOf(t, store, key).Phase; got != inbox.Phase("Quarantined") {
		t.Errorf("phase = %q, want it left untouched", got)
	}
}

// TestTriageSurfacesAFailureToStartWork: the entry is left un-triaged, so the next
// reconcile retries it rather than settling an entry whose work never began.
func TestTriageSurfacesAFailureToStartWork(t *testing.T) {
	store := newStore(t)
	key := putEntry(t, store, inbox.Spec{Source: "telegram", Conversation: "c1", Content: "hi"})
	worker := &flakyWorker{startErr: errBackend}

	_, err := newTriage(store, worker, inbox.NewSinks(&fakeSink{name: "telegram"})).Reconcile(context.Background(), key)
	if !errors.Is(err, errBackend) {
		t.Fatalf("Reconcile = %v, want the failure to start work", err)
	}
	if got := statusOf(t, store, key).Phase; got != "" {
		t.Errorf("phase = %q, want the entry left un-triaged for the retry", got)
	}
}

// TestTriageSurfacesAFailureToRecordTheDisposition: the work has started and the
// handle could not be written. Reporting it requeues the entry, which is the
// at-least-once trade the Worker's per-entry idempotence covers.
func TestTriageSurfacesAFailureToRecordTheDisposition(t *testing.T) {
	inner := newStore(t)
	key := putEntry(t, inner, inbox.Spec{Source: "telegram", Conversation: "c1", Content: "hi"})
	worker := &flakyWorker{}
	// The entry's own Put has already happened, so the next one is triage's.
	store := &faultyStore{Store: inner, putErr: errBackend}

	_, err := newTriage(store, worker, inbox.NewSinks(&fakeSink{name: "telegram"})).Reconcile(context.Background(), key)
	if !errors.Is(err, errBackend) {
		t.Fatalf("Reconcile = %v, want the failed status write", err)
	}
	if worker.started() != 1 {
		t.Errorf("worker.Start called %d times, want 1", worker.started())
	}
	if got := statusOf(t, inner, key).Phase; got != "" {
		t.Errorf("phase = %q, want no disposition recorded when the write failed", got)
	}
}

// TestActRejectsATriagedEntryWithNoHandle: a triaged entry names the work it is
// waiting on. One that does not cannot be polled, and inventing a handle would poll
// somebody else's work.
func TestActRejectsATriagedEntryWithNoHandle(t *testing.T) {
	store := newStore(t)
	key := putEntry(t, store, inbox.Spec{Source: "telegram", Conversation: "c1", Content: "hi"})
	setStatus(t, store, key, inbox.Status{Phase: inbox.PhaseTriaged, Disposition: inbox.DispositionReply})

	worker := &flakyWorker{}
	_, err := newTriage(store, worker, inbox.NewSinks(&fakeSink{name: "telegram"})).Reconcile(context.Background(), key)
	if err == nil {
		t.Fatal("a triaged entry with no work handle reconciled cleanly")
	}
	if !strings.Contains(err.Error(), "handle") {
		t.Errorf("error %q does not say the entry has no work handle", err)
	}
}

// TestActSurfacesAFailedPoll: the work's state is unknown, so the entry stays
// triaged and is retried, rather than being acted on with an answer nobody has.
func TestActSurfacesAFailedPoll(t *testing.T) {
	store := newStore(t)
	key := putEntry(t, store, inbox.Spec{Source: "telegram", Conversation: "c1", Content: "hi"})
	sink := &fakeSink{name: "telegram"}
	worker := &flakyWorker{pollErr: errBackend}
	tri := newTriage(store, worker, inbox.NewSinks(sink))

	if _, err := tri.Reconcile(context.Background(), key); err != nil { // triage + start
		t.Fatal(err)
	}
	_, err := tri.Reconcile(context.Background(), key) // poll fails
	if !errors.Is(err, errBackend) {
		t.Fatalf("Reconcile = %v, want the failed poll", err)
	}
	if len(sink.sends()) != 0 {
		t.Error("a reply was sent though the work's state is unknown")
	}
	if got := statusOf(t, store, key).Phase; got != inbox.PhaseTriaged {
		t.Errorf("phase = %q, want still Triaged for the retry", got)
	}
}

// TestActLeavesTheEntryUnActedWhenTheReplyCannotBeSent: the answer exists and the
// user has not seen it. Marking the entry acted would drop the reply for good, so
// the send failure is reported and the entry stays triaged for the next attempt.
func TestActLeavesTheEntryUnActedWhenTheReplyCannotBeSent(t *testing.T) {
	store := newStore(t)
	key := putEntry(t, store, inbox.Spec{Source: "telegram", Conversation: "c1", Content: "hi"})
	worker := &flakyWorker{}
	// No sink is registered for "telegram": the reply has nowhere to go.
	tri := newTriage(store, worker, inbox.NewSinks())

	if _, err := tri.Reconcile(context.Background(), key); err != nil { // triage + start
		t.Fatal(err)
	}
	worker.complete("the answer", false)

	_, err := tri.Reconcile(context.Background(), key)
	if err == nil {
		t.Fatal("acting succeeded though the reply could not be delivered")
	}
	if !strings.Contains(err.Error(), "no sink") {
		t.Errorf("error %q does not say the reply had nowhere to go", err)
	}
	if got := statusOf(t, store, key).Phase; got != inbox.PhaseTriaged {
		t.Errorf("phase = %q, want still Triaged: an undelivered reply is not acted", got)
	}
}

// TestTriageOptionsOverrideTheDefaults: a host supplies its own policy and its own
// poll period. The policy decides the disposition from the entry alone, so a host
// that routes conversational entries to a goal gets a goal, and no reply is sent.
func TestTriageOptionsOverrideTheDefaults(t *testing.T) {
	store := newStore(t)
	key := putEntry(t, store, inbox.Spec{Source: "telegram", Conversation: "c1", Content: "ship it"})
	worker := &flakyWorker{}
	sink := &fakeSink{name: "telegram"}

	const poll = 5 * time.Second
	tri := newTriage(
		store, worker, inbox.NewSinks(sink),
		inbox.WithPolicy(func(inbox.Spec) inbox.Disposition { return inbox.DispositionGoal }),
		inbox.WithPollInterval(poll),
		inbox.WithPolicy(nil),      // nil is ignored: the policy above stands
		inbox.WithPollInterval(-1), // as is a non-positive interval
	)

	res, err := tri.Reconcile(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != poll {
		t.Errorf("RequeueAfter = %v, want the configured %v", res.RequeueAfter, poll)
	}
	if st := statusOf(t, store, key); st.Disposition != inbox.DispositionGoal {
		t.Fatalf("disposition = %q, want the policy's Goal", st.Disposition)
	}

	// A goal is fire-and-forget: the work completes and nothing is sent back.
	worker.complete("the answer", false)
	if _, err := tri.Reconcile(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if got := sink.sends(); len(got) != 0 {
		t.Errorf("a Goal disposition replied on the conversation: %v", got)
	}
	if st := statusOf(t, store, key).Phase; st != inbox.PhaseActed {
		t.Errorf("phase = %q, want Acted", st)
	}
}

// failingSource is a Source whose platform cannot be reached, so Receive never
// starts.
type failingSource struct{ name string }

func (s *failingSource) Name() string { return s.name }

func (s *failingSource) Receive(context.Context) (<-chan inbox.Spec, error) {
	return nil, errBackend
}

// TestIngestReportsASourceThatWillNotStartAndRunsTheRest: one unreachable platform
// must not take the whole inbound boundary down with it.
func TestIngestReportsASourceThatWillNotStartAndRunsTheRest(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	q := &recordingQueue{}

	var mu sync.Mutex
	var reported []error
	in := inbox.NewIngest(
		store, q, clock.NewManual(time.Unix(1, 0)),
		[]inbox.Source{
			&failingSource{name: "signal"},
			&batchSource{name: "telegram", specs: []inbox.Spec{{Conversation: "c1", Content: "hi"}}},
		},
		inbox.WithIngestErrorHandler(func(err error) {
			mu.Lock()
			defer mu.Unlock()
			reported = append(reported, err)
		}),
		inbox.WithIngestErrorHandler(nil), // nil is ignored: the handler above stands
	)

	if err := in.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 {
		t.Fatalf("reported %v, want exactly the source that would not start", reported)
	}
	if !errors.Is(reported[0], errBackend) || !strings.Contains(reported[0].Error(), "signal") {
		t.Errorf("reported error %q does not name the failing source and its cause", reported[0])
	}
	if q.count() != 1 {
		t.Errorf("enqueued %d entries, want the working source's 1", q.count())
	}
}

// TestIngestFailsWhenNoSourceCanStart: with every platform unreachable, Run returns
// rather than blocking forever on readers that were never started.
func TestIngestFailsWhenNoSourceCanStart(t *testing.T) {
	in := inbox.NewIngest(newStore(t), &recordingQueue{}, clock.NewManual(time.Unix(1, 0)),
		[]inbox.Source{&failingSource{name: "signal"}, &failingSource{name: "telegram"}})

	err := in.Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil though no source could start")
	}
	if !strings.Contains(err.Error(), "no sources could start") {
		t.Errorf("error %q does not say no source could start", err)
	}
}

// TestIngestReportsAnEntryItCouldNotRecord: an entry the store rejects is reported
// to the handler and never enqueued, so triage is never handed a key that names no
// entry.
func TestIngestReportsAnEntryItCouldNotRecord(t *testing.T) {
	q := &recordingQueue{}
	store := &faultyStore{Store: newStore(t), putErr: errBackend}

	var mu sync.Mutex
	var reported []error
	src := &batchSource{name: "telegram", specs: []inbox.Spec{{Conversation: "c1", Content: "hi"}}}
	in := inbox.NewIngest(store, q, clock.NewManual(time.Unix(1, 0)), []inbox.Source{src},
		inbox.WithIngestErrorHandler(func(err error) {
			mu.Lock()
			defer mu.Unlock()
			reported = append(reported, err)
		}))

	if err := in.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 || !errors.Is(reported[0], errBackend) {
		t.Fatalf("reported %v, want the record failure", reported)
	}
	if !strings.Contains(reported[0].Error(), "telegram") {
		t.Errorf("reported error %q does not name the source the entry arrived on", reported[0])
	}
	if q.count() != 0 {
		t.Errorf("enqueued %d keys for entries that were never recorded", q.count())
	}
}
