package inbox_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/resource"
)

// fakeWorker records the work it was asked to start and returns a programmable poll
// result the test mutates between reconciles.
type fakeWorker struct {
	mu         sync.Mutex
	starts     int
	startedSig string
	startedObj string
	done       bool
	answer     string
	failed     bool
}

func (w *fakeWorker) Start(_ context.Context, sig, obj string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.starts++
	w.startedSig, w.startedObj = sig, obj
	return "g1", nil
}

func (w *fakeWorker) Poll(context.Context, string) (bool, string, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.done, w.answer, w.failed, nil
}

func (w *fakeWorker) complete(answer string, failed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done, w.answer, w.failed = true, answer, failed
}

// fakeSink records the replies it is asked to send.
type fakeSink struct {
	name string
	mu   sync.Mutex
	sent [][2]string // {conversation, text}
}

func (s *fakeSink) Name() string { return s.name }

func (s *fakeSink) Send(_ context.Context, conversation, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, [2]string{conversation, text})
	return nil
}

func (s *fakeSink) sends() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]string(nil), s.sent...)
}

// putEntry stores an entry with the given spec and returns its key.
func putEntry(t *testing.T, store resource.Store, spec inbox.Spec) resource.Key {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Put(context.Background(), resource.Resource{
		APIVersion:   inbox.GroupVersion,
		Kind:         inbox.Kind,
		GenerateName: "entry-",
		Spec:         raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	return saved.Key()
}

func statusOf(t *testing.T, store resource.Store, key resource.Key) inbox.Status {
	t.Helper()
	r, err := store.Get(context.Background(), key.Kind, key.Scope, key.Name)
	if err != nil {
		t.Fatal(err)
	}
	st, err := inbox.DecodeStatus(r)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestTriageRepliesOnConvergedWork(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	worker := &fakeWorker{}
	sink := &fakeSink{name: "telegram"}
	tri := inbox.NewTriage(store, worker, inbox.NewSinks(sink), clock.NewManual(time.Unix(1, 0)))

	key := putEntry(t, store, inbox.Spec{Source: "telegram", Conversation: "c1", Content: "hi"})

	// First reconcile: triage and start the work.
	res, err := tri.Reconcile(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatal("expected a requeue while work is in flight")
	}
	if worker.starts != 1 || worker.startedObj != "hi" {
		t.Fatalf("worker.Start = (%d, %q), want (1, hi)", worker.starts, worker.startedObj)
	}
	if st := statusOf(t, store, key); st.Phase != inbox.PhaseTriaged || st.Disposition != inbox.DispositionReply {
		t.Fatalf("status = %+v, want Triaged/Reply", st)
	}

	// Work completes; next reconcile replies and settles.
	worker.complete("the answer", false)
	if _, err := tri.Reconcile(ctx, key); err != nil {
		t.Fatal(err)
	}
	sent := sink.sends()
	if len(sent) != 1 || sent[0] != [2]string{"c1", "the answer"} {
		t.Fatalf("sent = %v, want one reply {c1, the answer}", sent)
	}
	if st := statusOf(t, store, key); st.Phase != inbox.PhaseActed {
		t.Fatalf("phase = %q, want Acted", st.Phase)
	}
}

func TestTriageWaitsWhileWorkInFlight(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	worker := &fakeWorker{} // never completes
	sink := &fakeSink{name: "telegram"}
	tri := inbox.NewTriage(store, worker, inbox.NewSinks(sink), clock.NewManual(time.Unix(1, 0)))
	key := putEntry(t, store, inbox.Spec{Source: "telegram", Conversation: "c1", Content: "hi"})

	if _, err := tri.Reconcile(ctx, key); err != nil { // triage + start
		t.Fatal(err)
	}
	res, err := tri.Reconcile(ctx, key) // poll: not done
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatal("expected a requeue while work is still in flight")
	}
	if len(sink.sends()) != 0 {
		t.Fatal("must not reply before the work is done")
	}
	if st := statusOf(t, store, key); st.Phase != inbox.PhaseTriaged {
		t.Fatalf("phase = %q, want still Triaged", st.Phase)
	}
}

func TestTriageDropsNonConversationalEntry(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	worker := &fakeWorker{}
	tri := inbox.NewTriage(store, worker, inbox.NewSinks(), clock.NewManual(time.Unix(1, 0)))
	key := putEntry(t, store, inbox.Spec{Source: "monitor", Content: "an alert with no conversation"})

	if _, err := tri.Reconcile(ctx, key); err != nil {
		t.Fatal(err)
	}
	if worker.starts != 0 {
		t.Fatal("a dropped entry must not start work")
	}
	if st := statusOf(t, store, key); st.Phase != inbox.PhaseDropped || st.Disposition != inbox.DispositionDrop {
		t.Fatalf("status = %+v, want Dropped/Drop", st)
	}
}

func TestTriageSettledIsNoOp(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	worker := &fakeWorker{done: true, answer: "x"}
	sink := &fakeSink{name: "telegram"}
	tri := inbox.NewTriage(store, worker, inbox.NewSinks(sink), clock.NewManual(time.Unix(1, 0)))
	key := putEntry(t, store, inbox.Spec{Source: "telegram", Conversation: "c1", Content: "hi"})

	// Drive to Acted.
	worker.complete("x", false)
	_, _ = tri.Reconcile(ctx, key)
	_, _ = tri.Reconcile(ctx, key)
	before := len(sink.sends())

	// A further reconcile of a settled entry does nothing.
	if _, err := tri.Reconcile(ctx, key); err != nil {
		t.Fatal(err)
	}
	if len(sink.sends()) != before {
		t.Fatal("a settled entry must not be acted on again")
	}
}

func TestTriageFailedWorkSendsFailureReply(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	worker := &fakeWorker{}
	sink := &fakeSink{name: "telegram"}
	tri := inbox.NewTriage(store, worker, inbox.NewSinks(sink), clock.NewManual(time.Unix(1, 0)))
	key := putEntry(t, store, inbox.Spec{Source: "telegram", Conversation: "c1", Content: "hi"})

	_, _ = tri.Reconcile(ctx, key) // triage + start
	worker.complete("", true)      // work failed
	if _, err := tri.Reconcile(ctx, key); err != nil {
		t.Fatal(err)
	}
	sent := sink.sends()
	if len(sent) != 1 || sent[0][0] != "c1" || sent[0][1] == "" {
		t.Fatalf("sent = %v, want a failure reply to c1", sent)
	}
	if st := statusOf(t, store, key); st.Phase != inbox.PhaseActed {
		t.Fatalf("phase = %q, want Acted", st.Phase)
	}
}

func TestSinksSendUnknownSourceErrors(t *testing.T) {
	err := inbox.NewSinks().Send(context.Background(), "nope", "c1", "x")
	if err == nil {
		t.Fatal("Send to an unregistered source = nil, want error")
	}
}
