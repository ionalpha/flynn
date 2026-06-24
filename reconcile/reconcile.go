package reconcile

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ionalpha/flynn/fault"
)

// Result tells the controller what to do after a reconcile. The zero Result with
// a nil error means "success, nothing more to do": the key settles and is not
// touched again until it changes or the next resync. A positive RequeueAfter asks
// to be reconciled again after that delay (the idempotent way to poll long-running
// work), independent of backoff.
type Result struct {
	RequeueAfter time.Duration
}

// Reconciler drives one resource of a kind toward its desired state. It is given
// only the key (identity), never an event or a cached object, which forces it to
// re-read the current observed state every time: the level-triggered discipline
// that makes the loop self-healing and crash-resumable. Reconcile must be
// idempotent and should return promptly; long or stochastic work (an LLM step) is
// dispatched elsewhere and observed, not run inline.
type Reconciler[T comparable] interface {
	Reconcile(ctx context.Context, key T) (Result, error)
}

// ReconcilerFunc adapts a function to a Reconciler.
type ReconcilerFunc[T comparable] func(ctx context.Context, key T) (Result, error)

// Reconcile calls f.
func (f ReconcilerFunc[T]) Reconcile(ctx context.Context, key T) (Result, error) {
	return f(ctx, key)
}

// Controller runs a Reconciler against keys delivered by a Queue. It owns the
// retry policy: a Transient error backs off and retries, a success forgets the
// key, an explicit RequeueAfter re-enqueues after a delay, and any other error
// class (Terminal, NeedsApproval, BudgetExceeded, Cancelled) is not hot-retried -
// the manager's periodic resync is the safety net that re-enqueues it later.
type Controller[T comparable] struct {
	name    string
	queue   *Queue[T]
	rec     Reconciler[T]
	workers int
}

// ControllerOption configures a Controller.
type ControllerOption func(*controllerConfig)

type controllerConfig struct{ workers int }

// WithWorkers sets the number of concurrent workers (default 1). The queue
// guarantees one key is never reconciled by two workers at once regardless, so
// more workers only add cross-key parallelism.
func WithWorkers(n int) ControllerOption {
	return func(c *controllerConfig) {
		if n > 0 {
			c.workers = n
		}
	}
}

// NewController builds a controller pulling from queue and reconciling with rec.
func NewController[T comparable](name string, queue *Queue[T], rec Reconciler[T], opts ...ControllerOption) *Controller[T] {
	cfg := controllerConfig{workers: 1}
	for _, o := range opts {
		o(&cfg)
	}
	return &Controller[T]{name: name, queue: queue, rec: rec, workers: cfg.workers}
}

// Queue exposes the controller's queue so a manager can enqueue keys into it.
func (c *Controller[T]) Queue() *Queue[T] { return c.queue }

// Run starts the workers and blocks until ctx is cancelled (which shuts the queue
// down) and every worker has drained. It is the controller's whole lifecycle.
func (c *Controller[T]) Run(ctx context.Context) {
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c.queue.ShutDown()
		case <-stop:
		}
	}()
	var wg sync.WaitGroup
	for range c.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.worker(ctx)
		}()
	}
	wg.Wait()
	close(stop)
}

func (c *Controller[T]) worker(ctx context.Context) {
	for {
		key, shutdown := c.queue.Get()
		if shutdown {
			return
		}
		c.reconcileOne(ctx, key)
	}
}

// reconcileOne reconciles one key and applies the retry policy, then marks the
// key done so a re-add that arrived mid-reconcile is re-queued.
func (c *Controller[T]) reconcileOne(ctx context.Context, key T) {
	defer c.queue.Done(key)
	res, err := c.safeReconcile(ctx, key)
	switch {
	case err == nil && res.RequeueAfter > 0:
		c.queue.Forget(key)
		c.queue.AddAfter(key, res.RequeueAfter)
	case err == nil:
		c.queue.Forget(key) // settled until the next change or resync
	case fault.Classify(err) == fault.Transient:
		c.queue.AddRateLimited(key) // back off and retry
	default:
		// Terminal / NeedsApproval / BudgetExceeded / Cancelled: do not spin.
		// Resync re-enqueues later, so nothing is abandoned permanently.
		c.queue.Forget(key)
	}
}

// safeReconcile runs the reconciler, converting a panic into a Transient error so
// a buggy reconcile backs off and retries instead of killing the worker.
func (c *Controller[T]) safeReconcile(ctx context.Context, key T) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fault.New(fault.Transient, "reconcile_panic", fmt.Sprintf("%s: %v", c.name, r))
		}
	}()
	return c.rec.Reconcile(ctx, key)
}
