package reconcile

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ionalpha/flynn/diag"
	"github.com/ionalpha/flynn/fault"
)

// Result tells the controller what to do after a reconcile. The zero Result with a nil
// error settles the key until it changes or the next resync. A positive RequeueAfter
// reconciles again after that delay, independent of backoff, to poll long work.
type Result struct {
	RequeueAfter time.Duration
}

// Reconciler drives one resource toward its desired state. It gets only the key, so it
// re-reads current state each time, which makes the loop resumable after a crash.
// Reconcile must be idempotent and return promptly; an LLM step runs elsewhere and is
// observed.
type Reconciler[T comparable] interface {
	Reconcile(ctx context.Context, key T) (Result, error)
}

// ReconcilerFunc adapts a function to a Reconciler.
type ReconcilerFunc[T comparable] func(ctx context.Context, key T) (Result, error)

// Reconcile calls f.
func (f ReconcilerFunc[T]) Reconcile(ctx context.Context, key T) (Result, error) {
	return f(ctx, key)
}

// Controller runs a Reconciler over keys from a Queue and owns the retry policy: a
// Transient error backs off and retries; any other class is left for the manager's
// resync, so it never spins.
type Controller[T comparable] struct {
	name    string
	queue   *Queue[T]
	rec     Reconciler[T]
	workers int
}

// ControllerOption configures a Controller.
type ControllerOption func(*controllerConfig)

type controllerConfig struct{ workers int }

// WithWorkers sets the number of concurrent workers (default 1). The queue never hands
// one key to two workers, so more workers add only cross-key parallelism.
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

// Queue returns the controller's queue, for the manager to enqueue into.
func (c *Controller[T]) Queue() *Queue[T] { return c.queue }

// Run starts the workers and blocks until ctx is cancelled, which shuts the queue down,
// and every worker has drained.
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
			// The label says which controller a stalled worker stack belongs to.
			c.worker(diag.LabelGoroutine(ctx, "component", "reconcile", "controller", c.name))
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

// reconcileOne applies the retry policy, then marks the key done so a re-add during the
// reconcile is re-queued.
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
		// Resync re-enqueues it later.
		c.queue.Forget(key)
	}
}

// safeReconcile turns a panic into a Transient error so the worker survives and the key
// backs off.
func (c *Controller[T]) safeReconcile(ctx context.Context, key T) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fault.New(fault.Transient, "reconcile_panic", fmt.Sprintf("%s: %v", c.name, r))
		}
	}()
	return c.rec.Reconcile(ctx, key)
}
