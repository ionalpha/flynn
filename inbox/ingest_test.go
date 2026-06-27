package inbox_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/resource"
)

// batchSource emits a fixed set of specs and then closes its channel, so an Ingest
// run terminates on its own once every entry is recorded.
type batchSource struct {
	name  string
	specs []inbox.Spec
}

func (b *batchSource) Name() string { return b.name }

func (b *batchSource) Receive(ctx context.Context) (<-chan inbox.Spec, error) {
	out := make(chan inbox.Spec)
	go func() {
		defer close(out)
		for _, s := range b.specs {
			select {
			case out <- s:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// recordingQueue captures the keys Ingest enqueues.
type recordingQueue struct {
	mu   sync.Mutex
	keys []resource.Key
}

func (q *recordingQueue) Enqueue(k resource.Key) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.keys = append(q.keys, k)
}

func (q *recordingQueue) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.keys)
}

func TestIngestRecordsEntriesStampedWithSourceAndTime(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	q := &recordingQueue{}
	at := time.Unix(1_700_000_000, 0).UTC()
	clk := clock.NewManual(at)

	src := &batchSource{name: "telegram", specs: []inbox.Spec{
		{Conversation: "c1", Content: "hi"},
		{Conversation: "c2", Content: "yo"},
	}}
	if err := inbox.NewIngest(store, q, clk, []inbox.Source{src}).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, err := store.List(ctx, inbox.Kind, resource.Scope{}, resource.Selector{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("entries stored = %d, want 2", len(got))
	}
	if q.count() != 2 {
		t.Fatalf("enqueued = %d, want 2", q.count())
	}
	for _, r := range got {
		s, err := inbox.DecodeSpec(r)
		if err != nil {
			t.Fatal(err)
		}
		if s.Source != "telegram" {
			t.Errorf("source = %q, want telegram (stamped by ingest)", s.Source)
		}
		if !s.ReceivedAt.Equal(at) {
			t.Errorf("receivedAt = %v, want %v (stamped from clock)", s.ReceivedAt, at)
		}
	}
}

func TestIngestNoSourcesErrors(t *testing.T) {
	if err := inbox.NewIngest(newStore(t), &recordingQueue{}, clock.NewManual(time.Unix(0, 0)), nil).Run(context.Background()); err == nil {
		t.Fatal("Run with no sources = nil, want error")
	}
}

// TestIngestRecordsEveryEntryProperty is a property: across any set of sources
// each emitting any number of entries, every entry is recorded exactly once and
// stamped with the source it arrived on, so no inbound item is lost or misrouted.
func TestIngestRecordsEveryEntryProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		store := newStore(t)
		q := &recordingQueue{}

		nSources := rapid.IntRange(1, 4).Draw(rt, "sources")
		sources := make([]inbox.Source, nSources)
		wantBySource := map[string]int{}
		total := 0
		for i := range sources {
			name := "src" + strconv.Itoa(i)
			n := rapid.IntRange(0, 8).Draw(rt, "msgs"+strconv.Itoa(i))
			specs := make([]inbox.Spec, n)
			for j := range specs {
				specs[j] = inbox.Spec{Conversation: strconv.Itoa(j), Content: "c"}
			}
			sources[i] = &batchSource{name: name, specs: specs}
			wantBySource[name] = n
			total += n
		}

		if err := inbox.NewIngest(store, q, clock.NewManual(time.Unix(1, 0)), sources).Run(ctx); err != nil {
			rt.Fatalf("run: %v", err)
		}

		got, err := store.List(ctx, inbox.Kind, resource.Scope{}, resource.Selector{})
		if err != nil {
			rt.Fatal(err)
		}
		if len(got) != total {
			rt.Fatalf("stored = %d, want %d", len(got), total)
		}
		if q.count() != total {
			rt.Fatalf("enqueued = %d, want %d", q.count(), total)
		}
		gotBySource := map[string]int{}
		for _, r := range got {
			s, err := inbox.DecodeSpec(r)
			if err != nil {
				rt.Fatal(err)
			}
			gotBySource[s.Source]++
		}
		for name, want := range wantBySource {
			if gotBySource[name] != want {
				rt.Fatalf("source %s recorded %d, want %d", name, gotBySource[name], want)
			}
		}
	})
}
