package session

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/spine"
)

// genEvent draws a session event with arbitrary body fields. Input is drawn from
// valid JSON values only, since an invalid raw message would not survive any JSON
// codec (the body is marshalled as part of the event).
func genEvent(rt *rapid.T) Event {
	kinds := []Kind{
		KindSessionStarted, KindTurnStarted, KindAssistant, KindToolCall,
		KindToolResult, KindTurnCompleted, KindConverged, KindStalled,
	}
	inputs := []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"a":1}`), json.RawMessage(`"s"`)}
	return Event{
		Kind:       rapid.SampledFrom(kinds).Draw(rt, "kind"),
		Turn:       rapid.IntRange(0, 50).Draw(rt, "turn"),
		Text:       rapid.String().Draw(rt, "text"),
		Tool:       rapid.String().Draw(rt, "tool"),
		ToolUseID:  rapid.String().Draw(rt, "toolUseID"),
		Input:      rapid.SampledFrom(inputs).Draw(rt, "input"),
		Result:     rapid.String().Draw(rt, "result"),
		IsError:    rapid.Bool().Draw(rt, "isError"),
		StopReason: rapid.String().Draw(rt, "stopReason"),
		Err:        rapid.String().Draw(rt, "err"),
	}
}

// Property: an event survives the spine codec unchanged in its body. Seq and Time
// are assigned by the log on append, so only the body fields must round-trip; this
// is the contract that lets any spine backend carry the session stream losslessly.
func TestProp_EventCodecRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := genEvent(rt)
		log := spine.NewMemoryLog()
		stored, err := log.Append(context.Background(), in.toAppend("s"))
		if err != nil {
			rt.Fatalf("append: %v", err)
		}
		got := fromSpine(stored)

		if got.Kind != in.Kind || got.Turn != in.Turn || got.Text != in.Text ||
			got.Tool != in.Tool || got.ToolUseID != in.ToolUseID || got.Result != in.Result ||
			got.IsError != in.IsError || got.StopReason != in.StopReason || got.Err != in.Err ||
			string(got.Input) != string(in.Input) {
			rt.Fatalf("round-trip mismatch:\n in = %+v\nout = %+v", in, got)
		}
	})
}

// Property: a subscriber at any offset receives exactly the events past that
// offset, contiguous and in Seq order, with no loss and no duplication, whether
// the events were already on the log (catch-up) or arrive after subscribing
// (tail). This is the central delivery guarantee of the stream.
func TestProp_StreamDeliversContiguousSuffix(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 25).Draw(rt, "n")
		offset := int64(rapid.IntRange(0, n).Draw(rt, "offset"))
		subscribeFirst := rapid.Bool().Draw(rt, "subscribeFirst")

		s := newStream(spine.NewMemoryLog(), bus.NewMemory(), "prop")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		appendAll := func() {
			for i := range n {
				if err := s.append(ctx, Event{Kind: KindTurnStarted, Turn: i + 1}); err != nil {
					rt.Fatalf("append: %v", err)
				}
			}
		}

		var ch <-chan Event
		var err error
		if subscribeFirst {
			if ch, err = s.Subscribe(ctx, offset); err != nil {
				rt.Fatalf("subscribe: %v", err)
			}
			appendAll()
		} else {
			appendAll()
			if ch, err = s.Subscribe(ctx, offset); err != nil {
				rt.Fatalf("subscribe: %v", err)
			}
		}

		want := n - int(offset) // events with Seq in (offset, n]
		deadline := time.After(3 * time.Second)
		prev := offset
		for got := range want {
			select {
			case ev, ok := <-ch:
				if !ok {
					rt.Fatalf("stream closed after %d/%d events", got, want)
				}
				if ev.Seq != prev+1 {
					rt.Fatalf("event %d seq = %d, want %d (contiguous from offset %d)", got, ev.Seq, prev+1, offset)
				}
				prev = ev.Seq
			case <-deadline:
				rt.Fatalf("timed out after %d/%d events (offset %d)", got, want, offset)
			}
		}
	})
}

// Property: concurrent appenders never tear the stream. With many goroutines
// appending at once, a subscriber still receives a dense, strictly increasing
// 1..N with no gap and no duplicate, because the spine assigns Seq under its lock
// and the cursor only advances. Run under -race, this guards the stream's
// concurrency contract.
func TestProp_StreamConcurrentAppendersContiguous(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		writers := rapid.IntRange(2, 6).Draw(rt, "writers")
		perWriter := rapid.IntRange(1, 8).Draw(rt, "perWriter")
		n := writers * perWriter

		s := newStream(spine.NewMemoryLog(), bus.NewMemory(), "conc")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch, err := s.Subscribe(ctx, 0)
		if err != nil {
			rt.Fatalf("subscribe: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(writers)
		for range writers {
			go func() {
				defer wg.Done()
				for range perWriter {
					_ = s.append(ctx, Event{Kind: KindTurnStarted})
				}
			}()
		}
		wg.Wait()

		seen := make(map[int64]bool, n)
		deadline := time.After(3 * time.Second)
		prev := int64(0)
		for got := range n {
			select {
			case ev := <-ch:
				if ev.Seq <= prev {
					rt.Fatalf("seq not strictly increasing: %d after %d", ev.Seq, prev)
				}
				if seen[ev.Seq] {
					rt.Fatalf("duplicate seq %d", ev.Seq)
				}
				seen[ev.Seq], prev = true, ev.Seq
			case <-deadline:
				rt.Fatalf("timed out after %d/%d events", got, n)
			}
		}
		for i := int64(1); i <= int64(n); i++ {
			if !seen[i] {
				rt.Fatalf("missing seq %d of %d", i, n)
			}
		}
	})
}
