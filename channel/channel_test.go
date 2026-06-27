package channel

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
)

// fakeChannel is an in-memory Channel: it delivers a fixed queue of inbound
// messages, then stays open until the context is cancelled (like a real adapter),
// and records everything sent back.
type fakeChannel struct {
	name  string
	queue []Inbound

	mu      sync.Mutex
	sent    []Outbound
	sendErr error
}

func newFakeChannel(name string, queue ...Inbound) *fakeChannel {
	return &fakeChannel{name: name, queue: queue}
}

func (f *fakeChannel) Name() string { return f.name }

func (f *fakeChannel) Receive(ctx context.Context) (<-chan Inbound, error) {
	out := make(chan Inbound)
	go func() {
		defer close(out)
		for _, m := range f.queue {
			select {
			case out <- m:
			case <-ctx.Done():
				return
			}
		}
		<-ctx.Done()
	}()
	return out, nil
}

func (f *fakeChannel) Send(_ context.Context, o Outbound) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, o)
	return nil
}

func (f *fakeChannel) sends() []Outbound {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Outbound(nil), f.sent...)
}

// counter tracks current and peak concurrency under the race detector.
type counter struct {
	mu       sync.Mutex
	cur, max int
}

func (c *counter) enter() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur++
	if c.cur > c.max {
		c.max = c.cur
	}
}

func (c *counter) leave() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur--
}

func (c *counter) peak() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.max
}

func TestGatewayRoutesMessageToRunnerAndReplies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var gotConvo, gotText string
		runner := RunnerFunc(func(_ context.Context, convo, text string) (string, error) {
			gotConvo, gotText = convo, text
			return "echo:" + text, nil
		})
		ch := newFakeChannel("tg", Inbound{Chat: "c1", User: "u1", Text: "hi"})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = NewGateway(runner, []Channel{ch}).Run(ctx) }()

		// Once every goroutine is blocked, the single message has been routed,
		// handled, and replied to.
		synctest.Wait()

		if gotConvo != "tg:c1" {
			t.Errorf("convo = %q, want %q", gotConvo, "tg:c1")
		}
		if gotText != "hi" {
			t.Errorf("text = %q, want %q", gotText, "hi")
		}
		sent := ch.sends()
		if len(sent) != 1 || sent[0] != (Outbound{Chat: "c1", Text: "echo:hi"}) {
			t.Fatalf("sent = %+v, want one reply {c1 echo:hi}", sent)
		}
	})
}

func TestGatewayRunnerErrorSendsFailureReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := RunnerFunc(func(context.Context, string, string) (string, error) {
			return "", errors.New("boom")
		})
		ch := newFakeChannel("tg", Inbound{Chat: "c1", Text: "hi"})

		var mu sync.Mutex
		var errs []error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			_ = NewGateway(runner, []Channel{ch}, WithErrorHandler(func(e error) {
				mu.Lock()
				errs = append(errs, e)
				mu.Unlock()
			})).Run(ctx)
		}()

		synctest.Wait()

		sent := ch.sends()
		if len(sent) != 1 || sent[0].Text != runFailureReply {
			t.Fatalf("sent = %+v, want the failure reply", sent)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(errs) != 1 {
			t.Fatalf("error handler calls = %d, want 1", len(errs))
		}
	})
}

func TestGatewayRespectsConcurrencyLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var conc counter
		runner := RunnerFunc(func(context.Context, string, string) (string, error) {
			conc.enter()
			<-release
			conc.leave()
			return "", nil
		})
		ch := newFakeChannel(
			"tg",
			Inbound{Chat: "1"}, Inbound{Chat: "2"}, Inbound{Chat: "3"},
			Inbound{Chat: "4"}, Inbound{Chat: "5"}, Inbound{Chat: "6"},
		)

		ctx, cancel := context.WithCancel(context.Background())
		go func() { _ = NewGateway(runner, []Channel{ch}, WithConcurrency(2)).Run(ctx) }()

		// With six messages queued and a limit of two, exactly two handlers run;
		// the rest are blocked acquiring the semaphore.
		synctest.Wait()
		if conc.peak() != 2 {
			t.Fatalf("peak concurrency = %d, want 2", conc.peak())
		}

		close(release) // let them all drain
		synctest.Wait()
		if conc.peak() != 2 {
			t.Fatalf("peak concurrency after drain = %d, want 2", conc.peak())
		}
		cancel()
	})
}

func TestGatewayDrainsInFlightOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		started := make(chan struct{}, 1)
		runner := RunnerFunc(func(context.Context, string, string) (string, error) {
			started <- struct{}{}
			<-release // ignore ctx on purpose: the gateway must wait for us
			return "done", nil
		})
		ch := newFakeChannel("tg", Inbound{Chat: "c1", Text: "hi"})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- NewGateway(runner, []Channel{ch}).Run(ctx) }()

		<-started
		cancel()
		synctest.Wait()

		// The inbound stream has closed, but a handler is still in flight, so Run
		// must not have returned yet.
		select {
		case <-done:
			t.Fatal("Run returned before draining the in-flight handler")
		default:
		}

		close(release)
		synctest.Wait()

		if sent := ch.sends(); len(sent) != 1 || sent[0].Text != "done" {
			t.Fatalf("sent = %+v, want the drained reply delivered", sent)
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run err = %v, want context.Canceled", err)
		}
	})
}

func TestGatewayNoChannelsErrors(t *testing.T) {
	if err := NewGateway(RunnerFunc(func(context.Context, string, string) (string, error) {
		return "", nil
	}), nil).Run(context.Background()); err == nil {
		t.Fatal("Run with no channels = nil, want error")
	}
}

func TestWithConcurrencyIgnoresNonPositive(t *testing.T) {
	g := NewGateway(nil, nil, WithConcurrency(0), WithConcurrency(-3))
	if g.concurrency != defaultConcurrency {
		t.Fatalf("concurrency = %d, want default %d", g.concurrency, defaultConcurrency)
	}
}
