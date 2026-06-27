package channel

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"pgregory.net/rapid"
)

// batchChannel delivers a fixed set of messages and then closes its inbound
// stream, so a Gateway run terminates on its own once every message is handled.
// It records the replies for assertions.
type batchChannel struct {
	queue []Inbound

	mu   sync.Mutex
	sent []Outbound
}

func (b *batchChannel) Name() string { return "batch" }

func (b *batchChannel) Receive(ctx context.Context) (<-chan Inbound, error) {
	out := make(chan Inbound)
	go func() {
		defer close(out)
		for _, m := range b.queue {
			select {
			case out <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (b *batchChannel) Send(_ context.Context, o Outbound) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, o)
	return nil
}

func (b *batchChannel) sends() []Outbound {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Outbound(nil), b.sent...)
}

// TestGatewayDeliversEveryMessageProperty is the rigor property: for any number of
// messages and any concurrency limit, the gateway replies to every message exactly
// once and never loses, duplicates, or mixes up a reply. The runner echoes the
// text, so a correct reply is the message's own text routed back to its chat.
func TestGatewayDeliversEveryMessageProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 30).Draw(rt, "messages")
		conc := rapid.IntRange(1, 8).Draw(rt, "concurrency")

		msgs := make([]Inbound, n)
		for i := range msgs {
			msgs[i] = Inbound{Chat: strconv.Itoa(i), Text: "t" + strconv.Itoa(i)}
		}
		ch := &batchChannel{queue: msgs}
		runner := RunnerFunc(func(_ context.Context, _, text string) (string, error) {
			return text, nil
		})

		if err := NewGateway(runner, []Channel{ch}, WithConcurrency(conc)).Run(context.Background()); err != nil {
			rt.Fatalf("run: %v", err)
		}

		got := ch.sends()
		if len(got) != n {
			rt.Fatalf("replies = %d, want %d", len(got), n)
		}
		byChat := make(map[string]string, len(got))
		for _, o := range got {
			byChat[o.Chat] = o.Text
		}
		if len(byChat) != n {
			rt.Fatalf("distinct chats replied = %d, want %d (a reply was lost or duplicated)", len(byChat), n)
		}
		for i := range msgs {
			chat := strconv.Itoa(i)
			if want := "t" + chat; byChat[chat] != want {
				rt.Fatalf("chat %s reply = %q, want %q", chat, byChat[chat], want)
			}
		}
	})
}
