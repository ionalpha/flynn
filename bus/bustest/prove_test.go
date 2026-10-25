package bustest_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/bus/bustest"
	"github.com/ionalpha/flynn/internal/conformtest"
)

// faulty wraps a correct bus and lets a case replace exactly one behaviour.
type faulty struct {
	bus.Bus
	publish   func(inner bus.Bus, ctx context.Context, m bus.Message) error
	subscribe func(inner bus.Bus, ctx context.Context, pattern string, h bus.Handler) (bus.Subscription, error)
	closeFn   func(inner bus.Bus) error
}

func (f *faulty) Publish(ctx context.Context, m bus.Message) error {
	if f.publish != nil {
		return f.publish(f.Bus, ctx, m)
	}
	return f.Bus.Publish(ctx, m)
}

func (f *faulty) Subscribe(ctx context.Context, p string, h bus.Handler) (bus.Subscription, error) {
	if f.subscribe != nil {
		return f.subscribe(f.Bus, ctx, p, h)
	}
	return f.Bus.Subscribe(ctx, p, h)
}

func (f *faulty) Close() error {
	if f.closeFn != nil {
		return f.closeFn(f.Bus)
	}
	return f.Bus.Close()
}

// inertSub is a subscription that was never registered.
type inertSub struct{ pattern string }

func (s inertSub) Subject() string    { return s.pattern }
func (s inertSub) Unsubscribe() error { return nil }

// stuckSub reports success from Unsubscribe without stopping delivery.
type stuckSub struct{ bus.Subscription }

func (stuckSub) Unsubscribe() error { return nil }

// rewritePattern builds a faulty bus that subscribes with a changed pattern.
func rewritePattern(rewrite func(string) string) func() bus.Bus {
	return func() bus.Bus {
		return &faulty{Bus: bus.NewMemory(), subscribe: func(in bus.Bus, ctx context.Context, p string, h bus.Handler) (bus.Subscription, error) {
			return in.Subscribe(ctx, rewrite(p), h)
		}}
	}
}

func TestSuiteFailsOnBrokenBus(t *testing.T) {
	good := func() bus.Bus { return bus.NewMemory() }
	// failsOn runs the suite against the bus built by mk.
	failsOn := func(name, want string, mk func() bus.Bus) conformtest.Case {
		return conformtest.Case{Name: name, Want: want, Suite: func(t *testing.T) { bustest.RunSuite(t, mk) }}
	}
	conformtest.Prove(t, []conformtest.Case{
		failsOn("drops every message", "DeliversToMatchingSubscriber", func() bus.Bus {
			return &faulty{Bus: good(), publish: func(bus.Bus, context.Context, bus.Message) error { return nil }}
		}),
		failsOn("star matches deeper subjects", "StarMatchesOneToken", rewritePattern(func(p string) string {
			return strings.TrimSuffix(p, "*") + strings.Repeat(">", strings.Count(p, "*"))
		})),
		failsOn("tail matches one token only", "TailMatchesTrailingTokens", rewritePattern(func(p string) string {
			return strings.Replace(p, ">", "*", 1)
		})),
		failsOn("delivers non-matching subjects", "NonMatchingSubjectNotDelivered", rewritePattern(func(string) string { return ">" })),
		failsOn("only first subscriber receives", "FanOutToAllSubscribers", func() bus.Bus {
			var mu sync.Mutex
			n := 0
			return &faulty{Bus: good(), subscribe: func(in bus.Bus, ctx context.Context, p string, h bus.Handler) (bus.Subscription, error) {
				mu.Lock()
				n++
				first := n == 1
				mu.Unlock()
				if !first {
					return inertSub{p}, nil
				}
				return in.Subscribe(ctx, p, h)
			}}
		}),
		failsOn("swaps adjacent messages", "OrderPreservedPerSubscription", func() bus.Bus {
			return &faulty{Bus: good(), subscribe: func(in bus.Bus, ctx context.Context, p string, h bus.Handler) (bus.Subscription, error) {
				var held *bus.Message
				return in.Subscribe(ctx, p, func(ctx context.Context, m bus.Message) error {
					if held == nil {
						held = &m
						return nil
					}
					prev := *held
					held = nil
					if err := h(ctx, m); err != nil {
						return err
					}
					return h(ctx, prev)
				})
			}}
		}),
		failsOn("delivers twice", "FanOutToAllSubscribers", func() bus.Bus {
			return &faulty{Bus: good(), subscribe: func(in bus.Bus, ctx context.Context, p string, h bus.Handler) (bus.Subscription, error) {
				return in.Subscribe(ctx, p, func(ctx context.Context, m bus.Message) error {
					_ = h(ctx, m)
					return h(ctx, m)
				})
			}}
		}),
		failsOn("unsubscribe is a no-op", "UnsubscribeStopsDelivery", func() bus.Bus {
			return &faulty{Bus: good(), subscribe: func(in bus.Bus, ctx context.Context, p string, h bus.Handler) (bus.Subscription, error) {
				s, err := in.Subscribe(ctx, p, h)
				return stuckSub{s}, err
			}}
		}),
		failsOn("swallows invalid subjects", "InvalidSubjectRejected", func() bus.Bus {
			return &faulty{Bus: good(), publish: func(in bus.Bus, ctx context.Context, m bus.Message) error {
				if err := in.Publish(ctx, m); !errors.Is(err, bus.ErrInvalidSubject) {
					return err
				}
				return nil
			}}
		}),
		failsOn("swallows invalid patterns", "InvalidPatternRejected", func() bus.Bus {
			return &faulty{Bus: good(), subscribe: func(in bus.Bus, ctx context.Context, p string, h bus.Handler) (bus.Subscription, error) {
				s, err := in.Subscribe(ctx, p, h)
				if errors.Is(err, bus.ErrInvalidPattern) {
					return inertSub{p}, nil
				}
				return s, err
			}}
		}),
		failsOn("close does not close", "ClosedBusRejects", func() bus.Bus {
			return &faulty{Bus: good(), closeFn: func(bus.Bus) error { return nil }}
		}),
		failsOn("handler error starves other subscribers", "HandlerErrorIsolated", func() bus.Bus {
			var mu sync.Mutex
			n, failed := 0, false
			firstDone := make(chan struct{})
			return &faulty{Bus: good(), subscribe: func(in bus.Bus, ctx context.Context, p string, h bus.Handler) (bus.Subscription, error) {
				mu.Lock()
				n++
				first := n == 1
				mu.Unlock()
				return in.Subscribe(ctx, p, func(ctx context.Context, m bus.Message) error {
					if first {
						defer close(firstDone)
					} else {
						<-firstDone
					}
					mu.Lock()
					skip := failed
					mu.Unlock()
					if skip {
						return nil
					}
					err := h(ctx, m)
					if err != nil {
						mu.Lock()
						failed = true
						mu.Unlock()
					}
					return err
				})
			}}
		}),
	})
}
