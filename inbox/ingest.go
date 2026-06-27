package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/resource"
)

// Enqueuer hands a newly recorded entry's key to the triage controller's work
// queue so it is reconciled promptly. reconcile.Manager.Enqueue satisfies it; the
// resync sweep is the safety net if a hint is ever missed.
type Enqueuer interface {
	Enqueue(resource.Key)
}

// Ingest records inbound entries from a set of Sources as Entry resources and
// enqueues each for triage. It is the inbound half of the boundary: it decides
// only that an entry is durably recorded, never what to do with it. That decision
// is the triage controller's, which keeps every disposition behind one governed
// waist rather than scattered across adapters.
type Ingest struct {
	store   resource.Store
	queue   Enqueuer
	sources []Source
	clk     clock.Timing
	onError func(error)
}

// IngestOption configures an Ingest.
type IngestOption func(*Ingest)

// WithIngestErrorHandler registers a callback for non-fatal errors (a source that
// will not start, a record that fails to persist). The default discards them. It
// may be called from several goroutines and must be safe for that.
func WithIngestErrorHandler(fn func(error)) IngestOption {
	return func(in *Ingest) {
		if fn != nil {
			in.onError = fn
		}
	}
}

// NewIngest builds an ingester that records entries from sources onto store and
// enqueues each for triage on queue. The clock stamps the receipt time when a
// source leaves it unset.
func NewIngest(store resource.Store, queue Enqueuer, clk clock.Timing, sources []Source, opts ...IngestOption) *Ingest {
	in := &Ingest{
		store:   store,
		queue:   queue,
		sources: sources,
		clk:     clk,
		onError: func(error) {},
	}
	for _, o := range opts {
		o(in)
	}
	return in
}

// Run starts every source and records entries until ctx is cancelled, then waits
// for the source readers to stop. A source that fails to start is reported and
// skipped; if none start, Run returns an error rather than blocking forever.
func (in *Ingest) Run(ctx context.Context) error {
	if len(in.sources) == 0 {
		return errors.New("inbox: ingest has no sources")
	}

	var wg sync.WaitGroup
	started := 0
	for _, src := range in.sources {
		ch, err := src.Receive(ctx)
		if err != nil {
			in.onError(fmt.Errorf("inbox: source %s: receive: %w", src.Name(), err))
			continue
		}
		started++
		wg.Add(1)
		go func(src Source, ch <-chan Spec) {
			defer wg.Done()
			for spec := range ch {
				if _, err := in.record(ctx, src.Name(), spec); err != nil {
					in.onError(fmt.Errorf("inbox: source %s: record: %w", src.Name(), err))
				}
			}
		}(src, ch)
	}

	if started == 0 {
		return errors.New("inbox: no sources could start")
	}
	wg.Wait()
	return ctx.Err()
}

// record stamps the source and (when unset) the receipt time, writes the entry as
// a resource, and enqueues its key for triage. The source is stamped here, not
// trusted from the adapter, so the reply-routing key is always authoritative.
func (in *Ingest) record(ctx context.Context, source string, spec Spec) (resource.Key, error) {
	spec.Source = source
	if spec.ReceivedAt.IsZero() {
		spec.ReceivedAt = in.clk.Now().UTC()
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return resource.Key{}, err
	}
	saved, err := in.store.Put(ctx, resource.Resource{
		APIVersion:   GroupVersion,
		Kind:         Kind,
		GenerateName: "entry-",
		Spec:         raw,
	})
	if err != nil {
		return resource.Key{}, err
	}
	key := saved.Key()
	in.queue.Enqueue(key)
	return key, nil
}
