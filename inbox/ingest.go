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

// Enqueuer hands a recorded entry to triage promptly. reconcile.Manager satisfies
// it; resync picks up a missed hint.
type Enqueuer interface {
	Enqueue(resource.Key)
}

// Ingest records entries from each [Source] and enqueues them for triage. It never
// decides what to do with an entry; [Triage] does.
type Ingest struct {
	store   resource.Store
	queue   Enqueuer
	sources []Source
	clk     clock.Timing
	onError func(error)
}

// IngestOption configures an Ingest.
type IngestOption func(*Ingest)

// WithIngestErrorHandler receives non-fatal errors: a source that will not start,
// a record that fails to persist. The default discards them. fn is called from
// several goroutines.
func WithIngestErrorHandler(fn func(error)) IngestOption {
	return func(in *Ingest) {
		if fn != nil {
			in.onError = fn
		}
	}
}

// NewIngest returns an Ingest. clk stamps the receipt time a source leaves unset.
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

// Run records entries until ctx is cancelled and the sources stop. A source that
// fails to start is reported and skipped; Run errors when none start.
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

// record overwrites spec.Source with the source's name, so an adapter cannot route
// replies to another sink.
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
